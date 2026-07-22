// Package config отвечает за загрузку, сохранение и валидацию структуры настроек сервиса.
// Секреты (пароль приложения Gmail, ключ безопасности Pyrus) хранятся в config.json,
// который находится вне git-репозитория.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// formIDRe извлекает числовой идентификатор формы из ссылки вида
// "https://pyrus.com/t#uf1089723" (форма) или "https://pyrus.com/t#id123456" (задача),
// а также принимает просто число. Используется, чтобы пользователь мог вставить
// в поле «ID формы Pyrus» прямую ссылку на форму/задачу из браузера, а не
// выяснять числовой идентификатор вручную.
var formIDRe = regexp.MustCompile(`(\d+)\s*$`)

// ExtractFormID извлекает числовой ID формы (или иного объекта Pyrus) из
// произвольной строки: либо это уже готовое число, либо ссылка вида
// "https://pyrus.com/t#uf1089723", "https://pyrus.com/t#id123456" и т.п.
// Если число извлечь не удалось, возвращается 0.
func ExtractFormID(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	if m := formIDRe.FindStringSubmatch(s); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return n
		}
	}
	return 0
}

// GmailConfig — параметры доступа к почтовому ящику Gmail.
type GmailConfig struct {
	Email       string `json:"email"`
	AppPassword string `json:"app_password"`
	IMAPHost    string `json:"imap_host"`
	IMAPPort    int    `json:"imap_port"`
}

// FilterConfig — правила отбора писем и период сканирования.
type FilterConfig struct {
	SenderEmail                 string `json:"sender_email"`
	StartDay                    int    `json:"start_day"`
	EndDay                      int    `json:"end_day"`
	FallbackPollIntervalMinutes int    `json:"fallback_poll_interval_minutes"`
}

// PyrusConfig — настройки интеграции с API Pyrus.
type PyrusConfig struct {
	Login                string `json:"login"`
	SecurityKey          string `json:"security_key"`
	AuthURL              string `json:"auth_url"`
	FormID               int    `json:"form_id"`
	TaskID               int    `json:"task_id"`
	AttachmentFieldID    int    `json:"attachment_field_id"`
	ClientNameFieldID    int    `json:"client_name_field_id"`
	AmountFieldID        int    `json:"amount_field_id"`
	MaxUpdateAttempts    int    `json:"max_update_attempts"`
	RetryIntervalMinutes int    `json:"retry_interval_minutes"`
}

// GeneralConfig — общие параметры работы сервиса.
type GeneralConfig struct {
	Timezone         string `json:"timezone"`
	LogRetentionDays int    `json:"log_retention_days"`
	DebugLogging     bool   `json:"debug_logging"`
}

// WebConfig — настройки веб-панели управления.
type WebConfig struct {
	Port               int    `json:"port"`
	AdminLogin         string `json:"admin_login"`
	AdminPasswordHash  string `json:"admin_password_hash"`
	MustChangePassword bool   `json:"must_change_password"`
}

// UpdateConfig — параметры самообновления сервиса.
type UpdateConfig struct {
	AutoUpdate bool   `json:"auto_update"`
	CheckTime  string `json:"check_time"`
	GitHubRepo string `json:"github_repo"`
}

// Config — корневая структура настроек.
type Config struct {
	Gmail   GmailConfig   `json:"gmail"`
	Filter  FilterConfig  `json:"filter"`
	Pyrus   PyrusConfig   `json:"pyrus"`
	General GeneralConfig `json:"general"`
	Web     WebConfig     `json:"web"`
	Update  UpdateConfig  `json:"update"`
}

// Manager инкапсулирует потокобезопасный доступ к конфигу и его файловое хранение.
type Manager struct {
	mu   sync.RWMutex
	cfg  Config
	path string
}

// Default возвращает конфигурацию со значениями по умолчанию.
// Ключевые поля Pyrus (form_id, task_id, attachment_field_id) остаются нулевыми —
// пока они не заполнены, сервис не запускает проверку почты.
func Default() Config {
	return Config{
		Gmail: GmailConfig{
			IMAPHost: "imap.gmail.com",
			IMAPPort: 993,
		},
		Filter: FilterConfig{
			StartDay:                    20,
			EndDay:                      29,
			FallbackPollIntervalMinutes: 30,
		},
		Pyrus: PyrusConfig{
			AuthURL:              "https://accounts.pyrus.com/api/v4/auth",
			MaxUpdateAttempts:    3,
			RetryIntervalMinutes: 60,
		},
		General: GeneralConfig{
			Timezone:         "Asia/Almaty",
			LogRetentionDays: 90,
		},
		Web: WebConfig{
			Port: 47291,
		},
		Update: UpdateConfig{
			AutoUpdate: true,
			CheckTime:  "03:00",
			GitHubRepo: "ho154/schyotovod",
		},
	}
}

// NewManager создаёт менеджер конфигурации и загружает config.json по указанному пути.
// Если файл отсутствует, используется конфигурация по умолчанию (файл будет создан
// при первом сохранении из веб-админки или установщиком).
func NewManager(path string) (*Manager, error) {
	m := &Manager{path: path, cfg: Default()}
	if _, err := os.Stat(path); err == nil {
		if err := m.Load(); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Load читает config.json с диска. Отсутствующие поля дополняются значениями по умолчанию.
func (m *Manager) Load() error {
	data, err := os.ReadFile(m.path)
	if err != nil {
		return fmt.Errorf("не удалось прочитать файл конфигурации %q: %w", m.path, err)
	}
	cfg := Default()
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("ошибка разбора файла конфигурации %q: %w", m.path, err)
	}
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
	return nil
}

// Save атомарно записывает конфигурацию в config.json (через временный файл + rename).
func (m *Manager) Save() error {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()

	if dir := filepath.Dir(m.path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("не удалось создать каталог для конфигурации %q: %w", dir, err)
		}
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("ошибка сериализации конфигурации: %w", err)
	}

	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("не удалось записать временный файл конфигурации: %w", err)
	}
	if err := os.Rename(tmp, m.path); err != nil {
		return fmt.Errorf("не удалось заменить файл конфигурации: %w", err)
	}
	return nil
}

// Get возвращает копию текущей конфигурации (потокобезопасно).
func (m *Manager) Get() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

// Set заменяет текущую конфигурацию и сохраняет её на диск.
func (m *Manager) Set(cfg Config) error {
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
	return m.Save()
}

// Path возвращает путь к файлу конфигурации.
func (m *Manager) Path() string {
	return m.path
}

// Location возвращает часовой пояс из конфигурации.
// При ошибке разбора имени возвращается UTC и текст ошибки.
func (c Config) Location() (*time.Location, error) {
	loc, err := time.LoadLocation(c.General.Timezone)
	if err != nil {
		return time.UTC, fmt.Errorf("неизвестный часовой пояс %q: %w", c.General.Timezone, err)
	}
	return loc, nil
}

// IsPyrusConfigured сообщает, заполнены ли ключевые поля Pyrus,
// без которых прикрепление счёта невозможно.
func (c Config) IsPyrusConfigured() bool {
	return c.Pyrus.Login != "" &&
		c.Pyrus.SecurityKey != "" &&
		c.Pyrus.FormID != 0 &&
		c.Pyrus.AttachmentFieldID != 0 &&
		c.Pyrus.ClientNameFieldID != 0 &&
		c.Pyrus.AmountFieldID != 0
}

// IsGmailConfigured сообщает, заполнены ли параметры доступа к почте.
func (c Config) IsGmailConfigured() bool {
	return c.Gmail.Email != "" && c.Gmail.AppPassword != "" && c.Filter.SenderEmail != ""
}

// IsReady сообщает, готов ли сервис к работе (почта и Pyrus настроены).
func (c Config) IsReady() bool {
	return c.IsGmailConfigured() && c.IsPyrusConfigured()
}
