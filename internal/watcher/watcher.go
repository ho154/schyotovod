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
	"schyotovod/internal/logger"
	"schyotovod/internal/pyrus"
)

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
	log      *logger.Logger

	mu       sync.Mutex
	notify   chan struct{} // сигнал «есть новое письмо» (от IDLE-обработчика)
	stopOnce sync.Once
	stop     chan struct{}
}

// New создаёт наблюдатель.
func New(cfgMgr *config.Manager, dd *dedup.Dedup, att AttemptsManager, log *logger.Logger) *Watcher {
	return &Watcher{
		cfgMgr:   cfgMgr,
		dedup:    dd,
		attempts: att,
		log:      log,
		notify:   make(chan struct{}, 1),
		stop:     make(chan struct{}),
	}
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

	if err := gmail.SelectInbox(client); err != nil {
		return err
	}

	w.log.Info("Подключение к почте установлено, ожидание писем со счетами (период %d–%d числа)",
		cfg.Filter.StartDay, cfg.Filter.EndDay)

	// Сразу проверяем письма, пришедшие до старта наблюдения.
	if err := w.processNewMessages(client, cfg); err != nil {
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
			if err := w.processNewMessages(client, cfg); err != nil {
				w.log.Error("Ошибка обработки писем: %v", err)
			}

		case <-fallbackTicker.C:
			// Резервная проверка.
			_ = idleCmd.Close()
			if err := idleCmd.Wait(); err != nil {
				w.log.Warn("Ожидание IDLE завершилось с ошибкой: %v", err)
			}
			w.log.Info("Резервная проверка почты")
			if err := w.processNewMessages(client, cfg); err != nil {
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

// processNewMessages ищет письма за период, отбирает необработанные и обрабатывает их.
func (w *Watcher) processNewMessages(client *imapclient.Client, cfg config.Config) error {
	loc, _ := cfg.Location()
	now := time.Now().In(loc)
	since, before := PeriodBounds(now, cfg.Filter.StartDay, cfg.Filter.EndDay)

	uids, err := gmail.Search(client, gmail.SearchCriteria{
		SenderEmail: cfg.Filter.SenderEmail,
		Since:       since,
		Before:      before,
	})
	if err != nil {
		return err
	}

	for _, uid := range uids {
		select {
		case <-w.stop:
			return nil
		default:
		}

		msg, err := gmail.FetchMessage(client, uid)
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

		if len(msg.Attachments) == 0 {
			w.log.Warn("ПРЕДУПРЕЖДЕНИЕ ПОЧТЫ: Письмо от %s без вложений, пропуск (тема: %q)", msg.From, msg.Subject)
			// Отмечаем обработанным, чтобы не проверять его повторно каждый раз.
			_ = w.dedup.MarkProcessed(msg.MessageID)
			continue
		}

		targetTaskID, err := w.attachToPyrus(cfg, msg)
		if err != nil {
			w.log.Error("ОШИБКА ОБРАБОТКИ: Не удалось прикрепить счёт из письма от %s в Pyrus: %v", msg.From, err)
			continue // не отмечаем обработанным — повторим при следующей проверке
		}

		if err := w.dedup.MarkProcessed(msg.MessageID); err != nil {
			w.log.Warn("Счёт прикреплён, но не удалось сохранить отметку об обработке: %v", err)
		}
		w.log.Info("Счёт из письма от %s успешно прикреплён в задачу Pyrus %d (файлов: %d)",
			msg.From, targetTaskID, len(msg.Attachments))
	}
	return nil
}

// attachToPyrus загружает вложения письма в Pyrus и прикрепляет их в поле-вложение задачи.
// checkAndProcessTaskAttempts проверяет, пришло ли время делать следующую попытку.
func (w *Watcher) checkAndProcessTaskAttempts(cfg config.Config, clientName string) (bool, error) {
	state := w.attempts.GetState(clientName)
	if state.Ignored {
		w.log.Info("Обработка клиента %q проигнорирована: исчерпан лимит попыток обновления", clientName)
		return false, nil
	}
	if !state.NextAttempt.IsZero() && time.Now().Before(state.NextAttempt) {
		w.log.Info("Обработка клиента %q отложена: следующая попытка после %s", clientName, state.NextAttempt.Format("15:04:05"))
		return false, nil
	}
	return true, nil
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

func (w *Watcher) processTaskUpdate(ctx context.Context, pClient *pyrus.Client, cfg config.Config, clientName string, guids []string, filenames []string, info *invoice.InvoiceInfo) (int, bool, error) {
	loc, _ := cfg.Location()
	now := time.Now().In(loc)
	since, _ := PeriodBounds(now, cfg.Filter.StartDay, cfg.Filter.EndDay)

	// Ищем задачу, фильтруя на уровне API Pyrus по дате начала периода
	tasks, err := pClient.FindTasksByForm(ctx, cfg.Pyrus.FormID, since)
	if err != nil {
		return 0, false, fmt.Errorf("ошибка поиска задач по форме: %w", err)
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
		return 0, false, fmt.Errorf("задача для клиента %q не найдена", clientName)
	}
	if foundCount > 1 {
		return 0, false, fmt.Errorf("найдено более одной открытой задачи (%d) для клиента %q", foundCount, clientName)
	}

	// Получаем детальную информацию о задаче, чтобы проверить, есть ли уже прикрепленный файл
	taskDetails, err := pClient.GetTask(ctx, targetTaskID)
	if err != nil {
		return targetTaskID, false, fmt.Errorf("не удалось получить детали задачи %d: %w", targetTaskID, err)
	}

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
		return targetTaskID, false, err
	}

	return targetTaskID, true, nil
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

// attachToPyrus загружает вложения письма в Pyrus и прикрепляет их в найденную задачу.
// Возвращает ID задачи, в которую были загружены файлы.
func (w *Watcher) attachToPyrus(cfg config.Config, msg *gmail.Message) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Парсим текст письма
	info, err := invoice.ParseMessageBody(msg.Body)
	if err != nil {
		return 0, fmt.Errorf("ошибка парсинга тела письма: %w", err)
	}

	// Проверяем попытки перед подключением к Pyrus
	ok, err := w.checkAndProcessTaskAttempts(cfg, info.ClientName)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil // обработка отложена или игнорируется
	}

	client := pyrus.NewClient(cfg.Pyrus.AuthURL, cfg.Pyrus.Login, cfg.Pyrus.SecurityKey)
	client.SetLogger(w.log)
	if err := client.Authorize(ctx); err != nil {
		w.log.Error("ОШИБКА АВТОРИЗАЦИИ PYRUS: Не удалось войти в Pyrus (логин: %s): %v", cfg.Pyrus.Login, err)
		return 0, err
	}

	// Загружаем файлы
	guids := make([]string, 0, len(msg.Attachments))
	for _, att := range msg.Attachments {
		guid, err := client.UploadFile(ctx, att.Filename, att.Content)
		if err != nil {
			w.log.Error("ОШИБКА ЗАГРУЗКИ ФАЙЛА PYRUS: Не удалось загрузить файл %s: %v", att.Filename, err)
			// Регистрируем неудачную попытку с привязкой к Message-ID письма
			state := w.attempts.RecordAttemptWithMsg(info.ClientName, false, cfg.Pyrus.MaxUpdateAttempts, cfg.Pyrus.RetryIntervalMinutes, msg.MessageID)
			w.logAttemptOutcome(cfg, info.ClientName, state)
			return 0, err
		}
		guids = append(guids, guid)
	}

	// Собираем имена файлов вложений
	filenames := msgFilenames(msg)

	// Обновляем задачу в Pyrus
	targetTaskID, _, err := w.processTaskUpdate(ctx, client, cfg, info.ClientName, guids, filenames, info)
	if err != nil {
		w.log.Error("ОШИБКА ОБНОВЛЕНИЯ ЗАДАЧИ PYRUS: Не удалось обновить задачу %d для клиента %s: %v", targetTaskID, info.ClientName, err)
		// При ошибке регистрируем неудачную попытку с привязкой к Message-ID письма
		state := w.attempts.RecordAttemptWithMsg(info.ClientName, false, cfg.Pyrus.MaxUpdateAttempts, cfg.Pyrus.RetryIntervalMinutes, msg.MessageID)
		w.logAttemptOutcome(cfg, info.ClientName, state)
		return targetTaskID, err
	}

	// При успехе сбрасываем попытки
	w.attempts.RecordAttemptWithMsg(info.ClientName, true, cfg.Pyrus.MaxUpdateAttempts, cfg.Pyrus.RetryIntervalMinutes, msg.MessageID)

	return targetTaskID, nil
}

// logAttemptOutcome пишет в лог понятное сообщение о результате неудачной
// попытки: какая это была попытка по счёту из максимума и когда (через сколько
// минут) состоится следующая, либо что лимит попыток исчерпан и клиент
// больше не будет обрабатываться до конца текущего месяца.
func (w *Watcher) logAttemptOutcome(cfg config.Config, clientName string, state attempts.AttemptState) {
	if state.Ignored {
		w.log.Error("Клиент %q: исчерпан лимит попыток (%d из %d). Обработка приостановлена до конца текущего месяца.",
			clientName, cfg.Pyrus.MaxUpdateAttempts, cfg.Pyrus.MaxUpdateAttempts)
		return
	}
	if state.NextAttempt.IsZero() {
		return
	}
	wait := time.Until(state.NextAttempt)
	if wait < 0 {
		wait = 0
	}
	w.log.Error("Клиент %q: попытка %d из %d не удалась. Следующая попытка через %d мин. (в %s).",
		clientName, state.Count, cfg.Pyrus.MaxUpdateAttempts,
		int(wait.Round(time.Minute).Minutes()), state.NextAttempt.Format("15:04:05"))
}

// CheckNow выполняет разовую проверку почты вне расписания (для тестового режима
// и кнопки «Проверить сейчас» в админке). Возвращает число обработанных писем и ошибку.
func (w *Watcher) CheckNow() (int, error) {
	cfg := w.cfgMgr.Get()
	if !cfg.IsReady() {
		return 0, fmt.Errorf("настройки почты и/или Pyrus заполнены не полностью")
	}

	dial := gmail.DialConfig{
		Host:        cfg.Gmail.IMAPHost,
		Port:        cfg.Gmail.IMAPPort,
		Email:       cfg.Gmail.Email,
		AppPassword: cfg.Gmail.AppPassword,
	}
	client, err := gmail.Connect(dial, nil)
	if err != nil {
		return 0, err
	}
	defer client.Logout()
	defer client.Close()

	if err := gmail.SelectInbox(client); err != nil {
		return 0, err
	}

	before := w.dedup.Count()
	if err := w.processNewMessages(client, cfg); err != nil {
		return 0, err
	}
	processed := w.dedup.Count() - before
	if processed < 0 {
		processed = 0
	}
	return processed, nil
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
