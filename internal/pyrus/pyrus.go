package pyrus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"schyotovod/internal/trace"
	"strings"
	"time"
)

// debugLogger — минимальный интерфейс логгера, которого достаточно клиенту
// Pyrus, чтобы писать отладочные сообщения (полные тела API-запросов/ответов).
// Отдельный интерфейс (а не прямая зависимость от internal/logger.Logger)
// используется, чтобы не создавать циклических импортов и оставить пакет
// pyrus независимым от конкретной реализации логирования.
type debugLogger interface {
	Debug(format string, args ...any)
}

// Client — клиент Pyrus API v4.
type Client struct {
	authURL     string
	login       string
	securityKey string
	http        *http.Client
	log         debugLogger // может быть nil — тогда отладочное логирование отключено

	// Заполняются после успешной авторизации.
	accessToken string
	apiURL      string
	filesURL    string
	tokenAt     time.Time
}

// NewClient создаёт клиент. authURL — адрес авторизации (обычно
// https://accounts.pyrus.com/api/v4/auth).
func NewClient(authURL, login, securityKey string) *Client {
	if authURL == "" {
		authURL = "https://accounts.pyrus.com/api/v4/auth"
	}
	return &Client{
		authURL:     authURL,
		login:       login,
		securityKey: securityKey,
		http:        &http.Client{Timeout: 30 * time.Second},
	}
}

// SetLogger задаёт логгер для отладочного вывода тел запросов/ответов.
func (c *Client) SetLogger(log debugLogger) {
	c.log = log
}

// authRequest описывает параметры авторизации бота в Pyrus.
type authRequest struct {
	Login       string `json:"login"`
	SecurityKey string `json:"security_key"`
}

// authResponse — токен доступа и адреса серверов API/Files.
type authResponse struct {
	AccessToken string `json:"access_token"`
	APIURL      string `json:"api_url"`
	FilesURL    string `json:"files_url"`
}

// Authorize запрашивает новый токен доступа и сохраняет его.
func (c *Client) Authorize(ctx context.Context) error {
	reqBody := authRequest{Login: c.login, SecurityKey: c.securityKey}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.authURL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	c.debugRequest(req)
	started := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		c.recordStep(ctx, http.MethodPost, c.authURL, string(raw), 0, "", started, err)
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		c.recordStep(ctx, http.MethodPost, c.authURL, string(raw), resp.StatusCode, "", started, err)
		return err
	}
	c.debugResponse(resp.StatusCode, data)
	c.recordStep(ctx, http.MethodPost, c.authURL, string(raw), resp.StatusCode, string(data), started, nil)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("авторизация Pyrus отклонена: %s", describeError(resp.StatusCode, data))
	}

	var ar authResponse
	if err := json.Unmarshal(data, &ar); err != nil {
		return fmt.Errorf("ошибка разбора ответа авторизации: %w", err)
	}

	c.accessToken = ar.AccessToken
	c.apiURL = strings.TrimRight(ar.APIURL, "/")
	c.filesURL = strings.TrimRight(ar.FilesURL, "/")
	c.tokenAt = time.Now()
	return nil
}

// ensureAuthorized обновляет токен, если он отсутствует или старше 15 минут.
func (c *Client) ensureAuthorized(ctx context.Context) error {
	if c.accessToken == "" || time.Since(c.tokenAt) > 15*time.Minute {
		return c.Authorize(ctx)
	}
	return nil
}

// doWithReauth оборачивает вызов API Pyrus проверкой и обновлением токена.
// Если запрос упал с 401 Unauthorized — токен принудительно обновляется и запрос повторяется.
func (c *Client) doWithReauth(ctx context.Context, buildReq func() (*http.Request, error)) (*http.Response, []byte, error) {
	if err := c.ensureAuthorized(ctx); err != nil {
		return nil, nil, err
	}

	req, err := buildReq()
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.accessToken)

	reqBody := string(readRequestBodyForDebug(req))
	c.debugRequest(req)
	started := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		c.recordStep(ctx, req.Method, req.URL.String(), reqBody, 0, "", started, err)
		return nil, nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		c.recordStep(ctx, req.Method, req.URL.String(), reqBody, resp.StatusCode, "", started, err)
		return nil, nil, err
	}
	c.debugResponse(resp.StatusCode, data)
	c.recordStep(ctx, req.Method, req.URL.String(), reqBody, resp.StatusCode, string(data), started, nil)

	if resp.StatusCode == http.StatusUnauthorized {
		// Токен протух (например, отозван на сервере) — пробуем авторизоваться заново и повторить.
		if err := c.Authorize(ctx); err != nil {
			return nil, nil, err
		}
		req2, err := buildReq()
		if err != nil {
			return nil, nil, err
		}
		req2.Header.Set("Authorization", "Bearer "+c.accessToken)
		req2.Header.Set("Content-Type", req.Header.Get("Content-Type"))

		reqBody2 := string(readRequestBodyForDebug(req2))
		c.debugRequest(req2)
		started2 := time.Now()
		resp2, err := c.http.Do(req2)
		if err != nil {
			c.recordStep(ctx, req2.Method, req2.URL.String(), reqBody2, 0, "", started2, err)
			return nil, nil, err
		}
		defer resp2.Body.Close()
		data2, err := io.ReadAll(resp2.Body)
		if err != nil {
			c.recordStep(ctx, req2.Method, req2.URL.String(), reqBody2, resp2.StatusCode, "", started2, err)
			return nil, nil, err
		}
		c.debugResponse(resp2.StatusCode, data2)
		c.recordStep(ctx, req2.Method, req2.URL.String(), reqBody2, resp2.StatusCode, string(data2), started2, nil)
		return resp2, data2, nil
	}
	return resp, data, nil
}

// recordStep фиксирует один HTTP-шаг обращения к Pyrus в trace.Collector из
// контекста (если он есть). Тела маскируются внутри trace.Collector.Record.
func (c *Client) recordStep(ctx context.Context, method, endpoint, reqBody string, status int, respBody string, started time.Time, callErr error) {
	col := trace.FromContext(ctx)
	if col == nil {
		return
	}
	step := trace.Step{
		Kind:         trace.KindPyrus,
		Method:       method,
		Endpoint:     endpoint,
		RequestBody:  reqBody,
		StatusCode:   status,
		ResponseBody: respBody,
		DurationMs:   time.Since(started).Milliseconds(),
	}
	if callErr != nil {
		step.Error = callErr.Error()
	}
	col.Record(step)
}

// readRequestBodyForDebug безопасно извлекает тело запроса для логирования,
// не потребляя фактический поток, который будет отправлен http.Client.Do.
// Работает благодаря req.GetBody, который net/http автоматически заполняет
// для тел на основе *bytes.Reader/*bytes.Buffer/*strings.Reader (в этом
// пакете все запросы строятся именно так). Если GetBody недоступен (например,
// для multipart-запросов с потоковым телом) — возвращает заглушку.
func readRequestBodyForDebug(req *http.Request) []byte {
	if req == nil || req.GetBody == nil {
		return []byte("<тело недоступно для просмотра>")
	}
	rc, err := req.GetBody()
	if err != nil {
		return []byte("<не удалось прочитать тело запроса>")
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return []byte("<ошибка чтения тела запроса>")
	}
	// Для multipart-запросов (загрузка файла) содержимое файла может быть
	// большим и бинарным — не имеет смысла логировать его целиком.
	if ct := req.Header.Get("Content-Type"); strings.HasPrefix(ct, "multipart/form-data") {
		return fmt.Appendf(nil, "<multipart-запрос, %d байт>", len(data))
	}
	return data
}

// uploadResponse — ответ метода files/upload.
type uploadResponse struct {
	GUID    string `json:"guid"`
	MD5Hash string `json:"md5_hash"`
}

// UploadFile загружает один файл и возвращает его guid.
// filename — имя файла, content — его содержимое.
func (c *Client) UploadFile(ctx context.Context, filename string, content []byte) (string, error) {
	base := c.filesURL
	if base == "" {
		base = c.apiURL
	}
	// Pyrus files_url возвращает хост вида "https://files.pyrus.com/".
	// Метод API v4 для загрузки файлов находится по пути: /v4/files/upload.
	// Добавим префикс /v4, если base не содержит его.
	var uploadURL string
	if !strings.Contains(base, "/v4") {
		uploadURL = strings.TrimRight(base, "/") + "/v4/files/upload"
	} else {
		uploadURL = strings.TrimRight(base, "/") + "/files/upload"
	}

	build := func() (*http.Request, error) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		part, err := w.CreateFormFile("file", filename)
		if err != nil {
			return nil, fmt.Errorf("не удалось сформировать тело загрузки файла: %w", err)
		}
		if _, err := part.Write(content); err != nil {
			return nil, fmt.Errorf("не удалось записать содержимое файла в запрос: %w", err)
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, &buf)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", w.FormDataContentType())
		return req, nil
	}

	resp, data, err := c.doWithReauth(ctx, build)
	if err != nil {
		return "", fmt.Errorf("не удалось загрузить файл в Pyrus: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Pyrus отклонил загрузку файла %q: %s", filename, describeError(resp.StatusCode, data))
	}

	var ur uploadResponse
	if err := json.Unmarshal(data, &ur); err != nil {
		return "", fmt.Errorf("не удалось разобрать ответ загрузки файла: %w", err)
	}
	if ur.GUID == "" {
		return "", fmt.Errorf("Pyrus не вернул идентификатор загруженного файла")
	}
	return ur.GUID, nil
}

// attachmentValue описывает значение поля-вложения при обновлении задачи.
type attachmentValue struct {
	GUID string `json:"guid"`
}

// UpdateTaskInvoice обновляет вложение и сумму счёта в задаче, добавляя комментарий.
func (c *Client) UpdateTaskInvoice(ctx context.Context, taskID int, fileFieldID int, guids []string, amountFieldID int, amount float64, comment string) error {
	type fieldUpdate struct {
		ID    int `json:"id"`
		Value any `json:"value"`
	}
	var updates []fieldUpdate

	// Если переданы guids файлов, формируем вложения.
	if len(guids) > 0 {
		var vals []attachmentValue
		for _, guid := range guids {
			vals = append(vals, attachmentValue{GUID: guid})
		}
		updates = append(updates, fieldUpdate{ID: fileFieldID, Value: vals})
	}

	// Поле суммы.
	updates = append(updates, fieldUpdate{ID: amountFieldID, Value: amount})

	reqBody := map[string]any{
		"text":          comment,
		"field_updates": updates,
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/tasks/%d/comments", c.apiURL, taskID)
	build := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	}

	resp, data, err := c.doWithReauth(ctx, build)
	if err != nil {
		return fmt.Errorf("не удалось отправить комментарий в Pyrus: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Pyrus отклонил отправку комментария в задачу %d: %s", taskID, describeError(resp.StatusCode, data))
	}
	return nil
}

// AddComment добавляет простой текстовый комментарий в задачу (используется для логирования ошибок).
func (c *Client) AddComment(ctx context.Context, taskID int, text string) error {
	reqBody := map[string]any{"text": text}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/tasks/%d/comments", c.apiURL, taskID)
	build := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	}

	resp, data, err := c.doWithReauth(ctx, build)
	if err != nil {
		return fmt.Errorf("не удалось отправить комментарий: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Pyrus отклонил отправку комментария: %s", describeError(resp.StatusCode, data))
	}
	return nil
}

// FormFieldOption описывает один вариант выбора поля типа multiple_choice.
// Каждый вариант может содержать собственные вложенные поля (info.options[].fields),
// которые становятся видимыми в задаче только при выборе этого варианта.
type FormFieldOption struct {
	ChoiceID    int         `json:"choice_id"`
	ChoiceValue string      `json:"choice_value"`
	Fields      []FormField `json:"fields,omitempty"`
	Deleted     bool        `json:"deleted,omitempty"`
}

// FormFieldInfo — дополнительная информация о поле формы. Важно: Pyrus API v4
// возвращает вложенные поля составных типов НЕ в top-level ключе "fields",
// а внутри "info":
//   - для типа "table"           — info.columns (столбцы таблицы);
//   - для типа "title"           — info.fields (поля внутри заголовка/блока);
//   - для типа "multiple_choice" — info.options[].fields (поля, зависящие от варианта выбора).
//
// Без разбора этих ключей FindFieldByID не находит поля, физически лежащие
// внутри таблицы/заголовка/варианта выбора, что приводит к ложной ошибке
// «поле не найдено в форме», хотя поле в форме реально существует.
type FormFieldInfo struct {
	Columns []FormField       `json:"columns,omitempty"`
	Fields  []FormField       `json:"fields,omitempty"`
	Options []FormFieldOption `json:"options,omitempty"`
}

// FormField описывает структуру поля формы Pyrus.
type FormField struct {
	ID     int            `json:"id"`
	Type   string         `json:"type"`
	Name   string         `json:"name"`
	Fields []FormField    `json:"fields,omitempty"` // для вложенных структур верхнего уровня (редко встречается)
	Info   *FormFieldInfo `json:"info,omitempty"`
}

// FormInfo содержит метаданные формы.
type FormInfo struct {
	ID     int         `json:"id"`
	Name   string      `json:"name"`
	Fields []FormField `json:"fields"`
}

// GetForm возвращает метаданные (поля) формы Pyrus.
func (c *Client) GetForm(ctx context.Context, formID int) (*FormInfo, error) {
	url := fmt.Sprintf("%s/forms/%d", c.apiURL, formID)
	build := func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	}

	resp, data, err := c.doWithReauth(ctx, build)
	if err != nil {
		return nil, fmt.Errorf("не удалось получить форму %d: %w", formID, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Pyrus вернул ошибку для формы %d: %s", formID, describeError(resp.StatusCode, data))
	}

	var fi FormInfo
	if err := json.Unmarshal(data, &fi); err != nil {
		return nil, fmt.Errorf("ошибка разбора ответа формы: %w", err)
	}
	return &fi, nil
}

// FindField ищет поле по ID в форме (включая рекурсивный поиск во вложенных полях).
func (f *FormInfo) FindField(id int) (FormField, bool) {
	return FindFieldByID(f.Fields, id)
}

// FindFieldByID рекурсивно ищет поле по ID в слайсе полей формы.
// Проверяет не только top-level "fields", но и вложенные поля составных
// типов, которые Pyrus API v4 возвращает внутри "info" (columns таблицы,
// fields заголовка/title, fields каждого варианта multiple_choice) — см.
// комментарий к FormFieldInfo.
func FindFieldByID(fields []FormField, id int) (FormField, bool) {
	for _, f := range fields {
		if f.ID == id {
			return f, true
		}
		if len(f.Fields) > 0 {
			if sf, found := FindFieldByID(f.Fields, id); found {
				return sf, true
			}
		}
		if f.Info != nil {
			if len(f.Info.Columns) > 0 {
				if sf, found := FindFieldByID(f.Info.Columns, id); found {
					return sf, true
				}
			}
			if len(f.Info.Fields) > 0 {
				if sf, found := FindFieldByID(f.Info.Fields, id); found {
					return sf, true
				}
			}
			for _, opt := range f.Info.Options {
				if len(opt.Fields) > 0 {
					if sf, found := FindFieldByID(opt.Fields, id); found {
						return sf, true
					}
				}
			}
		}
	}
	return FormField{}, false
}

// TaskField описывает поле задачи/формы (для проверки существования и типа поля).
type TaskField struct {
	ID    int    `json:"id"`
	Type  string `json:"type"`
	Value any    `json:"value"`
}

// TaskInfo — упрощённые сведения о задаче из реестра.
type TaskInfo struct {
	ID         int         `json:"id"`
	CreateDate time.Time   `json:"create_date"`
	CloseDate  *time.Time  `json:"close_date,omitempty"`
	Fields     []TaskField `json:"fields"`
}

// tasksResponse — ответ реестра задач.
type tasksResponse struct {
	Tasks []TaskInfo `json:"tasks"`
}

// FindTasksByForm возвращает открытые задачи по форме, изменённые после указанной даты.
func (c *Client) FindTasksByForm(ctx context.Context, formID int, modifiedAfter time.Time) ([]TaskInfo, error) {
	url := fmt.Sprintf("%s/projects", c.apiURL) // В Pyrus API v4 реестр задач формы получается через GET /projects с фильтрами
	// На самом деле метод получения задач по форме: GET /v4/forms/{formId}/register
	url = fmt.Sprintf("%s/forms/%d/register", c.apiURL, formID)

	if !modifiedAfter.IsZero() {
		// Pyrus принимает дату в формате ISO 8601 UTC (например, "2025-07-20T15:00:00Z").
		url += "?modified_after=" + modifiedAfter.UTC().Format("2006-01-02T15:04:05Z")
	}

	build := func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	}

	resp, data, err := c.doWithReauth(ctx, build)
	if err != nil {
		return nil, fmt.Errorf("не удалось получить задачи по форме %d: %w", formID, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Pyrus вернул ошибку при получении списка задач: %s", describeError(resp.StatusCode, data))
	}

	var tr tasksResponse
	if err := json.Unmarshal(data, &tr); err != nil {
		return nil, fmt.Errorf("ошибка разбора списка задач формы: %w", err)
	}
	return tr.Tasks, nil
}

// TaskDetails — детальная информация об одной задаче.
type TaskDetails struct {
	ID     int         `json:"id"`
	Fields []TaskField `json:"fields"`
}

// GetTask запрашивает детальную информацию по одной задаче.
func (c *Client) GetTask(ctx context.Context, taskID int) (*TaskDetails, error) {
	url := fmt.Sprintf("%s/tasks/%d", c.apiURL, taskID)
	build := func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	}

	resp, data, err := c.doWithReauth(ctx, build)
	if err != nil {
		return nil, fmt.Errorf("не удалось получить задачу %d: %w", taskID, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Pyrus вернул ошибку для задачи %d: %s", taskID, describeError(resp.StatusCode, data))
	}

	// Структура ответа Pyrus API v4 для задачи содержит объект "task".
	var wrapper struct {
		Task TaskDetails `json:"task"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return nil, fmt.Errorf("ошибка разбора деталей задачи: %w", err)
	}
	return &wrapper.Task, nil
}

// FindField ищет поле по ID в деталях задачи.
func (t *TaskDetails) FindField(id int) (TaskField, bool) {
	return FindTaskFieldByID(t.Fields, id)
}

// FindTaskFieldByID рекурсивно ищет поле по ID в слайсе полей задачи.
func FindTaskFieldByID(fields []TaskField, id int) (TaskField, bool) {
	for _, f := range fields {
		if f.ID == id {
			return f, true
		}
		// В Pyrus поля могут возвращаться как вложенные структуры (например, внутри таблицы).
		// В API v4 это обычно описывается объектом в Value.
		if f.Value != nil {
			if valMap, ok := f.Value.(map[string]any); ok {
				if subFieldsVal, exists := valMap["fields"]; exists {
					if subFieldsRaw, err := json.Marshal(subFieldsVal); err == nil {
						var subFields []TaskField
						if json.Unmarshal(subFieldsRaw, &subFields) == nil {
							if sf, found := FindTaskFieldByID(subFields, id); found {
								return sf, true
							}
						}
					}
				}
			} else if valSlice, ok := f.Value.([]any); ok {
				// Обработка табличных строк.
				for _, row := range valSlice {
					if rowMap, ok := row.(map[string]any); ok {
						if cellsVal, exists := rowMap["cells"]; exists {
							if cellsRaw, err := json.Marshal(cellsVal); err == nil {
								var cells []TaskField
								if json.Unmarshal(cellsRaw, &cells) == nil {
									if sf, found := FindTaskFieldByID(cells, id); found {
										return sf, true
									}
								}
							}
						}
					}
				}
			}
		}
	}
	return TaskField{}, false
}

// describeError собирает понятный текст ошибки из ответа Pyrus.
func describeError(statusCode int, body []byte) string {
	var er struct {
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
	}
	if json.Unmarshal(body, &er) == nil && er.Error != "" {
		return fmt.Sprintf("%s (код: %s, HTTP %d)", er.Error, er.ErrorCode, statusCode)
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 150 {
		s = s[:150] + "…"
	}
	if s == "" {
		s = "<пустой ответ>"
	}
	return fmt.Sprintf("%s (HTTP %d)", s, statusCode)
}

func (c *Client) debugRequest(req *http.Request) {
	if c.log == nil {
		return
	}
	body := readRequestBodyForDebug(req)
	// Маскируем секреты (security_key, Authorization) перед записью в лог.
	safeBody := trace.Sanitize(body)
	safeHeaders := trace.Sanitize([]byte(fmt.Sprintf("%v", req.Header)))
	c.log.Debug("--> %s %s\nHeaders: %s\nBody: %s", req.Method, req.URL.String(), string(safeHeaders), string(safeBody))
}

func (c *Client) debugResponse(statusCode int, body []byte) {
	if c.log == nil {
		return
	}
	// Для логов безопасности маскируем access_token в JSON-ответах авторизации
	var ar authResponse
	loggedBody := body
	if json.Unmarshal(body, &ar) == nil && ar.AccessToken != "" {
		masked := ar
		masked.AccessToken = ar.AccessToken[:4] + "***" + ar.AccessToken[len(ar.AccessToken)-4:]
		loggedBody, _ = json.Marshal(masked)
	}
	c.log.Debug("<-- HTTP %d\nBody: %s", statusCode, string(loggedBody))
}
