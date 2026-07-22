package trace

import (
	"context"
	"strings"
	"testing"
)

func TestSanitize(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		mustHide string // подстрока, которой НЕ должно быть в результате
	}{
		{
			name:     "security_key в JSON",
			in:       `{"login":"bot@x","security_key":"SUPERSECRET123"}`,
			mustHide: "SUPERSECRET123",
		},
		{
			name:     "access_token в ответе",
			in:       `{"access_token":"abcdef123456","api_url":"https://api"}`,
			mustHide: "abcdef123456",
		},
		{
			name:     "app_password в JSON",
			in:       `{"app_password":"qwertyuiop"}`,
			mustHide: "qwertyuiop",
		},
		{
			name:     "Authorization Bearer в заголовках",
			in:       `Headers: map[Authorization:[Bearer tok_topsecret]]`,
			mustHide: "tok_topsecret",
		},
		{
			name:     "IMAP LOGIN пароль",
			in:       `LOGIN user@gmail.com appPassSecret`,
			mustHide: "appPassSecret",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(Sanitize([]byte(tt.in)))
			if strings.Contains(got, tt.mustHide) {
				t.Errorf("Sanitize не скрыл секрет: %q всё ещё в %q", tt.mustHide, got)
			}
			if !strings.Contains(got, "***") {
				t.Errorf("Sanitize не вставил маску ***: %q", got)
			}
		})
	}
}

func TestCollectorRecordAndContext(t *testing.T) {
	c := NewCollector()
	ctx := WithCollector(context.Background(), c)

	got := FromContext(ctx)
	if got != c {
		t.Fatal("FromContext вернул не тот коллектор")
	}

	c.SetStage("авторизация_pyrus")
	c.Record(Step{
		Kind:        KindPyrus,
		Method:      "POST",
		Endpoint:    "https://api/auth",
		RequestBody: `{"security_key":"SECRET"}`,
	})
	c.Record(Step{Kind: KindIMAP, Method: "SEARCH", Endpoint: "INBOX"})

	steps := c.Steps()
	if len(steps) != 2 {
		t.Fatalf("ожидалось 2 шага, получено %d", len(steps))
	}
	if steps[0].Seq != 1 || steps[1].Seq != 2 {
		t.Errorf("Seq проставлен неверно: %d, %d", steps[0].Seq, steps[1].Seq)
	}
	if steps[0].Stage != "авторизация_pyrus" {
		t.Errorf("Stage не подставлен из SetStage: %q", steps[0].Stage)
	}
	if strings.Contains(steps[0].RequestBody, "SECRET") {
		t.Errorf("тело запроса не замаскировано: %q", steps[0].RequestBody)
	}
}

func TestNilCollectorSafe(t *testing.T) {
	var c *Collector
	// Не должно паниковать.
	c.SetStage("x")
	c.Record(Step{Method: "GET"})
	if c.Steps() != nil {
		t.Error("Steps() на nil-коллекторе должен возвращать nil")
	}
	if FromContext(context.Background()) != nil {
		t.Error("FromContext без коллектора должен возвращать nil")
	}
}
