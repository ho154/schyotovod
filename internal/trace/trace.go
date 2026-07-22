// Package trace реализует сбор структурированных «шагов» (Step) обработки
// одного письма: HTTP-запросы к Pyrus и IMAP-операции к почтовому серверу.
// Коллектор передаётся через context.Context, чтобы не менять сигнатуры всех
// методов клиентов. Собранные шаги затем сохраняются в журнал событий
// (internal/journal) и служат инструментом анализа «что и на каком этапе
// пошло не так».
//
// Тела запросов/ответов сохраняются ПОЛНОСТЬЮ (без обрезки), но перед
// сохранением из них удаляются чувствительные данные (security_key,
// access_token, app_password, заголовок Authorization) через Sanitize.
package trace

import (
	"context"
	"regexp"
	"sync"
	"time"
)

// Kind — вид шага (HTTP-запрос к Pyrus или IMAP-операция).
type Kind string

const (
	KindPyrus Kind = "pyrus"
	KindIMAP  Kind = "imap"
)

// Step — один зафиксированный шаг обработки: запрос к внешней системе и ответ.
type Step struct {
	Seq          int       `json:"seq"`           // порядковый номер в рамках обработки письма
	Kind         Kind      `json:"kind"`          // pyrus | imap
	Stage        string    `json:"stage"`         // этап пайплайна (journal.Stage), к которому относится шаг
	Method       string    `json:"method"`        // HTTP-метод или IMAP-команда (LOGIN/SELECT/SEARCH/FETCH)
	Endpoint     string    `json:"endpoint"`      // URL запроса или описание IMAP-цели
	RequestBody  string    `json:"request_body"`  // тело запроса (с замаскированными секретами)
	StatusCode   int       `json:"status_code"`   // HTTP-код ответа (для IMAP — 0)
	ResponseBody string    `json:"response_body"` // тело ответа (с замаскированными секретами)
	DurationMs   int64     `json:"duration_ms"`   // длительность шага в миллисекундах
	Error        string    `json:"error"`         // текст ошибки, если шаг завершился неудачей
	Time         time.Time `json:"time"`          // момент завершения шага
}

// Collector потокобезопасно накапливает шаги обработки одного письма.
type Collector struct {
	mu    sync.Mutex
	seq   int
	stage string
	steps []Step
}

// NewCollector создаёт пустой коллектор.
func NewCollector() *Collector {
	return &Collector{}
}

// SetStage задаёт текущий этап пайплайна, который будет проставляться в
// последующих шагах, если у самого шага этап не указан явно.
func (c *Collector) SetStage(stage string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.stage = stage
	c.mu.Unlock()
}

// Record добавляет шаг в коллектор. Тела запроса/ответа маскируются здесь же,
// поэтому вызывающий код может передавать «сырые» строки. Nil-safe: вызов на
// nil-коллекторе (когда трассировка не нужна) ничего не делает.
func (c *Collector) Record(s Step) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	s.Seq = c.seq
	if s.Stage == "" {
		s.Stage = c.stage
	}
	if s.Time.IsZero() {
		s.Time = time.Now()
	}
	s.RequestBody = string(Sanitize([]byte(s.RequestBody)))
	s.ResponseBody = string(Sanitize([]byte(s.ResponseBody)))
	c.steps = append(c.steps, s)
}

// Steps возвращает копию накопленных шагов.
func (c *Collector) Steps() []Step {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Step, len(c.steps))
	copy(out, c.steps)
	return out
}

// --- Передача коллектора через context ---

type ctxKey struct{}

// WithCollector возвращает контекст с вложенным коллектором.
func WithCollector(ctx context.Context, c *Collector) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// FromContext извлекает коллектор из контекста. Возвращает nil, если его нет —
// все методы Collector nil-safe, поэтому вызывающий код может не проверять.
func FromContext(ctx context.Context) *Collector {
	if ctx == nil {
		return nil
	}
	if c, ok := ctx.Value(ctxKey{}).(*Collector); ok {
		return c
	}
	return nil
}

// --- Маскирование секретов ---

var (
	// JSON-поля с секретами: "security_key":"...."  → "security_key":"***"
	reSecurityKey = regexp.MustCompile(`("security_key"\s*:\s*")[^"]*(")`)
	reAppPassword = regexp.MustCompile(`("app_password"\s*:\s*")[^"]*(")`)
	// access_token маскируем частично (первые/последние 4 символа), но для
	// простоты и безопасности при сохранении на диск скрываем полностью.
	reAccessToken = regexp.MustCompile(`("access_token"\s*:\s*")[^"]*(")`)
	// Заголовок Authorization: Bearer <token> (в т.ч. в Go-виде
	// map[Authorization:[Bearer <token>]]).
	reAuthHeader = regexp.MustCompile(`(?i)(Authorization\W{0,3}Bearer\s+)\S+`)
	// IMAP LOGIN <user> <pass> — прячем пароль (второй аргумент).
	reIMAPLogin = regexp.MustCompile(`(?i)(LOGIN\s+\S+\s+)\S+`)
)

// Sanitize удаляет чувствительные данные из тела запроса/ответа перед
// сохранением. Возвращает новый срез; исходный не изменяется.
func Sanitize(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	out := body
	out = reSecurityKey.ReplaceAll(out, []byte(`${1}***${2}`))
	out = reAppPassword.ReplaceAll(out, []byte(`${1}***${2}`))
	out = reAccessToken.ReplaceAll(out, []byte(`${1}***${2}`))
	out = reAuthHeader.ReplaceAll(out, []byte(`${1}***`))
	out = reIMAPLogin.ReplaceAll(out, []byte(`${1}***`))
	return out
}
