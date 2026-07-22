// Package validation проверяет корректность настроек перед их применением:
// выполняет реальные проверочные запросы к Gmail (авторизация + выбор INBOX)
// и к Pyrus (получение токена, проверка задачи и поля-вложения).
// Все сообщения об ошибках — на русском и привязаны к конкретным полям формы.
package validation

import (
	"context"
	"strings"
	"time"

	"schyotovod/internal/config"
	"schyotovod/internal/gmail"
	"schyotovod/internal/pyrus"
)

// debugLogger — минимальный интерфейс логгера для отладочного вывода полных
// тел запросов/ответов Pyrus во время проверки настроек (см. pyrus.Client.SetLogger).
type debugLogger interface {
	Debug(format string, args ...any)
}

// FieldError — ошибка, привязанная к конкретному полю конфигурации.
type FieldError struct {
	// Field — ключ поля (например, "gmail.app_password"), для отображения ошибки под нужным полем.
	Field string `json:"field"`
	// Message — человекочитаемое сообщение на русском.
	Message string `json:"message"`
}

// Result — результат валидации.
type Result struct {
	// GmailOK — проверка почты пройдена.
	GmailOK bool `json:"gmail_ok"`
	// PyrusOK — проверка Pyrus пройдена.
	PyrusOK bool `json:"pyrus_ok"`
	// Errors — список ошибок по полям (пустой при успехе).
	Errors []FieldError `json:"errors"`
}

// OK сообщает, прошла ли валидация полностью.
func (r Result) OK() bool {
	return len(r.Errors) == 0
}

// attachmentFieldTypes — типы полей Pyrus, считающиеся полем «Вложение».
// Pyrus использует тип "attachment" (в некоторых формах — "file").
var attachmentFieldTypes = map[string]bool{
	"attachment": true,
	"file":       true,
	"files":      true,
}

// Validate выполняет полную проверку настроек. Не изменяет состояние приложения.
// log может быть nil — тогда отладочные сообщения (полные тела запросов/ответов
// Pyrus) не пишутся.
func Validate(ctx context.Context, cfg config.Config, log debugLogger) Result {
	res := Result{}

	validateGmail(ctx, cfg, &res)
	validatePyrus(ctx, cfg, &res, log)

	return res
}

// validateGmail проверяет доступ к почте: авторизацию и открытие INBOX.
func validateGmail(_ context.Context, cfg config.Config, res *Result) {
	if cfg.Gmail.Email == "" {
		res.Errors = append(res.Errors, FieldError{
			Field:   "gmail.email",
			Message: "Не указан адрес почты Gmail.",
		})
	}
	if cfg.Gmail.AppPassword == "" {
		res.Errors = append(res.Errors, FieldError{
			Field:   "gmail.app_password",
			Message: "Не указан пароль приложения (App Password).",
		})
	}
	if cfg.Filter.SenderEmail == "" {
		res.Errors = append(res.Errors, FieldError{
			Field:   "filter.sender_email",
			Message: "Не указан адрес отправителя счёта.",
		})
	}
	if cfg.Gmail.Email == "" || cfg.Gmail.AppPassword == "" {
		return
	}

	dial := gmail.DialConfig{
		Host:        cfg.Gmail.IMAPHost,
		Port:        cfg.Gmail.IMAPPort,
		Email:       cfg.Gmail.Email,
		AppPassword: cfg.Gmail.AppPassword,
	}
	client, err := gmail.Connect(dial, nil)
	if err != nil {
		res.Errors = append(res.Errors, FieldError{
			Field: "gmail.app_password",
			Message: "Не удалось подключиться к почте. Проверьте адрес и пароль приложения " +
				"(это НЕ обычный пароль, а App Password из настроек Google). Подробности: " + shortErr(err),
		})
		return
	}
	defer client.Logout()
	defer client.Close()

	if err := gmail.SelectInbox(client); err != nil {
		res.Errors = append(res.Errors, FieldError{
			Field:   "gmail.email",
			Message: "Почта подключена, но не удалось открыть папку «Входящие»: " + shortErr(err),
		})
		return
	}

	res.GmailOK = true
}

// validatePyrus проверяет доступ к Pyrus: токен, задачу и поле-вложение.
func validatePyrus(ctx context.Context, cfg config.Config, res *Result, log debugLogger) {
	if cfg.Pyrus.Login == "" {
		res.Errors = append(res.Errors, FieldError{
			Field:   "pyrus.login",
			Message: "Не указан логин бота Pyrus.",
		})
	}
	if cfg.Pyrus.SecurityKey == "" {
		res.Errors = append(res.Errors, FieldError{
			Field:   "pyrus.security_key",
			Message: "Не указан ключ безопасности бота Pyrus.",
		})
	}
	if cfg.Pyrus.FormID == 0 {
		res.Errors = append(res.Errors, FieldError{
			Field:   "pyrus.form_id",
			Message: "Не указан ID формы Pyrus, в которой ищутся задачи клиентов.",
		})
	}
	if cfg.Pyrus.AttachmentFieldID == 0 {
		res.Errors = append(res.Errors, FieldError{
			Field:   "pyrus.attachment_field_id",
			Message: "Не указан Field ID поля-вложения Pyrus.",
		})
	}
	if cfg.Pyrus.ClientNameFieldID == 0 {
		res.Errors = append(res.Errors, FieldError{
			Field:   "pyrus.client_name_field_id",
			Message: "Не указан Field ID поля наименования клиента.",
		})
	}
	if cfg.Pyrus.AmountFieldID == 0 {
		res.Errors = append(res.Errors, FieldError{
			Field:   "pyrus.amount_field_id",
			Message: "Не указан Field ID поля суммы.",
		})
	}
	if cfg.Pyrus.Login == "" || cfg.Pyrus.SecurityKey == "" {
		return
	}

	client := pyrus.NewClient(cfg.Pyrus.AuthURL, cfg.Pyrus.Login, cfg.Pyrus.SecurityKey)
	client.SetLogger(log)
	authCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := client.Authorize(authCtx); err != nil {
		res.Errors = append(res.Errors, FieldError{
			Field:   "pyrus.security_key",
			Message: "Не удалось авторизоваться в Pyrus. Проверьте логин бота и ключ безопасности. Подробности: " + shortErr(err),
		})
		return
	}

	if cfg.Pyrus.FormID == 0 {
		return
	}

	form, err := client.GetForm(authCtx, cfg.Pyrus.FormID)
	if err != nil {
		res.Errors = append(res.Errors, FieldError{
			Field:   "pyrus.form_id",
			Message: "Форма не найдена или у бота нет к ней доступа. Проверьте ID формы и права бота. Подробности: " + shortErr(err),
		})
		return
	}

	// Проверяем поле вложения в форме по числовому Field ID.
	attField, found := form.FindField(cfg.Pyrus.AttachmentFieldID)
	if !found {
		res.Errors = append(res.Errors, FieldError{
			Field:   "pyrus.attachment_field_id",
			Message: "Поле-вложение с указанным Field ID не найдено в форме. Проверьте правильность номера.",
		})
	} else if attField.Type != "" && !attachmentFieldTypes[strings.ToLower(attField.Type)] {
		res.Errors = append(res.Errors, FieldError{
			Field:   "pyrus.attachment_field_id",
			Message: "Поле найдено, но имеет тип «" + attField.Type + "», ожидался тип «Файл».",
		})
	}

	// Проверяем поле наименования клиента
	clientField, found := form.FindField(cfg.Pyrus.ClientNameFieldID)
	if !found {
		res.Errors = append(res.Errors, FieldError{
			Field:   "pyrus.client_name_field_id",
			Message: "Поле наименования клиента с указанным Field ID не найдено в форме. Проверьте правильность номера.",
		})
	} else if clientField.Type != "" && strings.ToLower(clientField.Type) != "text" {
		res.Errors = append(res.Errors, FieldError{
			Field:   "pyrus.client_name_field_id",
			Message: "Поле наименования клиента имеет тип «" + clientField.Type + "», ожидался тип «Текст».",
		})
	}

	// Проверяем поле суммы
	amountField, found := form.FindField(cfg.Pyrus.AmountFieldID)
	if !found {
		res.Errors = append(res.Errors, FieldError{
			Field:   "pyrus.amount_field_id",
			Message: "Поле суммы с указанным Field ID не найдено в форме. Проверьте правильность номера.",
		})
	} else if amountField.Type != "" && strings.ToLower(amountField.Type) != "money" && strings.ToLower(amountField.Type) != "number" {
		res.Errors = append(res.Errors, FieldError{
			Field:   "pyrus.amount_field_id",
			Message: "Поле суммы имеет тип «" + amountField.Type + "», ожидался тип «Деньги» или «Число».",
		})
	}

	if len(res.Errors) > 0 {
		return
	}

	res.PyrusOK = true
}

// shortErr укорачивает текст ошибки для показа в интерфейсе.
func shortErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
