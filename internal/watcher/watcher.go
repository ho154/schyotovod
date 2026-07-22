package watcher

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"

	"schyotovod/internal/attempts"
	"schyotovod/internal/config"
	"schyotovod/internal/dedup"
	"schyotovod/internal/gmail"
	"schyotovod/internal/invoice"
	"schyotovod/internal/journal"
	"schyotovod/internal/logger"
	"schyotovod/internal/pyrus"
	"schyotovod/internal/trace"
)

// JournalRecorder — интерфейс журнала событий, используемый наблюдателем.
// Реализуется *journal.Manager. Может быть nil — тогда журналирование
// отключено (все вызовы через методы-обёртки становятся no-op).
type JournalRecorder interface {
	Upsert(ev journal.Event)
	Get(id string) (journal.Event, bool)
	SaveTrace(eventID string, steps []trace.Step) error
}

// CheckResult — результат обработки одного письма (для сводки ручной проверки).
type CheckResult struct {
	MessageID   string
	From        string
	Subject     string
	MsgDate     time.Time
	ClientName  string
	LicenseNo   string
	LicenseDate time.Time
	InvoiceNo   string
	InvoiceDate time.Time
	Amount      float64
	Filename    string
	TaskID      int
	Success     bool
	Skipped     bool
	Deferred    bool
	ErrText     string
	ErrStage    journal.Stage
}

// CheckSummary — сводка одного запуска проверки почты (найдено/успех/ошибки/…).
type CheckSummary struct {
	Found     int
	Processed int
	Failed    int
	Deferred  int
	Skipped   int
	Results   []CheckResult
}

// idleTimeout — как часто переустанавливать IMAP IDLE (Gmail рвёт ~29 мин).
const idleTimeout = 25 * time.Minute

type AttemptsManager interface {
	GetState(client string) attempts.AttemptState
	RecordAttempt(client string, success bool, maxAttempts int, retryIntervalMinutes int) attempts.AttemptState
	RecordAttemptWithMsg(client string, success bool, maxAttempts int, retryIntervalMinutes int, messageID string) attempts.AttemptState
	ResetAttempt(client string)
}

// Watcher наблюдает за почтой и обрабатывает письма со счетами.
type Watcher struct {
	cfgMgr   *config.Manager
	dedup    *dedup.Dedup
	attempts AttemptsManager
	journal  JournalRecorder // может быть nil
	log      *logger.Logger

	mu       sync.Mutex
	notify   chan struct{} // сигнал «есть новое письмо» (от IDLE-обработчика)
	stopOnce sync.Once
	stop     chan struct{}
}

// New создаёт наблюдатель. jr — журнал событий (может быть nil).
func New(cfgMgr *config.Manager, dd *dedup.Dedup, att AttemptsManager, jr JournalRecorder, log *logger.Logger) *Watcher {
	return &Watcher{
		cfgMgr:   cfgMgr,
		dedup:    dd,
		attempts: att,
		journal:  jr,
		log:      log,
		notify:   make(chan struct{}, 1),
		stop:     make(chan struct{}),
	}
}

// recordJournal — безопасная обёртка над журналом (no-op если journal == nil).
func (w *Watcher) recordJournal(ev journal.Event) {
	if w.journal == nil {
		return
	}
	w.journal.Upsert(ev)
}

// journalGet — безопасная обёртка чтения записи журнала (nil-safe).
func (w *Watcher) journalGet(id string) (journal.Event, bool) {
	if w.journal == nil {
		return journal.Event{}, false
	}
	return w.journal.Get(id)
}

// Stop останавливает наблюдатель.
func (w *Watcher) Stop() {
	w.stopOnce.Do(func() { close(w.stop) })
}

// Run запускает основной цикл наблюдения. Блокирует до вызова Stop.
func (w *Watcher) Run() {
	w.log.Info("Служба наблюдения за почтой запущена")
	for {
		select {
		case <-w.stop:
			w.log.Info("Служба наблюдения за почтой остановлена")
			return
		default:
		}

		cfg := w.cfgMgr.Get()
		if !cfg.IsReady() {
			// Настройки не заполнены — ждём и проверяем снова.
			w.sleep(30 * time.Second)
			continue
		}

		loc, err := cfg.Location()
		if err != nil {
			w.log.Warn("Проблема с часовым поясом: %v. Используется UTC", err)
		}
		now := time.Now().In(loc)

		if !InPeriod(now, cfg.Filter.StartDay, cfg.Filter.EndDay) {
			// Вне активного периода — не держим соединение, ждём до следующей проверки.
			w.sleep(untilNextPeriodCheck(now, cfg.Filter.StartDay))
			continue
		}

		// Внутри периода — работаем через IMAP IDLE с резервным поллингом.
		if err := w.watchDuringPeriod(cfg); err != nil {
			w.log.Error("Ошибка при наблюдении за почтой: %v. Повтор через 1 минуту", err)
			w.sleep(time.Minute)
		}
	}
}

// watchDuringPeriod держит соединение и IDLE, пока идёт активный период.
// Возвращает управление при выходе из периода, ошибке или остановке.
func (w *Watcher) watchDuringPeriod(cfg config.Config) error {
	dial := gmail.DialConfig{
		Host:        cfg.Gmail.IMAPHost,
		Port:        cfg.Gmail.IMAPPort,
		Email:       cfg.Gmail.Email,
		AppPassword: cfg.Gmail.AppPassword,
	}

	client, err := gmail.Connect(dial, w.signalNewMail)
	if err != nil {
		return err
	}
	defer client.Logout()
	defer client.Close()

	if err := gmail.SelectInbox(context.Background(), client); err != nil {
		return err
	}

	w.log.Info("Подключение к почте установлено, ожидание писем со счетами (период %d–%d числа)",
		cfg.Filter.StartDay, cfg.Filter.EndDay)

	// Сразу проверяем письма, пришедшие до старта наблюдения.
	if _, err := w.processNewMessages(client, cfg); err != nil {
		w.log.Error("Ошибка первичной проверки писем: %v", err)
	}

	fallbackInterval := time.Duration(cfg.Filter.FallbackPollIntervalMinutes) * time.Minute
	if fallbackInterval <= 0 {
		fallbackInterval = 30 * time.Minute
	}
	fallbackTicker := time.NewTicker(fallbackInterval)
	defer fallbackTicker.Stop()

	for {
		// Проверяем, не вышли ли из периода.
		loc, _ := cfg.Location()
		if !InPeriod(time.Now().In(loc), cfg.Filter.StartDay, cfg.Filter.EndDay) {
			w.log.Info("Активный период завершён, наблюдение приостановлено до следующего месяца")
			return nil
		}

		// Запускаем IDLE.
		idleCmd, err := client.Idle()
		if err != nil {
			return fmt.Errorf("не удалось войти в режим ожидания IMAP IDLE: %w", err)
		}

		select {
		case <-w.stop:
			_ = idleCmd.Close()
			return nil

		case <-w.notify:
			// Пришло уведомление о новом письме.
			_ = idleCmd.Close()
			if err := idleCmd.Wait(); err != nil {
				w.log.Warn("Ожидание IDLE завершилось с ошибкой: %v", err)
			}
			if _, err := w.processNewMessages(client, cfg); err != nil {
				w.log.Error("Ошибка обработки писем: %v", err)
			}

		case <-fallbackTicker.C:
			// Резервная проверка.
			_ = idleCmd.Close()
			if err := idleCmd.Wait(); err != nil {
				w.log.Warn("Ожидание IDLE завершилось с ошибкой: %v", err)
			}
			w.log.Info("Резервная проверка почты")
			if _, err := w.processNewMessages(client, cfg); err != nil {
				w.log.Error("Ошибка резервной проверки писем: %v", err)
			}

		case <-time.After(idleTimeout):
			// Переустанавливаем IDLE, чтобы Gmail не разорвал соединение.
			_ = idleCmd.Close()
			if err := idleCmd.Wait(); err != nil {
				// Соединение могло разорваться — выходим, внешний цикл переподключится.
				return fmt.Errorf("соединение IMAP разорвано: %w", err)
			}
		}
	}
}

// processNewMessages ищет письма за период, отбирает необработанные и
// обрабатывает их. Возвращает сводку по каждому письму (для журнала и итогов
// ручной проверки).
func (w *Watcher) processNewMessages(client *imapclient.Client, cfg config.Config) (CheckSummary, error) {
	var summary CheckSummary
	loc, _ := cfg.Location()
	now := time.Now().In(loc)
	since, before := PeriodBounds(now, cfg.Filter.StartDay, cfg.Filter.EndDay)

	uids, err := gmail.Search(context.Background(), client, gmail.SearchCriteria{
		SenderEmail: cfg.Filter.SenderEmail,
		Since:       since,
		Before:      before,
	})
	if err != nil {
		return summary, err
	}

	for _, uid := range uids {
		select {
		case <-w.stop:
			return summary, nil
		default:
		}

		msg, err := gmail.FetchMessage(context.Background(), client, uid)
		if err != nil {
			w.log.Error("ОШИБКА ПОЧТЫ: Не удалось получить письмо (UID %v): %v", uid, err)
			continue
		}

		if msg.MessageID == "" {
			w.log.Warn("ПРЕДУПРЕЖДЕНИЕ ПОЧТЫ: У письма отсутствует Message-ID, пропуск (тема: %q)", msg.Subject)
			continue
		}

		// Дополнительная проверка периода по дате письма (точнее, чем IMAP SEARCH).
		if !msg.Date.IsZero() && !InPeriodByDate(msg.Date, now, cfg.Filter.StartDay, cfg.Filter.EndDay) {
			continue
		}

		if w.dedup.IsProcessed(msg.MessageID) {
			continue
		}

		summary.Found++

		if len(msg.Attachments) == 0 {
			w.log.Warn("ПРЕДУПРЕЖДЕНИЕ ПОЧТЫ: Письмо от %s без вложений, пропуск (тема: %q)", msg.From, msg.Subject)
			w.recordJournal(journal.Event{
				ID: msg.MessageID, MessageID: msg.MessageID, From: msg.From,
				Subject: msg.Subject, MsgDate: msg.Date,
				CurrentStage: journal.StageReceived, OverallStatus: journal.StatusSkipped,
				ErrorMessage: "письмо без вложений",
			})
			summary.Skipped++
			summary.Results = append(summary.Results, CheckResult{
				MessageID: msg.MessageID, From: msg.From, Subject: msg.Subject,
				MsgDate: msg.Date, Skipped: true, ErrText: "письмо без вложений",
			})
			// Отмечаем обработанным, чтобы не проверять его повторно каждый раз.
			_ = w.dedup.MarkProcessed(msg.MessageID)
			continue
		}

		res := w.processOneMessage(cfg, msg)
		summary.Results = append(summary.Results, res)

		switch {
		case res.Success:
			summary.Processed++
			if err := w.dedup.MarkProcessed(msg.MessageID); err != nil {
				w.log.Warn("Счёт прикреплён, но не удалось сохранить отметку об обработке: %v", err)
			}
		case res.Deferred:
			summary.Deferred++
			// Не отмечаем обработанным — повторим при следующей проверке.
		default:
			summary.Failed++
			// Не отмечаем обработанным — повторим при следующей проверке.
		}
	}
	return summary, nil
}

// processOneMessage выполняет полный цикл обработки одного письма с ведением
// журнала и трассировки, возвращает результат для сводки.
func (w *Watcher) processOneMessage(cfg config.Config, msg *gmail.Message) CheckResult {
	res := CheckResult{
		MessageID: msg.MessageID, From: msg.From, Subject: msg.Subject, MsgDate: msg.Date,
	}

	// Заводим коллектор трассировки и контекст с привязкой к письму.
	col := trace.NewCollector()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ctx = trace.WithCollector(ctx, col)
	ctx = logger.WithMessageID(ctx, msg.MessageID)

	// Базовая запись журнала: письмо получено.
	ev := journal.Event{
		ID: msg.MessageID, MessageID: msg.MessageID, From: msg.From,
		Subject: msg.Subject, MsgDate: msg.Date,
		CurrentStage: journal.StageReceived, OverallStatus: journal.StatusPending,
	}
	if len(msg.Attachments) > 0 {
		ev.Filename = msg.Attachments[0].Filename
	}
	w.recordJournal(ev)

	// Выполняем обработку.
	out := w.attachToPyrus(ctx, cfg, msg)

	// Переносим данные из результата обработки в журнал и результат сводки.
	if out.Info != nil {
		ev.ClientName = out.Info.ClientName
		ev.LicenseNo = out.Info.LicenseNo
		ev.LicenseDate = out.Info.LicenseDate
		ev.Amount = out.Info.Amount
		res.ClientName = out.Info.ClientName
		res.LicenseNo = out.Info.LicenseNo
		res.LicenseDate = out.Info.LicenseDate
		res.Amount = out.Info.Amount
	}
	ev.InvoiceNo = out.InvoiceNo
	ev.InvoiceDate = out.InvoiceDate
	res.InvoiceNo = out.InvoiceNo
	res.InvoiceDate = out.InvoiceDate
	res.Filename = out.Filename
	if out.Filename != "" {
		ev.Filename = out.Filename
	}
	ev.TaskID = out.TaskID
	ev.PyrusTaskStatus = out.PyrusStatus
	ev.CurrentStage = out.Stage
	res.TaskID = out.TaskID

	steps := col.Steps()
	// Не перезаписываем ранее сохранённый непустой трейс пустым: при отложенной
	// (retry) повторной проверке обработка завершается ещё до обращений к Pyrus/
	// почте, поэтому шагов нет — но полезный трейс предыдущей попытки нужно
	// сохранить для анализа.
	if w.journal != nil && len(steps) > 0 {
		if err := w.journal.SaveTrace(msg.MessageID, steps); err != nil {
			w.log.Warn("Не удалось сохранить трейс запросов для письма: %v", err)
		}
	}
	if len(steps) > 0 {
		ev.StepsCount = len(steps)
	} else if prev, ok := w.journalGet(msg.MessageID); ok {
		// Сохраняем счётчик шагов из предыдущей записи, если сейчас шагов нет.
		ev.StepsCount = prev.StepsCount
	}

	switch {
	case out.Err != nil:
		res.ErrText = out.Err.Error()
		res.ErrStage = out.Stage
		ev.OverallStatus = journal.StatusFailed
		ev.ErrorStage = out.Stage
		ev.ErrorMessage = out.Err.Error()
		w.logStageError(ctx, cfg, msg, out)
	case out.Deferred:
		res.Deferred = true
		ev.OverallStatus = journal.StatusPending
		w.logDeferred(ctx, cfg, msg, out)
	default:
		res.Success = true
		ev.OverallStatus = journal.StatusOK
		ev.CurrentStage = journal.StageDone
		taskLink := journal.PyrusTaskURL(out.TaskID)
		w.log.InfoCtx(ctx, "Счёт успешно прикреплён в Pyrus. Клиент: «%s». Задача: %s («%s»). Счёт №%s%s на сумму %.2f. Лицензия №%s%s. Письмо получено: %s от %s. Файл: %s",
			out.Info.ClientName, taskLink, out.Info.ClientName,
			out.InvoiceNo, dateSuffix(out.InvoiceDate), out.Info.Amount,
			out.Info.LicenseNo, dateSuffix(out.Info.LicenseDate),
			formatMsgDate(msg.Date), msg.From, out.Filename)
	}

	w.recordJournal(ev)
	return res
}

// logStageError пишет в лог понятную ошибку обработки с указанием этапа и всех
// известных атрибутов письма/счёта.
func (w *Watcher) logStageError(ctx context.Context, cfg config.Config, msg *gmail.Message, out attachOutcome) {
	client := "—"
	license := "—"
	amount := ""
	if out.Info != nil {
		if out.Info.ClientName != "" {
			client = out.Info.ClientName
		}
		if out.Info.LicenseNo != "" {
			license = out.Info.LicenseNo + dateSuffix(out.Info.LicenseDate)
		}
		if out.Info.Amount > 0 {
			amount = fmt.Sprintf(" на сумму %.2f", out.Info.Amount)
		}
	}
	invoice := "—"
	if out.InvoiceNo != "" {
		invoice = out.InvoiceNo + dateSuffix(out.InvoiceDate)
	}
	w.log.ErrorCtx(ctx, "ОШИБКА ОБРАБОТКИ (этап: %s): письмо от %s (получено %s). Клиент: %s. Счёт №%s. Лицензия №%s%s. Файл: %s. Причина: %v",
		out.Stage, msg.From, formatMsgDate(msg.Date), client, invoice, license, amount, out.Filename, out.Err)
}

// logDeferred пишет в лог сообщение об отсрочке загрузки в Pyrus.
func (w *Watcher) logDeferred(ctx context.Context, cfg config.Config, msg *gmail.Message, out attachOutcome) {
	client := ""
	if out.Info != nil {
		client = out.Info.ClientName
	}
	taskLink := ""
	if out.TaskID != 0 {
		taskLink = fmt.Sprintf(" Задача: %s («%s»).", journal.PyrusTaskURL(out.TaskID), client)
	}
	next := ""
	if !out.NextAttempt.IsZero() {
		next = " Следующая попытка после " + out.NextAttempt.Format("15:04:05") + "."
	}
	w.log.InfoCtx(ctx, "Отложена загрузка в Pyrus для клиента «%s» (попытка %d из %d).%s%s Письмо получено: %s.",
		client, out.AttemptCount, cfg.Pyrus.MaxUpdateAttempts, taskLink, next, formatMsgDate(msg.Date))
}

// checkAndProcessTaskAttempts проверяет, пришло ли время делать следующую
// попытку. Возвращает (ok, ignored) — ok=false и ignored означает исчерпание
// лимита; ok=false без ignored означает отсрочку до NextAttempt.
func (w *Watcher) checkAndProcessTaskAttempts(clientName string) (ok bool, state attempts.AttemptState) {
	state = w.attempts.GetState(clientName)
	if state.Ignored {
		return false, state
	}
	if !state.NextAttempt.IsZero() && time.Now().Before(state.NextAttempt) {
		return false, state
	}
	return true, state
}

// processTaskUpdate с логикой ретраев и комментариев в задачу Pyrus.
// msgFilenames возвращает имена файлов вложений.
func msgFilenames(msg *gmail.Message) []string {
	names := make([]string, 0, len(msg.Attachments))
	for _, a := range msg.Attachments {
		names = append(names, a.Filename)
	}
	return names
}

// taskUpdateResult — результат поиска и обновления задачи в Pyrus.
type taskUpdateResult struct {
	TaskID      int
	PyrusStatus string // бизнес-статус задачи (поле id:29), для журнала
}

func (w *Watcher) processTaskUpdate(ctx context.Context, pClient *pyrus.Client, cfg config.Config, clientName string, guids []string, filenames []string, info *invoice.InvoiceInfo) (taskUpdateResult, journal.Stage, error) {
	var out taskUpdateResult
	loc, _ := cfg.Location()
	now := time.Now().In(loc)
	since, _ := PeriodBounds(now, cfg.Filter.StartDay, cfg.Filter.EndDay)

	// Ищем задачу, фильтруя на уровне API Pyrus по дате начала периода
	tasks, err := pClient.FindTasksByForm(ctx, cfg.Pyrus.FormID, since)
	if err != nil {
		return out, journal.StageFindTask, fmt.Errorf("ошибка поиска задач по форме: %w", err)
	}

	var targetTaskID int
	var foundCount int

	normClientName := invoice.NormalizeString(clientName)

	for _, t := range tasks {
		// Задача должна быть открытой (нет даты закрытия)
		if t.CloseDate != nil {
			continue
		}
		// Создана в текущем месяце и не раньше дня начала периода
		if t.CreateDate.Before(since) {
			continue
		}

		// Ищем поле наименования клиента по числовому Field ID
		for _, f := range t.Fields {
			if f.ID == cfg.Pyrus.ClientNameFieldID {
				if valStr, ok := f.Value.(string); ok {
					if strings.EqualFold(invoice.NormalizeString(valStr), normClientName) {
						targetTaskID = t.ID
						foundCount++
					}
				}
			}
		}
	}

	if foundCount == 0 {
		return out, journal.StageFindTask, fmt.Errorf("задача для клиента %q не найдена", clientName)
	}
	if foundCount > 1 {
		return out, journal.StageFindTask, fmt.Errorf("найдено более одной открытой задачи (%d) для клиента %q", foundCount, clientName)
	}
	out.TaskID = targetTaskID
	// Запоминаем задачу для сообщений об отсрочке.
	if setter, ok := w.attempts.(interface{ SetLastTaskID(string, int) }); ok {
		setter.SetLastTaskID(clientName, targetTaskID)
	}

	// Получаем детальную информацию о задаче, чтобы проверить, есть ли уже прикрепленный файл
	taskDetails, err := pClient.GetTask(ctx, targetTaskID)
	if err != nil {
		return out, journal.StageFindTask, fmt.Errorf("не удалось получить детали задачи %d: %w", targetTaskID, err)
	}

	// Читаем бизнес-статус задачи (поле id:29 "Статус") для журнала.
	out.PyrusStatus = readPyrusTaskStatus(taskDetails)

	existingCount := 0
	if f, found := taskDetails.FindField(cfg.Pyrus.AttachmentFieldID); found {
		if f.Value != nil {
			if valSlice, ok := f.Value.([]any); ok {
				existingCount = len(valSlice)
			} else if valStr, ok := f.Value.(string); ok && valStr != "" {
				existingCount = 1
			}
		}
	}

	// Формируем комментарий
	state := w.attempts.GetState(clientName)
	currentAttempt := state.Count + 1
	leftAttempts := cfg.Pyrus.MaxUpdateAttempts - currentAttempt

	var comment string
	actionText := "Добавлен счет"
	if existingCount > 0 {
		actionText = fmt.Sprintf("В задаче уже файлов: %d. Новый прикрепленный файл: %s", existingCount, strings.Join(filenames, ", "))
	} else {
		actionText = fmt.Sprintf("Прикреплен файл: %s", strings.Join(filenames, ", "))
	}

	if leftAttempts > 0 {
		comment = fmt.Sprintf("Выполнено: %s. Сумма обновлена. (Попытка %d из %d). Следующая попытка при ошибке через %d мин.",
			actionText, currentAttempt, cfg.Pyrus.MaxUpdateAttempts, cfg.Pyrus.RetryIntervalMinutes)
	} else {
		comment = fmt.Sprintf("Выполнено: %s. Сумма обновлена. (Попытка %d из %d). Это последняя попытка.",
			actionText, currentAttempt, cfg.Pyrus.MaxUpdateAttempts)
	}

	// Выполняем обновление задачи
	err = pClient.UpdateTaskInvoice(ctx, targetTaskID, cfg.Pyrus.AttachmentFieldID, guids, cfg.Pyrus.AmountFieldID, info.Amount, comment)
	if err != nil {
		// Пишем в чат задачи причину ошибки (если задача все же найдена)
		errMsg := fmt.Sprintf("Ошибка обновления задачи (попытка %d из %d): %v", currentAttempt, cfg.Pyrus.MaxUpdateAttempts, err)
		if leftAttempts > 0 {
			errMsg += fmt.Sprintf(" Повторим через %d мин.", cfg.Pyrus.RetryIntervalMinutes)
		}
		if commentErr := pClient.AddComment(ctx, targetTaskID, errMsg); commentErr != nil {
			w.log.Error("ОШИБКА PYRUS: Не удалось отправить комментарий с ошибкой в задачу %d: %v", targetTaskID, commentErr)
		}
		return out, journal.StageUpdateTask, err
	}

	return out, journal.StageDone, nil
}

// pyrusStatusFieldID — ID поля «Статус» в форме (id:29), значения которого
// (multiple_choice) отражают бизнес-статус задачи в Pyrus.
const pyrusStatusFieldID = 29

// readPyrusTaskStatus извлекает человекочитаемый бизнес-статус задачи из поля
// id:29. Возвращает пустую строку, если поле не найдено или пустое.
func readPyrusTaskStatus(td *pyrus.TaskDetails) string {
	f, found := td.FindField(pyrusStatusFieldID)
	if !found || f.Value == nil {
		return ""
	}
	switch v := f.Value.(type) {
	case string:
		return v
	case map[string]any:
		if cv, ok := v["choice_value"].(string); ok {
			return cv
		}
	}
	return ""
}

// ResetAttempts сбрасывает все попытки клиентов.
func (w *Watcher) ResetAttempts() {
	type resets interface {
		ResetAll()
	}
	if r, ok := w.attempts.(resets); ok {
		r.ResetAll()
	}
}

// attachOutcome — результат полной обработки одного письма (для журнала/сводки).
type attachOutcome struct {
	Info         *invoice.InvoiceInfo // данные из тела письма (может быть nil при ошибке парсинга)
	InvoiceNo    string               // номер счёта из имени файла
	InvoiceDate  time.Time            // дата счёта из имени файла
	Filename     string               // имя файла-счёта
	TaskID       int
	PyrusStatus  string
	Stage        journal.Stage // этап, на котором завершилась обработка (или произошла ошибка)
	Deferred     bool          // обработка отложена (retry) или лимит исчерпан
	AttemptCount int
	NextAttempt  time.Time
	Err          error
}

// attachToPyrus загружает вложения письма в Pyrus и прикрепляет их в найденную
// задачу. Ведёт этапы пайплайна и возвращает подробный attachOutcome.
func (w *Watcher) attachToPyrus(ctx context.Context, cfg config.Config, msg *gmail.Message) attachOutcome {
	out := attachOutcome{Stage: journal.StageParseBody}
	col := trace.FromContext(ctx)

	// Этап: разбор тела письма.
	col.SetStage(string(journal.StageParseBody))
	info, err := invoice.ParseMessageBody(msg.Body)
	if err != nil {
		out.Err = fmt.Errorf("не удалось разобрать письмо: %w", err)
		return out
	}
	out.Info = info

	// Этап: разбор имени файла счёта (номер и дата счёта).
	out.Stage = journal.StageParseFile
	col.SetStage(string(journal.StageParseFile))
	invoiceFile, invNo, invDate := findInvoiceAttachment(msg)
	out.Filename = invoiceFile
	out.InvoiceNo = invNo
	out.InvoiceDate = invDate

	// Проверяем попытки перед подключением к Pyrus.
	ok, state := w.checkAndProcessTaskAttempts(info.ClientName)
	if !ok {
		out.Deferred = true
		out.AttemptCount = state.Count
		out.NextAttempt = state.NextAttempt
		out.TaskID = state.LastTaskID
		if state.Ignored {
			out.Stage = journal.StageUpdateTask
		} else {
			out.Stage = journal.StageFindTask
		}
		return out
	}

	// Этап: авторизация Pyrus.
	out.Stage = journal.StagePyrusAuth
	col.SetStage(string(journal.StagePyrusAuth))
	client := pyrus.NewClient(cfg.Pyrus.AuthURL, cfg.Pyrus.Login, cfg.Pyrus.SecurityKey)
	client.SetLogger(w.log)
	if err := client.Authorize(ctx); err != nil {
		out.Err = fmt.Errorf("авторизация Pyrus не удалась (логин %s): %w", cfg.Pyrus.Login, err)
		return out
	}

	// Этап: загрузка файлов.
	out.Stage = journal.StageUpload
	col.SetStage(string(journal.StageUpload))
	guids := make([]string, 0, len(msg.Attachments))
	for _, att := range msg.Attachments {
		guid, err := client.UploadFile(ctx, att.Filename, att.Content)
		if err != nil {
			st := w.attempts.RecordAttemptWithMsg(info.ClientName, false, cfg.Pyrus.MaxUpdateAttempts, cfg.Pyrus.RetryIntervalMinutes, msg.MessageID)
			out.AttemptCount = st.Count
			out.NextAttempt = st.NextAttempt
			out.Err = fmt.Errorf("не удалось загрузить файл %s: %w", att.Filename, err)
			return out
		}
		guids = append(guids, guid)
	}

	// Этап: обновление задачи.
	out.Stage = journal.StageUpdateTask
	col.SetStage(string(journal.StageUpdateTask))
	filenames := msgFilenames(msg)
	tur, stage, err := w.processTaskUpdate(ctx, client, cfg, info.ClientName, guids, filenames, info)
	out.TaskID = tur.TaskID
	out.PyrusStatus = tur.PyrusStatus
	if err != nil {
		out.Stage = stage
		st := w.attempts.RecordAttemptWithMsg(info.ClientName, false, cfg.Pyrus.MaxUpdateAttempts, cfg.Pyrus.RetryIntervalMinutes, msg.MessageID)
		out.AttemptCount = st.Count
		out.NextAttempt = st.NextAttempt
		out.Err = err
		return out
	}

	// Успех — сбрасываем попытки.
	w.attempts.RecordAttemptWithMsg(info.ClientName, true, cfg.Pyrus.MaxUpdateAttempts, cfg.Pyrus.RetryIntervalMinutes, msg.MessageID)
	out.Stage = journal.StageDone
	return out
}

// findInvoiceAttachment выбирает вложение-счёт (по распознаваемому имени файла)
// и извлекает из него номер и дату счёта. Если ни одно имя не распознано,
// берёт первое вложение как имя файла, но номер/дату оставляет пустыми.
func findInvoiceAttachment(msg *gmail.Message) (filename, invoiceNo string, invoiceDate time.Time) {
	for _, att := range msg.Attachments {
		if no, date, err := invoice.ParseInvoiceFromFilename(att.Filename); err == nil {
			return att.Filename, no, date
		}
	}
	if len(msg.Attachments) > 0 {
		return msg.Attachments[0].Filename, "", time.Time{}
	}
	return "", "", time.Time{}
}

// dateSuffix возвращает « от ДД.ММ.ГГГГ» или пустую строку для нулевой даты.
func dateSuffix(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return " от " + t.Format("02.01.2006")
}

// formatMsgDate форматирует дату письма для лога.
func formatMsgDate(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("02.01.2006 15:04")
}

// CheckNow выполняет разовую проверку почты вне расписания (для тестового режима
// и кнопки «Проверить сейчас» в админке). Возвращает подробную сводку.
func (w *Watcher) CheckNow() (CheckSummary, error) {
	var summary CheckSummary
	cfg := w.cfgMgr.Get()
	if !cfg.IsReady() {
		return summary, fmt.Errorf("настройки почты и/или Pyrus заполнены не полностью")
	}

	dial := gmail.DialConfig{
		Host:        cfg.Gmail.IMAPHost,
		Port:        cfg.Gmail.IMAPPort,
		Email:       cfg.Gmail.Email,
		AppPassword: cfg.Gmail.AppPassword,
	}
	client, err := gmail.Connect(dial, nil)
	if err != nil {
		return summary, err
	}
	defer client.Logout()
	defer client.Close()

	if err := gmail.SelectInbox(context.Background(), client); err != nil {
		return summary, err
	}

	summary, err = w.processNewMessages(client, cfg)
	if err != nil {
		return summary, err
	}
	w.logSummary(summary)
	return summary, nil
}

// logSummary пишет в лог итоговую сводку проверки: сколько найдено, успешно,
// с ошибками, отложено, а затем построчно по каждому письму.
func (w *Watcher) logSummary(s CheckSummary) {
	w.log.Info("Проверка писем завершена. Найдено новых: %d, успешно: %d, с ошибками: %d, отложено: %d, пропущено: %d",
		s.Found, s.Processed, s.Failed, s.Deferred, s.Skipped)
	for _, r := range s.Results {
		switch {
		case r.Success:
			w.log.Info("  ✅ %s | %s | письмо %s | счёт №%s%s | лицензия №%s%s | %.2f | задача: %s",
				r.ClientName, r.From, formatMsgDate(r.MsgDate),
				r.InvoiceNo, dateSuffix(r.InvoiceDate), r.LicenseNo, dateSuffix(r.LicenseDate),
				r.Amount, journal.PyrusTaskURL(r.TaskID))
		case r.Deferred:
			w.log.Info("  ⏳ %s | письмо %s | отложено (retry) | счёт №%s%s",
				r.ClientName, formatMsgDate(r.MsgDate), r.InvoiceNo, dateSuffix(r.InvoiceDate))
		case r.Skipped:
			w.log.Info("  ⤼ письмо %s от %s | пропущено: %s", formatMsgDate(r.MsgDate), r.From, r.ErrText)
		default:
			w.log.Info("  ❌ письмо %s от %s | ошибка (этап %s): %s",
				formatMsgDate(r.MsgDate), r.From, r.ErrStage, r.ErrText)
		}
	}
}

// signalNewMail неблокирующе сигнализирует основному циклу о новом письме.
func (w *Watcher) signalNewMail() {
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

// sleep ждёт указанное время либо до остановки.
func (w *Watcher) sleep(d time.Duration) {
	if d <= 0 {
		d = time.Second
	}
	select {
	case <-w.stop:
	case <-time.After(d):
	}
}

// untilNextPeriodCheck вычисляет паузу вне периода: проверяем раз в час,
// но не дольше, чем до начала следующего активного периода.
func untilNextPeriodCheck(now time.Time, startDay int) time.Duration {
	const maxSleep = time.Hour
	return maxSleep
}
