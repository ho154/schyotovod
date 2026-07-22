// Package web реализует веб-панель управления (админку) сервиса Schyotovod.
// Все тексты — на русском. Аутентификация через bcrypt + cookie-сессии.
package web

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"schyotovod/internal/auth"
	"schyotovod/internal/config"
	"schyotovod/internal/gmail"
	"schyotovod/internal/i18n"
	"schyotovod/internal/invoice"
	"schyotovod/internal/logger"
	"schyotovod/internal/pyrus"
	"schyotovod/internal/updater"
	"schyotovod/internal/validation"
	"schyotovod/internal/version"
	"schyotovod/internal/watcher"
)

//go:embed templates/*.html
var templatesFS embed.FS

const sessionCookie = "schyotovod_session"

var attachmentFieldTypes = map[string]bool{
	"attachment": true,
	"file":       true,
	"files":      true,
}

// Checker выполняет разовую проверку почты (реализуется watcher).
type Checker interface {
	CheckNow() (int, error)
}

// UpdateApplier применяет обновление (реализуется в main/updater-обвязке).
type UpdateApplier interface {
	CheckForUpdate(ctx context.Context) (*updater.Release, bool, error)
	ApplyUpdate(ctx context.Context, rel *updater.Release) error
	LastUpdateError() string
}

// Server — веб-сервер админки.
type Server struct {
	cfgMgr    *config.Manager
	log       *logger.Logger
	sessions  *auth.SessionStore
	checker   Checker
	applier   UpdateApplier
	templates map[string]*template.Template
}

// New создаёт веб-сервер.
func New(cfgMgr *config.Manager, log *logger.Logger, checker Checker, applier UpdateApplier) (*Server, error) {
	s := &Server{
		cfgMgr:    cfgMgr,
		log:       log,
		sessions:  auth.NewSessionStore(),
		checker:   checker,
		applier:   applier,
		templates: make(map[string]*template.Template),
	}

	files, err := templatesFS.ReadDir("templates")
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать каталог встроенных шаблонов: %w", err)
	}

	for _, file := range files {
		name := file.Name()
		if name == "base.html" || file.IsDir() {
			continue
		}
		tmpl := template.New(name).Funcs(template.FuncMap{
			"F": s.fieldData,
		})
		tmpl, err = tmpl.ParseFS(templatesFS, "templates/base.html", "templates/"+name)
		if err != nil {
			return nil, fmt.Errorf("не удалось загрузить шаблон %s: %w", name, err)
		}
		s.templates[name] = tmpl
	}

	return s, nil
}

// Handler возвращает http.Handler со всеми маршрутами.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Публичные маршруты.
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)

	// Защищённые маршруты.
	mux.HandleFunc("/", s.auth(s.handleIndex))
	mux.HandleFunc("/settings", s.auth(s.handleSettings))
	mux.HandleFunc("/change-password", s.auth(s.handleChangePassword))
	mux.HandleFunc("/check-now", s.auth(s.handleCheckNow))
	mux.HandleFunc("/logs", s.auth(s.handleLogs))
	mux.HandleFunc("/logs/stream", s.auth(s.handleLogsStream))
	mux.HandleFunc("/updates", s.auth(s.handleUpdates))
	mux.HandleFunc("/settings-update", s.auth(s.handleSettingsUpdate))
	mux.HandleFunc("/check-updates", s.auth(s.handleCheckUpdates))
	mux.HandleFunc("/apply-update", s.auth(s.handleApplyUpdate))

	// Действия из панели диагностики логов.
	mux.HandleFunc("/logs/action/check-email-access", s.auth(s.handleCheckEmailAccess))
	mux.HandleFunc("/logs/action/check-emails", s.auth(s.handleCheckEmails))
	mux.HandleFunc("/logs/action/check-pyrus-access", s.auth(s.handleCheckPyrusAccess))
	mux.HandleFunc("/logs/action/check-pyrus-fields", s.auth(s.handleCheckPyrusFields))
	mux.HandleFunc("/logs/action/reset-attempts", s.auth(s.handleResetAttempts))
	mux.HandleFunc("/logs/action/test-upload", s.auth(s.handleTestUpload))
	mux.HandleFunc("/logs/action/toggle-debug", s.auth(s.handleToggleDebug))

	return mux
}

// ---- Аутентификация ----

// auth оборачивает обработчик проверкой сессии.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || !s.sessions.Valid(c.Value) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && s.sessions.Valid(c.Value) {
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	if r.Method == http.MethodPost {
		login := strings.TrimSpace(r.FormValue("login"))
		password := r.FormValue("password")
		cfg := s.cfgMgr.Get()

		if login == cfg.Web.AdminLogin && cfg.Web.AdminPasswordHash != "" &&
			auth.CheckPassword(cfg.Web.AdminPasswordHash, password) {
			token, err := s.sessions.Create()
			if err != nil {
				s.renderLogin(w, "Внутренняя ошибка при создании сессии.")
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name: sessionCookie, Value: token, Path: "/",
				HttpOnly: true, SameSite: http.SameSiteLaxMode,
			})
			s.log.Info("Успешный вход в панель управления (логин: %s)", login)
			http.Redirect(w, r, "/settings", http.StatusSeeOther)
			return
		}
		s.log.Warn("Неудачная попытка входа в панель управления (логин: %s)", login)
		s.renderLogin(w, "Неверный логин или пароль.")
		return
	}
	s.renderLogin(w, "")
}

func (s *Server) renderLogin(w http.ResponseWriter, errMsg string) {
	s.render(w, "login.html", map[string]any{
		"Title": "Вход", "ShowNav": false, "Error": errMsg,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.Delete(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// ---- Данные полей для шаблона настроек ----

type fieldTemplateData struct {
	Key   string
	Type  string
	Value any
	Hint  i18n.FieldHint
	Err   string
}

func (s *Server) fieldData(page map[string]any, key, typ string, value any) fieldTemplateData {
	hint, _ := i18n.Hint(key)
	var errMsg string
	if errs, ok := page["FieldErrors"].(map[string]string); ok {
		errMsg = errs[key]
	}
	return fieldTemplateData{Key: key, Type: typ, Value: value, Hint: hint, Err: errMsg}
}

// ---- Настройки ----

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfgMgr.Get()

	page := map[string]any{
		"Title": "Настройки", "ShowNav": true, "Active": "settings",
		"Cfg":                cfg,
		"MustChangePassword": cfg.Web.MustChangePassword,
		"Timezones":          config.Timezones,
		"FieldErrors":        map[string]string{},
	}

	if r.Method == http.MethodPost {
		newCfg := s.parseSettingsForm(r, cfg)

		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		res := validation.Validate(ctx, newCfg, s.log)

		if !res.OK() {
			fieldErrors := map[string]string{}
			for _, e := range res.Errors {
				fieldErrors[e.Field] = e.Message
				s.log.Error("Проверка настроек: %s — %s", e.Field, e.Message)
			}
			page["Cfg"] = newCfg
			page["HasErrors"] = true
			page["FieldErrors"] = fieldErrors
			s.render(w, "settings.html", page)
			return
		}

		if err := s.cfgMgr.Set(newCfg); err != nil {
			page["HasErrors"] = true
			page["FieldErrors"] = map[string]string{"web.port": "Не удалось сохранить настройки: " + err.Error()}
			s.render(w, "settings.html", page)
			return
		}
		s.log.Info("Настройки успешно сохранены и проверены (Gmail: %s, Pyrus: форма %d)",
			maskEmail(newCfg.Gmail.Email), newCfg.Pyrus.FormID)
		s.log.SetRetention(newCfg.General.LogRetentionDays)
		s.log.SetDebug(newCfg.General.DebugLogging)
		if loc, err := newCfg.Location(); err == nil {
			s.log.SetLocation(loc)
		}
		page["Cfg"] = newCfg
		page["Saved"] = true
	}

	s.render(w, "settings.html", page)
}

func (s *Server) parseSettingsForm(r *http.Request, old config.Config) config.Config {
	c := old

	c.Gmail.Email = strings.TrimSpace(r.FormValue("gmail.email"))
	appPassword := r.FormValue("gmail.app_password")
	appPassword = strings.ReplaceAll(appPassword, " ", "")
	c.Gmail.AppPassword = strings.TrimSpace(appPassword)
	c.Gmail.IMAPHost = strings.TrimSpace(r.FormValue("gmail.imap_host"))
	c.Gmail.IMAPPort = atoiDefault(r.FormValue("gmail.imap_port"), old.Gmail.IMAPPort)

	c.Filter.SenderEmail = strings.TrimSpace(r.FormValue("filter.sender_email"))
	c.Filter.StartDay = atoiDefault(r.FormValue("filter.start_day"), old.Filter.StartDay)
	c.Filter.EndDay = atoiDefault(r.FormValue("filter.end_day"), old.Filter.EndDay)
	c.Filter.FallbackPollIntervalMinutes = atoiDefault(r.FormValue("filter.fallback_poll_interval_minutes"), old.Filter.FallbackPollIntervalMinutes)

	c.Pyrus.Login = strings.TrimSpace(r.FormValue("pyrus.login"))
	c.Pyrus.SecurityKey = strings.TrimSpace(r.FormValue("pyrus.security_key"))
	if formIDRaw := strings.TrimSpace(r.FormValue("pyrus.form_id")); formIDRaw != "" {
		c.Pyrus.FormID = config.ExtractFormID(formIDRaw)
	} else {
		c.Pyrus.FormID = 0
	}
	c.Pyrus.TaskID = atoiDefault(r.FormValue("pyrus.task_id"), 0)
	c.Pyrus.AttachmentFieldID = atoiDefault(r.FormValue("pyrus.attachment_field_id"), 0)
	c.Pyrus.ClientNameFieldID = atoiDefault(r.FormValue("pyrus.client_name_field_id"), 0)
	c.Pyrus.AmountFieldID = atoiDefault(r.FormValue("pyrus.amount_field_id"), 0)
	c.Pyrus.MaxUpdateAttempts = atoiDefault(r.FormValue("pyrus.max_update_attempts"), old.Pyrus.MaxUpdateAttempts)
	c.Pyrus.RetryIntervalMinutes = atoiDefault(r.FormValue("pyrus.retry_interval_minutes"), old.Pyrus.RetryIntervalMinutes)

	c.General.Timezone = strings.TrimSpace(r.FormValue("general.timezone"))
	c.General.LogRetentionDays = atoiDefault(r.FormValue("general.log_retention_days"), old.General.LogRetentionDays)
	c.General.DebugLogging = r.FormValue("general.debug_logging") != ""

	c.Web.Port = atoiDefault(r.FormValue("web.port"), old.Web.Port)

	return c
}

// ---- Смена пароля ----

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	cfg := s.cfgMgr.Get()
	cur := r.FormValue("current_password")
	newP := r.FormValue("new_password")
	newP2 := r.FormValue("new_password2")

	page := map[string]any{
		"Title": "Настройки", "ShowNav": true, "Active": "settings",
		"Cfg": cfg, "MustChangePassword": cfg.Web.MustChangePassword,
		"Timezones": config.Timezones, "FieldErrors": map[string]string{},
	}

	if !cfg.Web.MustChangePassword {
		if !auth.CheckPassword(cfg.Web.AdminPasswordHash, cur) {
			page["PasswordError"] = "Текущий пароль указан неверно."
			s.render(w, "settings.html", page)
			return
		}
	}
	if newP == "" || newP != newP2 {
		page["PasswordError"] = "Новый пароль пуст или пароли не совпадают."
		s.render(w, "settings.html", page)
		return
	}

	hash, err := auth.HashPassword(newP)
	if err != nil {
		page["PasswordError"] = "Не удалось сохранить новый пароль."
		s.render(w, "settings.html", page)
		return
	}
	cfg.Web.AdminPasswordHash = hash
	cfg.Web.MustChangePassword = false
	if err := s.cfgMgr.Set(cfg); err != nil {
		page["PasswordError"] = "Не удалось сохранить новый пароль: " + err.Error()
		s.render(w, "settings.html", page)
		return
	}
	s.log.Info("Пароль администратора изменён через панель управления")
	page["Cfg"] = cfg
	page["MustChangePassword"] = false
	page["PasswordChanged"] = true
	s.render(w, "settings.html", page)
}

// ---- Проверить сейчас ----

func (s *Server) handleCheckNow(w http.ResponseWriter, r *http.Request) {
	if s.checker == nil {
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	s.log.Info("Запущена ручная проверка почты («Проверить сейчас»)")
	n, err := s.checker.CheckNow()

	cfg := s.cfgMgr.Get()
	page := map[string]any{
		"Title": "Настройки", "ShowNav": true, "Active": "settings", "Cfg": cfg,
		"MustChangePassword": cfg.Web.MustChangePassword,
		"Timezones":          config.Timezones,
		"FieldErrors":        map[string]string{},
	}
	if err != nil {
		s.log.Error("Ручная проверка почты завершилась ошибкой: %v", err)
		page["HasErrors"] = true
		page["FieldErrors"] = map[string]string{"gmail.email": "Проверка не удалась: " + err.Error()}
	} else {
		s.log.Info("Ручная проверка почты завершена, обработано писем: %d", n)
		page["Saved"] = false
		page["CheckResult"] = fmt.Sprintf("Проверка завершена. Обработано новых писем: %d.", n)
	}
	if msg, ok := page["CheckResult"].(string); ok {
		page["Flash"] = msg
	}
	s.render(w, "settings.html", page)
}

// ---- Логи ----

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	dates, _ := s.log.ListLogDates()
	selectedDate := r.URL.Query().Get("date")
	if selectedDate == "" && len(dates) > 0 {
		selectedDate = dates[0]
	}
	level := r.URL.Query().Get("level")

	isToday := len(dates) > 0 && selectedDate == dates[0]

	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "all"
	}

	var content string
	if selectedDate != "" {
		content, _ = s.log.ReadLog(selectedDate, logger.Level(level))
		if isToday && scope == "session" {
			content = filterCurrentSessionLogs(content)
		}
	}

	cfg := s.cfgMgr.Get()
	s.render(w, "logs.html", map[string]any{
		"Title": "Логи", "ShowNav": true, "Active": "logs",
		"Dates": dates, "SelectedDate": selectedDate,
		"SelectedLevel": level, "Content": content,
		"IsToday":       isToday,
		"SelectedScope": scope,
		"DebugLogging":  cfg.General.DebugLogging,
	})
}

func filterCurrentSessionLogs(content string) string {
	lines := strings.Split(content, "\n")
	lastStartIdx := -1
	for i, line := range lines {
		if strings.Contains(line, "=== Запуск Schyotovod") {
			lastStartIdx = i
		}
	}
	if lastStartIdx != -1 {
		return strings.Join(lines[lastStartIdx:], "\n")
	}
	return content
}

func (s *Server) handleLogsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "потоковая передача не поддерживается", http.StatusInternalServerError)
		return
	}

	levelFilter := r.URL.Query().Get("level")

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := s.log.Subscribe()
	defer s.log.Unsubscribe(ch)

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	needle := ""
	if levelFilter != "" {
		needle = "[" + levelFilter + "]"
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case line, ok := <-ch:
			if !ok {
				return
			}
			if needle != "" && !strings.Contains(line, needle) {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(strings.TrimRight(line, "\n"), "\n", "\\n"))
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// ---- Обновления ----

func (s *Server) handleUpdates(w http.ResponseWriter, r *http.Request) {
	s.renderUpdates(w, r, "", "")
}

func (s *Server) renderUpdates(w http.ResponseWriter, r *http.Request, flash, flashType string) {
	cfg := s.cfgMgr.Get()
	page := map[string]any{
		"Title": "Обновления", "ShowNav": true, "Active": "updates",
		"Cfg": cfg, "CurrentVersion": version.Version,
		"Flash": flash, "FlashType": flashType,
	}
	if s.applier != nil {
		if lastErr := s.applier.LastUpdateError(); lastErr != "" {
			page["UpdateFailed"] = true
			page["UpdateError"] = lastErr
		}
	}
	s.render(w, "updates.html", page)
}

func (s *Server) handleSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/updates", http.StatusSeeOther)
		return
	}
	cfg := s.cfgMgr.Get()
	cfg.Update.AutoUpdate = r.FormValue("auto_update") != ""
	if t := strings.TrimSpace(r.FormValue("check_time")); t != "" {
		cfg.Update.CheckTime = t
	}
	cfg.Update.GitHubRepo = strings.TrimSpace(r.FormValue("github_repo"))
	if err := s.cfgMgr.Set(cfg); err != nil {
		s.renderUpdates(w, r, "Не удалось сохранить настройки обновлений: "+err.Error(), "err")
		return
	}
	s.log.Info("Настройки обновлений сохранены (автообновление: %v, время: %s)",
		cfg.Update.AutoUpdate, cfg.Update.CheckTime)
	s.renderUpdates(w, r, "Настройки обновлений сохранены.", "ok")
}

func (s *Server) handleCheckUpdates(w http.ResponseWriter, r *http.Request) {
	if s.applier == nil {
		s.renderUpdates(w, r, "Механизм обновлений недоступен.", "err")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	rel, newer, err := s.applier.CheckForUpdate(ctx)
	if err != nil {
		s.renderUpdates(w, r, "Не удалось проверить обновления: "+err.Error(), "err")
		return
	}

	cfg := s.cfgMgr.Get()
	page := map[string]any{
		"Title": "Обновления", "ShowNav": true, "Active": "updates",
		"Cfg": cfg, "CurrentVersion": version.Version,
	}
	if newer && rel != nil {
		page["NewAvailable"] = true
		page["LatestVersion"] = rel.TagName
		page["Changelog"] = rel.Body
	} else {
		page["Flash"] = "У вас установлена последняя версия."
		page["FlashType"] = "ok"
	}
	s.render(w, "updates.html", page)
}

func (s *Server) handleApplyUpdate(w http.ResponseWriter, r *http.Request) {
	if s.applier == nil {
		s.renderUpdates(w, r, "Механизм обновлений недоступен.", "err")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	rel, newer, err := s.applier.CheckForUpdate(ctx)
	if err != nil || !newer || rel == nil {
		s.renderUpdates(w, r, "Нет доступного обновления для установки.", "warn")
		return
	}
	s.log.Info("Запущена установка обновления до %s из панели управления", rel.TagName)
	if err := s.applier.ApplyUpdate(ctx, rel); err != nil {
		s.renderUpdates(w, r, "Обновление не удалось: "+err.Error(), "err")
		return
	}
	s.renderUpdates(w, r, "Обновление применяется, сервис перезапускается…", "ok")
}

// ---- Вспомогательное ----

func (s *Server) render(w http.ResponseWriter, name string, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	t, ok := s.templates[name]
	if !ok {
		s.log.Error("Шаблон %s не найден", name)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
		return
	}
	if err := t.Execute(w, data); err != nil {
		s.log.Error("Ошибка рендеринга страницы %s: %v", name, err)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
	}
}

func atoiDefault(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func maskEmail(email string) string {
	at := strings.Index(email, "@")
	if at <= 1 {
		return email
	}
	return email[:1] + "***" + email[at:]
}

// ---- Диагностические действия (AJAX) ----

func (s *Server) writeJSONResponse(w http.ResponseWriter, success bool, message string) {
	w.Header().Set("Content-Type", "application/json")
	status := "ok"
	if !success {
		status = "error"
	}
	fmt.Fprintf(w, `{"status":%q,"message":%q}`, status, message)
}

func (s *Server) handleCheckEmailAccess(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	cfg := s.cfgMgr.Get()
	dial := gmail.DialConfig{
		Host:        cfg.Gmail.IMAPHost,
		Port:        cfg.Gmail.IMAPPort,
		Email:       cfg.Gmail.Email,
		AppPassword: cfg.Gmail.AppPassword,
	}
	s.log.Info("Проверка доступа к почте для %s", cfg.Gmail.Email)
	client, err := gmail.Connect(dial, nil)
	if err != nil {
		s.log.Error("ОШИБКА ПОЧТЫ: Проверка доступа не удалась для %s: %v", cfg.Gmail.Email, err)
		s.writeJSONResponse(w, false, "Ошибка подключения: "+err.Error())
		return
	}
	defer client.Logout()
	defer client.Close()

	if err := gmail.SelectInbox(client); err != nil {
		s.log.Error("ОШИБКА ПОЧТЫ: Не удалось выбрать INBOX при проверке: %v", err)
		s.writeJSONResponse(w, false, "Ошибка выбора INBOX: "+err.Error())
		return
	}
	s.log.Info("Доступ к почте успешно проверен для %s", cfg.Gmail.Email)
	s.writeJSONResponse(w, true, "Доступ к почте успешно проверен. Подключение установлено.")
}

func (s *Server) handleCheckEmails(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	if s.checker == nil {
		s.writeJSONResponse(w, false, "Механизм проверки почты недоступен.")
		return
	}
	s.log.Info("Запуск ручной проверки писем из веб-панели")
	n, err := s.checker.CheckNow()
	if err != nil {
		s.log.Error("ОШИБКА ПОЧТЫ: Ручная проверка писем не удалась: %v", err)
		s.writeJSONResponse(w, false, "Ошибка при ручной проверке писем: "+err.Error())
		return
	}
	s.log.Info("Ручная проверка писем завершена. Обработано новых писем: %d", n)
	s.writeJSONResponse(w, true, fmt.Sprintf("Ручная проверка завершена. Обработано новых писем: %d.", n))
}

func (s *Server) handleCheckPyrusAccess(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	cfg := s.cfgMgr.Get()
	s.log.Info("Проверка авторизации Pyrus для %s", cfg.Pyrus.Login)
	client := pyrus.NewClient(cfg.Pyrus.AuthURL, cfg.Pyrus.Login, cfg.Pyrus.SecurityKey)
	client.SetLogger(s.log)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := client.Authorize(ctx); err != nil {
		s.log.Error("ОШИБКА PYRUS: Проверка авторизации не удалась для %s: %v", cfg.Pyrus.Login, err)
		s.writeJSONResponse(w, false, "Ошибка авторизации в Pyrus: "+err.Error())
		return
	}
	s.log.Info("Авторизация Pyrus успешно пройдена для %s", cfg.Pyrus.Login)
	s.writeJSONResponse(w, true, "Доступ к Pyrus успешно проверен. Авторизация пройдена.")
}

func (s *Server) handleCheckPyrusFields(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	cfg := s.cfgMgr.Get()
	s.log.Info("Проверка полей формы Pyrus %d", cfg.Pyrus.FormID)
	client := pyrus.NewClient(cfg.Pyrus.AuthURL, cfg.Pyrus.Login, cfg.Pyrus.SecurityKey)

	oldDebug := s.log.IsDebug()
	s.log.SetDebug(true)
	client.SetLogger(s.log)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := client.Authorize(ctx); err != nil {
		s.log.SetDebug(oldDebug)
		s.log.Error("ОШИБКА PYRUS: Авторизация не удалась: %v", err)
		s.writeJSONResponse(w, false, "Ошибка авторизации: "+err.Error())
		return
	}

	form, err := client.GetForm(ctx, cfg.Pyrus.FormID)
	s.log.SetDebug(oldDebug)
	if err != nil {
		s.log.Error("ОШИБКА PYRUS: Не удалось получить шаблон формы %d: %v", cfg.Pyrus.FormID, err)
		s.writeJSONResponse(w, false, "Ошибка получения формы: "+err.Error())
		return
	}

	var errors []string

	attField, found := form.FindField(cfg.Pyrus.AttachmentFieldID)
	if !found {
		errors = append(errors, fmt.Sprintf("Поле-вложение (ID %d) не найдено в форме", cfg.Pyrus.AttachmentFieldID))
	} else if attField.Type != "" && !attachmentFieldTypes[strings.ToLower(attField.Type)] {
		errors = append(errors, fmt.Sprintf("Поле-вложение (ID %d) имеет тип %q, ожидался файл", cfg.Pyrus.AttachmentFieldID, attField.Type))
	}

	clientField, found := form.FindField(cfg.Pyrus.ClientNameFieldID)
	if !found {
		errors = append(errors, fmt.Sprintf("Поле наименования клиента (ID %d) не найдено в форме", cfg.Pyrus.ClientNameFieldID))
	} else if clientField.Type != "" && strings.ToLower(clientField.Type) != "text" {
		errors = append(errors, fmt.Sprintf("Поле наименования клиента (ID %d) имеет тип %q, ожидался текст", cfg.Pyrus.ClientNameFieldID, clientField.Type))
	}

	amountField, found := form.FindField(cfg.Pyrus.AmountFieldID)
	if !found {
		errors = append(errors, fmt.Sprintf("Поле суммы (ID %d) не найдено в форме", cfg.Pyrus.AmountFieldID))
	} else if amountField.Type != "" && strings.ToLower(amountField.Type) != "money" && strings.ToLower(amountField.Type) != "number" {
		errors = append(errors, fmt.Sprintf("Поле суммы (ID %d) имеет тип %q, ожидались деньги или число", cfg.Pyrus.AmountFieldID, amountField.Type))
	}

	if len(errors) > 0 {
		s.writeJSONResponse(w, false, strings.Join(errors, "; "))
		return
	}

	s.writeJSONResponse(w, true, "Все поля Pyrus в форме успешно найдены и проверены.")
}

func (s *Server) handleResetAttempts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}

	type attemptsResetter interface {
		ResetAttempts()
	}
	if resetter, ok := s.checker.(attemptsResetter); ok {
		resetter.ResetAttempts()
	}

	n, err := s.checker.CheckNow()
	if err != nil {
		s.writeJSONResponse(w, false, "Таймеры сброшены, но при проверке почты возникла ошибка: "+err.Error())
		return
	}

	s.writeJSONResponse(w, true, fmt.Sprintf("Таймеры попыток сброшены. Повторная отправка счетов запущена. Обработано писем: %d.", n))
}

func (s *Server) handleTestUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}

	clientName := strings.TrimSpace(r.FormValue("client"))
	amountStr := strings.TrimSpace(r.FormValue("amount"))

	if clientName == "" || amountStr == "" {
		s.writeJSONResponse(w, false, "Все поля (клиент, файл счета, сумма) обязательны для заполнения.")
		return
	}

	amount, err := strconv.ParseFloat(strings.ReplaceAll(amountStr, ",", "."), 64)
	if err != nil {
		s.writeJSONResponse(w, false, "Некорректный формат суммы. Введите число.")
		return
	}

	file, header, err := r.FormFile("invoice_file")
	if err != nil {
		s.writeJSONResponse(w, false, "Ошибка получения файла счета: "+err.Error())
		return
	}
	defer file.Close()

	fileContent, err := io.ReadAll(file)
	if err != nil {
		s.writeJSONResponse(w, false, "Ошибка чтения файла счета: "+err.Error())
		return
	}
	filename := header.Filename

	cfg := s.cfgMgr.Get()
	if !cfg.IsReady() {
		s.writeJSONResponse(w, false, "Настройки сервиса не заполнены полностью.")
		return
	}

	pClient := pyrus.NewClient(cfg.Pyrus.AuthURL, cfg.Pyrus.Login, cfg.Pyrus.SecurityKey)
	pClient.SetLogger(s.log)
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	if err := pClient.Authorize(ctx); err != nil {
		s.writeJSONResponse(w, false, "Ошибка авторизации в Pyrus: "+err.Error())
		return
	}

	loc, _ := cfg.Location()
	now := time.Now().In(loc)
	since, _ := watcher.PeriodBounds(now, cfg.Filter.StartDay, cfg.Filter.EndDay)

	tasks, err := pClient.FindTasksByForm(ctx, cfg.Pyrus.FormID, since)
	if err != nil {
		s.writeJSONResponse(w, false, "Ошибка поиска задач: "+err.Error())
		return
	}

	var targetTaskID int
	var foundCount int
	normClientName := invoice.NormalizeString(clientName)

	for _, t := range tasks {
		if t.CloseDate != nil {
			continue
		}
		if t.CreateDate.Before(since) {
			continue
		}
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
		s.writeJSONResponse(w, false, fmt.Sprintf("Задача для клиента %q не найдена в форме Pyrus.", clientName))
		return
	}
	if foundCount > 1 {
		s.writeJSONResponse(w, false, fmt.Sprintf("Найдено несколько открытых задач (%d) для клиента %q.", foundCount, clientName))
		return
	}

	existingCount := 0
	taskDetails, err := pClient.GetTask(ctx, targetTaskID)
	if err == nil {
		if f, found := taskDetails.FindField(cfg.Pyrus.AttachmentFieldID); found && f.Value != nil {
			if valSlice, ok := f.Value.([]any); ok {
				existingCount = len(valSlice)
			} else if valStr, ok := f.Value.(string); ok && valStr != "" {
				existingCount = 1
			}
		}
	}

	guid, err := pClient.UploadFile(ctx, filename, fileContent)
	if err != nil {
		s.writeJSONResponse(w, false, "Ошибка загрузки файла в Pyrus: "+err.Error())
		return
	}

	var comment string
	if existingCount > 0 {
		comment = fmt.Sprintf("Тестовая загрузка из панели управления.\nВ задаче уже файлов: %d. Новый прикрепленный файл: %s\nСумма: %.2f", existingCount, filename, amount)
	} else {
		comment = fmt.Sprintf("Тестовая загрузка из панели управления.\nПрикреплен файл: %s\nСумма: %.2f", filename, amount)
	}

	err = pClient.UpdateTaskInvoice(ctx, targetTaskID, cfg.Pyrus.AttachmentFieldID, []string{guid}, cfg.Pyrus.AmountFieldID, amount, comment)
	if err != nil {
		s.writeJSONResponse(w, false, "Ошибка обновления полей задачи: "+err.Error())
		return
	}

	s.writeJSONResponse(w, true, fmt.Sprintf("Тестовый счет успешно загружен в задачу %d для клиента %q.", targetTaskID, clientName))
}

func (s *Server) handleToggleDebug(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}

	enabled := r.URL.Query().Get("enabled") == "true"
	cfg := s.cfgMgr.Get()
	cfg.General.DebugLogging = enabled

	if err := s.cfgMgr.Set(cfg); err != nil {
		s.writeJSONResponse(w, false, "Не удалось сохранить конфигурацию: "+err.Error())
		return
	}

	s.log.SetDebug(enabled)
	s.log.Info("Изменён режим отладки из панели логов: %v", enabled)
	s.writeJSONResponse(w, true, fmt.Sprintf("Режим отладки переключен в: %v.", enabled))
}
