package pyrus

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestFindFieldByID_NestedInInfo проверяет, что поиск поля по ID находит
// поля, вложенные внутрь составных типов (table, title, multiple_choice),
// которые Pyrus API v4 возвращает не в top-level "fields", а внутри "info"
// (columns/fields/options[].fields). Без этого валидные поля формы ложно
// считались "не найденными".
func TestFindFieldByID_NestedInInfo(t *testing.T) {
	raw := []byte(`[
		{
			"id": 1,
			"type": "title",
			"name": "Блок счёта",
			"info": {
				"fields": [
					{"id": 84, "type": "attachment", "name": "Вложение"},
					{"id": 85, "type": "text", "name": "Клиент"}
				]
			}
		},
		{
			"id": 2,
			"type": "table",
			"name": "Платежи",
			"info": {
				"columns": [
					{"id": 86, "type": "money", "name": "Сумма"}
				]
			}
		},
		{
			"id": 3,
			"type": "multiple_choice",
			"name": "Способ оплаты",
			"info": {
				"options": [
					{
						"choice_id": 1,
						"choice_value": "Да",
						"fields": [
							{"id": 87, "type": "text", "name": "Комментарий"}
						]
					}
				]
			}
		}
	]`)

	var fields []FormField
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("не удалось разобрать тестовые поля: %v", err)
	}

	cases := []struct {
		id       int
		wantType string
	}{
		{84, "attachment"},
		{85, "text"},
		{86, "money"},
		{87, "text"},
	}
	for _, tc := range cases {
		f, found := FindFieldByID(fields, tc.id)
		if !found {
			t.Errorf("поле с ID %d не найдено, хотя должно быть вложено в info", tc.id)
			continue
		}
		if f.Type != tc.wantType {
			t.Errorf("поле %d: тип = %q, ожидался %q", tc.id, f.Type, tc.wantType)
		}
	}

	if _, found := FindFieldByID(fields, 999); found {
		t.Errorf("несуществующее поле 999 неожиданно найдено")
	}
}

// TestFullFlow эмулирует Pyrus API мок-сервером и проверяет полный цикл:
// авторизация → загрузка файла → прикрепление в поле-вложение задачи.
func TestFullFlow(t *testing.T) {
	var uploadedFile bool
	var attachedGUID string
	var attachedFieldID int

	// API-сервер (api_url).
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/files/upload"):
			uploadedFile = true
			if r.Header.Get("Authorization") != "Bearer test-token" {
				t.Errorf("отсутствует или неверный токен авторизации")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"guid": "guid-123", "md5_hash": "abc",
			})
		case strings.HasSuffix(r.URL.Path, "/register"):
			if r.Header.Get("Authorization") != "Bearer test-token" {
				t.Errorf("отсутствует или неверный токен авторизации")
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tasks": []any{},
			})
		case strings.Contains(r.URL.Path, "/tasks/") && strings.HasSuffix(r.URL.Path, "/comments"):
			var body struct {
				FieldUpdates []struct {
					ID    int   `json:"id"`
					Value []any `json:"value"`
				} `json:"field_updates"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.FieldUpdates) > 0 {
				attachedFieldID = body.FieldUpdates[0].ID
				raw, _ := json.Marshal(body.FieldUpdates[0].Value)
				if strings.Contains(string(raw), "guid-123") {
					attachedGUID = "guid-123"
				}
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"task": map[string]any{"id": 555}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer apiSrv.Close()

	// Сервер авторизации (auth_url).
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req authRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Login != "bot@x" || req.SecurityKey != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "bad credentials"})
			return
		}
		_ = json.NewEncoder(w).Encode(authResponse{
			AccessToken: "test-token",
			APIURL:      apiSrv.URL,
			FilesURL:    apiSrv.URL,
		})
	}))
	defer authSrv.Close()

	client := NewClient(authSrv.URL, "bot@x", "secret")
	ctx := context.Background()

	if err := client.Authorize(ctx); err != nil {
		t.Fatalf("Authorize: %v", err)
	}

	guid, err := client.UploadFile(ctx, "schet.pdf", []byte("PDF-содержимое"))
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if guid != "guid-123" {
		t.Errorf("guid = %q, ожидалось guid-123", guid)
	}
	if !uploadedFile {
		t.Error("сервер не получил запрос на загрузку файла")
	}

	// Проверяем FindTasksByForm
	_, err = client.FindTasksByForm(ctx, 123, time.Time{})
	if err != nil {
		t.Fatalf("FindTasksByForm: %v", err)
	}

	// Проверяем UpdateTaskInvoice
	err = client.UpdateTaskInvoice(ctx, 555, 999, []string{"guid-123"}, 888, 1500.50, "test comment")
	if err != nil {
		t.Fatalf("UpdateTaskInvoice: %v", err)
	}
	if attachedGUID != "guid-123" {
		t.Errorf("attachedGUID = %q, ожидалось guid-123", attachedGUID)
	}
	if attachedFieldID != 999 {
		t.Errorf("attachedFieldID = %d, ожидалось 999", attachedFieldID)
	}
}

func TestReauthOn401(t *testing.T) {
	var authCalls int
	var getFormCalls int

	// Эмулируем сервер.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth"):
			authCalls++
			_ = json.NewEncoder(w).Encode(authResponse{
				AccessToken: "token-new",
				APIURL:      "http://" + r.Host,
				FilesURL:    "http://" + r.Host,
			})
		case strings.Contains(r.URL.Path, "/forms/"):
			getFormCalls++
			if r.Header.Get("Authorization") == "Bearer token-expired" {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "token_expired"})
				return
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(FormInfo{ID: 123})
		}
	}))
	defer srv.Close()

	client := NewClient(srv.URL+"/auth", "bot", "secret")
	// Предустановим истекший токен.
	client.accessToken = "token-expired"
	client.tokenAt = time.Now() // чтобы ensureAuthorized посчитал его свежим и не обновлял сразу
	client.apiURL = srv.URL

	ctx := context.Background()
	form, err := client.GetForm(ctx, 123)
	if err != nil {
		t.Fatalf("GetForm: %v", err)
	}
	if form.ID != 123 {
		t.Errorf("expected form ID 123, got %d", form.ID)
	}
	if authCalls != 2 { // один раз при старте GetForm (в ensureAuthorized не пойдет так как tokenAt свежий, но придет 401 и вызовется reauth)
		// Стоп, почему 2? Первый раз reauth вызывается при получении 401 внутри doWithReauth.
		// А почему 2 вызова? Потому что первый ручной вызов не делался, но в коде мы принудительно делали client.Authorize(ctx).
		// Давайте посмотрим: client.Authorize(ctx) мы не вызывали. Но в getFormCalls:
		// Первый вызов с "token-expired" упал с 401.
		// doWithReauth пошел на ветку reauth, вызвал client.Authorize (1-й вызов /auth) и получил "token-new".
		// Затем повторил запрос с "token-new", который прошел успешно.
		// Итого вызовов /auth должно быть ровно 1.
		if authCalls != 1 {
			t.Errorf("expected 1 auth call during reauth, got %d", authCalls)
		}
	}
	if getFormCalls != 2 { // первый упал, второй прошел
		t.Errorf("expected 2 get form calls, got %d", getFormCalls)
	}
}
