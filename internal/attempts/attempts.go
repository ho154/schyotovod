package attempts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AttemptState хранит сведения о попытках обработки для конкретного клиента.
type AttemptState struct {
	Count         int       `json:"count"`
	LastAttempt   time.Time `json:"last_attempt"`
	NextAttempt   time.Time `json:"next_attempt"`
	Ignored       bool      `json:"ignored"`
	LastMessageID string    `json:"last_message_id,omitempty"`
	// LastTaskID — ID последней найденной задачи Pyrus для этого клиента.
	// Позволяет в сообщениях об отсрочке ссылаться на задачу без повторного
	// похода в Pyrus за её поиском.
	LastTaskID int `json:"last_task_id,omitempty"`
}

// store описывает структуру файла сохранения попыток.
type store struct {
	Month string                  `json:"month"`
	Tasks map[string]AttemptState `json:"tasks"` // ключ — нормализованное имя клиента
}

// Manager управляет персистентным хранением попыток по клиентам за текущий месяц.
type Manager struct {
	mu   sync.Mutex
	path string
	loc  *time.Location
	data store
}

// New создает менеджер попыток.
func New(path string, loc *time.Location) (*Manager, error) {
	if loc == nil {
		loc = time.UTC
	}
	m := &Manager{
		path: path,
		loc:  loc,
		data: store{
			Tasks: make(map[string]AttemptState),
		},
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) currentMonth() string {
	return time.Now().In(m.loc).Format("2006-01")
}

func (m *Manager) load() error {
	month := m.currentMonth()
	data, err := os.ReadFile(m.path)
	if err != nil {
		if os.IsNotExist(err) {
			m.data = store{Month: month, Tasks: make(map[string]AttemptState)}
			return nil
		}
		return fmt.Errorf("не удалось прочитать файл попыток %q: %w", m.path, err)
	}

	var s store
	if err := json.Unmarshal(data, &s); err != nil {
		m.data = store{Month: month, Tasks: make(map[string]AttemptState)}
		return nil
	}

	if s.Month != month {
		m.data = store{Month: month, Tasks: make(map[string]AttemptState)}
	} else {
		m.data = s
		if m.data.Tasks == nil {
			m.data.Tasks = make(map[string]AttemptState)
		}
	}
	return nil
}

func (m *Manager) save() error {
	if dir := filepath.Dir(m.path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("не удалось создать каталог для файла попыток %q: %w", dir, err)
		}
	}
	raw, err := json.MarshalIndent(m.data, "", "  ")
	if err != nil {
		return fmt.Errorf("ошибка сериализации данных попыток: %w", err)
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("не удалось записать временный файл попыток: %w", err)
	}
	if err := os.Rename(tmp, m.path); err != nil {
		return fmt.Errorf("не удалось заменить файл попыток: %w", err)
	}
	return nil
}

func (m *Manager) ensureCurrentMonth() {
	month := m.currentMonth()
	if m.data.Month != month {
		m.data = store{Month: month, Tasks: make(map[string]AttemptState)}
	}
}

// GetState возвращает текущее состояние попыток для клиента.
func (m *Manager) GetState(client string) AttemptState {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureCurrentMonth()
	return m.data.Tasks[client]
}

// RecordAttempt регистрирует попытку (успех или неудачу).
// Если это успех, счетчик сбрасывается.
// Если неудача, счетчик увеличивается, вычисляется время следующей попытки.
func (m *Manager) RecordAttempt(client string, success bool, maxAttempts int, retryIntervalMinutes int) AttemptState {
	return m.RecordAttemptWithMsg(client, success, maxAttempts, retryIntervalMinutes, "")
}

// RecordAttemptWithMsg регистрирует попытку с привязкой к Message-ID.
// Если это успех, счетчик сбрасывается.
// Если неудача, счетчик увеличивается, вычисляется время следующей попытки.
func (m *Manager) RecordAttemptWithMsg(client string, success bool, maxAttempts int, retryIntervalMinutes int, messageID string) AttemptState {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureCurrentMonth()

	state := m.data.Tasks[client]
	now := time.Now().In(m.loc)

	// Если пришло новое письмо (другой Message-ID), сбрасываем предыдущие ошибки и таймеры
	if messageID != "" && state.LastMessageID != messageID {
		state.Count = 0
		state.Ignored = false
		state.NextAttempt = time.Time{}
		state.LastMessageID = messageID
	}

	if success {
		state.Count = 0
		state.Ignored = false
		state.LastAttempt = now
		state.NextAttempt = time.Time{}
		if messageID != "" {
			state.LastMessageID = messageID
		}
	} else {
		state.Count++
		state.LastAttempt = now
		if state.Count >= maxAttempts {
			state.Ignored = true
			state.NextAttempt = time.Time{}
		} else {
			state.NextAttempt = now.Add(time.Duration(retryIntervalMinutes) * time.Minute)
		}
	}

	m.data.Tasks[client] = state
	_ = m.save()
	return state
}

// SetLastTaskID сохраняет ID найденной задачи Pyrus для клиента, чтобы позже
// в сообщениях об отсрочке можно было сослаться на задачу без повторного поиска.
func (m *Manager) SetLastTaskID(client string, taskID int) {
	if taskID == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureCurrentMonth()
	state := m.data.Tasks[client]
	state.LastTaskID = taskID
	m.data.Tasks[client] = state
	_ = m.save()
}

// ResetAttempt сбрасывает состояние попыток для конкретного клиента.
func (m *Manager) ResetAttempt(client string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureCurrentMonth()
	delete(m.data.Tasks, client)
	_ = m.save()
}

// ResetAll сбрасывает состояние попыток для всех клиентов.
func (m *Manager) ResetAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureCurrentMonth()
	m.data.Tasks = make(map[string]AttemptState)
	_ = m.save()
}
