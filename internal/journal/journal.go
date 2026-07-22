// Package journal ведёт структурированный «Журнал событий» (ЖС) обработки
// писем со счетами: по каждому письму хранится запись Event с данными счёта,
// лицензии, клиента, текущим этапом обработки и статусом, а также отдельно —
// полный трейс HTTP/IMAP-шагов (Trace) для анализа «что и на каком этапе
// пошло не так».
//
// Хранение двухуровневое:
//   - <dir>/events.json      — лёгкие записи Event для таблицы в веб-панели;
//   - <dir>/traces/<id>.json — тяжёлые трейсы (тела запросов/ответов), полные,
//     без обрезки; подгружаются лениво по клику в интерфейсе.
//
// Очистка (Cleanup) удаляет записи и связанные трейсы старше retentionDays —
// вызывается из того же суточного планировщика, что и очистка текстовых логов,
// с той же настройкой general.log_retention_days.
package journal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"schyotovod/internal/trace"
)

// Stage — технический этап обработки письма ботом. Это НЕ бизнес-статус задачи
// в Pyrus (для него в Event есть отдельное поле PyrusTaskStatus).
type Stage string

const (
	StageReceived   Stage = "получено"
	StageParseBody  Stage = "разбор_письма"
	StageParseFile  Stage = "разбор_имени_файла"
	StagePyrusAuth  Stage = "авторизация_pyrus"
	StageFindTask   Stage = "поиск_задачи"
	StageUpload     Stage = "загрузка_файла"
	StageUpdateTask Stage = "обновление_задачи"
	StageDone       Stage = "завершено"
)

// StageStatus — итоговый статус обработки письма.
type StageStatus string

const (
	StatusOK      StageStatus = "ok"       // успешно завершено
	StatusFailed  StageStatus = "ошибка"   // ошибка на каком-то этапе
	StatusPending StageStatus = "ожидание" // отложено (retry) / в процессе
	StatusSkipped StageStatus = "пропущено"
)

// Event — лёгкая запись журнала по одному письму (для таблицы в веб-панели).
type Event struct {
	ID          string    `json:"id"` // = MessageID (ключ записи)
	MessageID   string    `json:"message_id"`
	From        string    `json:"from"`
	Subject     string    `json:"subject"`
	MsgDate     time.Time `json:"msg_date"`     // дата письма
	ClientName  string    `json:"client_name"`  // имя клиента (из письма)
	LicenseNo   string    `json:"license_no"`   // номер лицензии (из тела письма)
	LicenseDate time.Time `json:"license_date"` // дата лицензии (из тела письма)
	InvoiceNo   string    `json:"invoice_no"`   // номер счёта (из имени файла)
	InvoiceDate time.Time `json:"invoice_date"` // дата счёта (из имени файла)
	Amount      float64   `json:"amount"`
	Filename    string    `json:"filename"`

	CurrentStage  Stage       `json:"current_stage"`
	OverallStatus StageStatus `json:"overall_status"`
	ErrorStage    Stage       `json:"error_stage,omitempty"`
	ErrorMessage  string      `json:"error_message,omitempty"`

	TaskID          int    `json:"task_id,omitempty"`
	PyrusTaskStatus string `json:"pyrus_task_status,omitempty"` // бизнес-статус задачи в Pyrus (поле id:29)

	AttemptCount int       `json:"attempt_count,omitempty"`
	NextAttempt  time.Time `json:"next_attempt,omitempty"`

	StepsCount int       `json:"steps_count"` // сколько HTTP/IMAP-шагов в трейсе
	CreatedAt  time.Time `json:"created_at"`  // когда сервис впервые увидел письмо
	UpdatedAt  time.Time `json:"updated_at"`  // последнее изменение
}

// TaskURL возвращает ссылку на задачу Pyrus с именем клиента рядом, например
// "https://pyrus.com/t#id1089745 («ИП Эксперт»)". Если TaskID нулевой —
// возвращает пустую строку.
func (e Event) TaskURL() string {
	if e.TaskID == 0 {
		return ""
	}
	url := PyrusTaskURL(e.TaskID)
	if e.ClientName != "" {
		return fmt.Sprintf("%s («%s»)", url, e.ClientName)
	}
	return url
}

// PyrusTaskURL формирует ссылку на задачу Pyrus по её ID.
func PyrusTaskURL(taskID int) string {
	return fmt.Sprintf("https://pyrus.com/t#id%d", taskID)
}

// store — сериализуемая структура файла events.json.
type store struct {
	Events []Event `json:"events"`
}

// Manager управляет журналом потокобезопасно.
type Manager struct {
	mu        sync.Mutex
	dir       string // каталог журнала (внутри — events.json и traces/)
	eventPath string
	tracesDir string
	loc       *time.Location
	events    map[string]Event // ключ — Event.ID
	order     []string         // порядок появления (для стабильности)
}

// New создаёт менеджер журнала. dir — каталог для файлов журнала.
func New(dir string, loc *time.Location) (*Manager, error) {
	if loc == nil {
		loc = time.UTC
	}
	m := &Manager{
		dir:       dir,
		eventPath: filepath.Join(dir, "events.json"),
		tracesDir: filepath.Join(dir, "traces"),
		loc:       loc,
		events:    make(map[string]Event),
	}
	if err := os.MkdirAll(m.tracesDir, 0o700); err != nil {
		return nil, fmt.Errorf("не удалось создать каталог трейсов журнала %q: %w", m.tracesDir, err)
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) load() error {
	data, err := os.ReadFile(m.eventPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("не удалось прочитать журнал событий %q: %w", m.eventPath, err)
	}
	var s store
	if err := json.Unmarshal(data, &s); err != nil {
		// Повреждённый файл — не роняем сервис, начинаем с чистого журнала.
		return nil
	}
	for _, ev := range s.Events {
		if _, exists := m.events[ev.ID]; !exists {
			m.order = append(m.order, ev.ID)
		}
		m.events[ev.ID] = ev
	}
	return nil
}

// saveLocked записывает events.json. Должна вызываться под удержанным mu.
func (m *Manager) saveLocked() error {
	s := store{Events: m.eventsSliceLocked()}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("ошибка сериализации журнала событий: %w", err)
	}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return fmt.Errorf("не удалось создать каталог журнала %q: %w", m.dir, err)
	}
	tmp := m.eventPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("не удалось записать временный файл журнала: %w", err)
	}
	if err := os.Rename(tmp, m.eventPath); err != nil {
		return fmt.Errorf("не удалось заменить файл журнала: %w", err)
	}
	return nil
}

// eventsSliceLocked возвращает события в порядке появления. Под удержанным mu.
func (m *Manager) eventsSliceLocked() []Event {
	out := make([]Event, 0, len(m.order))
	for _, id := range m.order {
		if ev, ok := m.events[id]; ok {
			out = append(out, ev)
		}
	}
	return out
}

// tracePath возвращает путь к файлу трейса для указанного event ID.
func (m *Manager) tracePath(id string) string {
	return filepath.Join(m.tracesDir, sanitizeID(id)+".json")
}

// sanitizeID приводит Message-ID к безопасному для имени файла виду.
func sanitizeID(id string) string {
	repl := func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', ' ', '@':
			return '_'
		}
		return r
	}
	out := make([]rune, 0, len(id))
	for _, r := range id {
		out = append(out, repl(r))
	}
	s := string(out)
	if len(s) > 180 {
		s = s[:180]
	}
	if s == "" {
		s = "unknown"
	}
	return s
}

// Upsert создаёт или обновляет запись Event по её ID. Проставляет CreatedAt при
// первом появлении и UpdatedAt при каждом изменении.
func (m *Manager) Upsert(ev Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().In(m.loc)
	if existing, ok := m.events[ev.ID]; ok {
		if ev.CreatedAt.IsZero() {
			ev.CreatedAt = existing.CreatedAt
		}
	} else {
		if ev.CreatedAt.IsZero() {
			ev.CreatedAt = now
		}
		m.order = append(m.order, ev.ID)
	}
	ev.UpdatedAt = now
	m.events[ev.ID] = ev
	_ = m.saveLocked()
}

// Get возвращает событие по ID.
func (m *Manager) Get(id string) (Event, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ev, ok := m.events[id]
	return ev, ok
}

// SaveTrace сохраняет полный трейс шагов для события (перезаписывает файл).
func (m *Manager) SaveTrace(eventID string, steps []trace.Step) error {
	m.mu.Lock()
	path := m.tracePath(eventID)
	m.mu.Unlock()

	raw, err := json.MarshalIndent(steps, "", "  ")
	if err != nil {
		return fmt.Errorf("ошибка сериализации трейса: %w", err)
	}
	if err := os.MkdirAll(m.tracesDir, 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("не удалось записать трейс: %w", err)
	}
	return os.Rename(tmp, path)
}

// LoadTrace читает трейс шагов для события. Если файла нет — возвращает nil, nil.
func (m *Manager) LoadTrace(eventID string) ([]trace.Step, error) {
	m.mu.Lock()
	path := m.tracePath(eventID)
	m.mu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var steps []trace.Step
	if err := json.Unmarshal(data, &steps); err != nil {
		return nil, fmt.Errorf("ошибка разбора трейса: %w", err)
	}
	return steps, nil
}

// ListOptions — параметры выборки для таблицы журнала.
type ListOptions struct {
	Status     string // фильтр по OverallStatus ("" = все)
	Client     string // подстрока имени клиента (без учёта регистра, "" = все)
	SortBy     string // msg_date | client | invoice_no | amount | status | updated
	Descending bool
}

// List возвращает отфильтрованные и отсортированные события для отображения.
func (m *Manager) List(opts ListOptions) []Event {
	m.mu.Lock()
	events := m.eventsSliceLocked()
	m.mu.Unlock()

	// Фильтрация.
	filtered := events[:0:0]
	clientNeedle := normalizeLower(opts.Client)
	for _, ev := range events {
		if opts.Status != "" && string(ev.OverallStatus) != opts.Status {
			continue
		}
		if clientNeedle != "" && !containsLower(ev.ClientName, clientNeedle) {
			continue
		}
		filtered = append(filtered, ev)
	}

	// Сортировка.
	less := sortLess(opts.SortBy)
	sort.SliceStable(filtered, func(i, j int) bool {
		if opts.Descending {
			return less(filtered[j], filtered[i])
		}
		return less(filtered[i], filtered[j])
	})
	return filtered
}

// normalizeLower приводит строку к нижнему регистру с обрезкой пробелов.
func normalizeLower(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// containsLower проверяет вхождение needle (уже в нижнем регистре) в haystack
// без учёта регистра.
func containsLower(haystack, needleLower string) bool {
	return strings.Contains(strings.ToLower(haystack), needleLower)
}

func sortLess(by string) func(a, b Event) bool {
	switch by {
	case "client":
		return func(a, b Event) bool { return a.ClientName < b.ClientName }
	case "invoice_no":
		return func(a, b Event) bool { return a.InvoiceNo < b.InvoiceNo }
	case "amount":
		return func(a, b Event) bool { return a.Amount < b.Amount }
	case "status":
		return func(a, b Event) bool { return a.OverallStatus < b.OverallStatus }
	case "updated":
		return func(a, b Event) bool { return a.UpdatedAt.Before(b.UpdatedAt) }
	default: // msg_date
		return func(a, b Event) bool { return a.MsgDate.Before(b.MsgDate) }
	}
}

// Cleanup удаляет записи старше retentionDays (по CreatedAt) и связанные трейсы.
// Возвращает число удалённых записей.
func (m *Manager) Cleanup(retentionDays int) (int, error) {
	if retentionDays <= 0 {
		return 0, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().In(m.loc).AddDate(0, 0, -retentionDays)
	removed := 0
	newOrder := m.order[:0:0]
	for _, id := range m.order {
		ev, ok := m.events[id]
		if !ok {
			continue
		}
		if ev.CreatedAt.Before(cutoff) {
			delete(m.events, id)
			_ = os.Remove(m.tracePath(id))
			removed++
			continue
		}
		newOrder = append(newOrder, id)
	}
	m.order = newOrder
	if removed > 0 {
		if err := m.saveLocked(); err != nil {
			return removed, err
		}
	}
	return removed, nil
}
