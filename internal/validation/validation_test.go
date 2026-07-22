package validation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"schyotovod/internal/config"
)

type mockLogger struct{}

func (mockLogger) Debug(format string, args ...any) {}

func TestShortErr(t *testing.T) {
	if shortErr(nil) != "" {
		t.Error("shortErr(nil) should be empty string")
	}

	short := strings.Repeat("a", 100)
	if shortErr(stdError{msg: short}) != short {
		t.Errorf("expected %q, got %q", short, shortErr(stdError{msg: short}))
	}

	long := strings.Repeat("a", 400)
	expected := strings.Repeat("a", 300) + "…"
	if shortErr(stdError{msg: long}) != expected {
		t.Errorf("expected length 301, got %d", len(shortErr(stdError{msg: long})))
	}
}

type stdError struct {
	msg string
}

func (e stdError) Error() string {
	return e.msg
}

func TestValidateEmptyConfigs(t *testing.T) {
	cfg := config.Default() // Все ключевые поля пустые/нулевые
	ctx := context.Background()

	res := Validate(ctx, cfg, mockLogger{})
	if res.OK() {
		t.Error("Validation should fail on empty config")
	}

	hasGmailErrors := false
	hasPyrusErrors := false
	for _, err := range res.Errors {
		if strings.HasPrefix(err.Field, "gmail.") || err.Field == "filter.sender_email" {
			hasGmailErrors = true
		}
		if strings.HasPrefix(err.Field, "pyrus.") {
			hasPyrusErrors = true
		}
	}

	if !hasGmailErrors {
		t.Error("expected Gmail validation errors")
	}
	if !hasPyrusErrors {
		t.Error("expected Pyrus validation errors")
	}
}

func TestValidatePyrus(t *testing.T) {
	// API-сервер Pyrus
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "test-token",
				"api_url":      "http://" + r.Host,
				"files_url":    "http://" + r.Host,
			})
		case strings.Contains(r.URL.Path, "/forms/"):
			// Эмулируем структуру формы
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 111,
				"fields": []map[string]any{
					{"id": 222, "type": "attachment", "name": "FileField"},
					{"id": 333, "type": "text", "name": "ClientField"},
					{"id": 444, "type": "money", "name": "MoneyField"},
					{"id": 555, "type": "multiple_choice", "name": "WrongField"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	ctx := context.Background()

	t.Run("Valid Pyrus config", func(t *testing.T) {
		cfg := config.Default()
		// Настраиваем верные поля
		cfg.Pyrus.Login = "bot@x"
		cfg.Pyrus.SecurityKey = "secret"
		cfg.Pyrus.AuthURL = srv.URL + "/auth"
		cfg.Pyrus.FormID = 111
		cfg.Pyrus.AttachmentFieldID = 222
		cfg.Pyrus.ClientNameFieldID = 333
		cfg.Pyrus.AmountFieldID = 444

		res := Result{}
		validatePyrus(ctx, cfg, &res, mockLogger{})
		if res.PyrusOK != true {
			t.Errorf("expected PyrusOK to be true, got errors: %v", res.Errors)
		}

		// Не должно быть ошибок по Pyrus
		for _, err := range res.Errors {
			if strings.HasPrefix(err.Field, "pyrus.") {
				t.Errorf("unexpected Pyrus error: %s - %s", err.Field, err.Message)
			}
		}
	})

	t.Run("Pyrus wrong field types", func(t *testing.T) {
		cfg := config.Default()
		cfg.Pyrus.Login = "bot@x"
		cfg.Pyrus.SecurityKey = "secret"
		cfg.Pyrus.AuthURL = srv.URL + "/auth"
		cfg.Pyrus.FormID = 111

		// Укажем неверные типы полей
		cfg.Pyrus.AttachmentFieldID = 555 // wrong type (multiple_choice)
		cfg.Pyrus.ClientNameFieldID = 222  // wrong type (attachment)
		cfg.Pyrus.AmountFieldID = 333      // wrong type (text)

		res := Result{}
		validatePyrus(ctx, cfg, &res, mockLogger{})

		if res.PyrusOK {
			t.Error("Pyrus validation should have failed")
		}

		errMap := make(map[string]string)
		for _, err := range res.Errors {
			errMap[err.Field] = err.Message
		}

		if !strings.Contains(errMap["pyrus.attachment_field_id"], "ожидался тип «Файл»") {
			t.Errorf("unexpected error message for attachment field: %s", errMap["pyrus.attachment_field_id"])
		}
		if !strings.Contains(errMap["pyrus.client_name_field_id"], "ожидался тип «Текст»") {
			t.Errorf("unexpected error message for client field: %s", errMap["pyrus.client_name_field_id"])
		}
		if !strings.Contains(errMap["pyrus.amount_field_id"], "ожидался тип «Деньги» или «Число»") {
			t.Errorf("unexpected error message for amount field: %s", errMap["pyrus.amount_field_id"])
		}
	})

	t.Run("Pyrus fields not found", func(t *testing.T) {
		cfg := config.Default()
		cfg.Pyrus.Login = "bot@x"
		cfg.Pyrus.SecurityKey = "secret"
		cfg.Pyrus.AuthURL = srv.URL + "/auth"
		cfg.Pyrus.FormID = 111

		// Укажем несуществующие ID полей
		cfg.Pyrus.AttachmentFieldID = 999
		cfg.Pyrus.ClientNameFieldID = 999
		cfg.Pyrus.AmountFieldID = 999

		res := Result{}
		validatePyrus(ctx, cfg, &res, mockLogger{})

		if res.PyrusOK {
			t.Error("Pyrus validation should have failed")
		}

		for _, err := range res.Errors {
			if !strings.Contains(err.Message, "не найдено в форме") {
				t.Errorf("expected field not found error, got: %s", err.Message)
			}
		}
	})
}
