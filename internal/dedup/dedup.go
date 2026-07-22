// Package dedup реализует лёгкую дедупликацию обработанных писем.
// Хранит список Message-ID за текущий месяц в JSON-файле. При смене месяца
// список автоматически сбрасывается — за предыдущий месяц письма больше не нужны.
package dedup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// store — сериализуемая структура файла дедупликации.
type store struct {
	// Month — месяц, к которому относится список, в формате "2006-01".
	Month string `json:"month"`
	// ProcessedMessageIDs — Message-ID уже обработанных писем за этот месяц.
	ProcessedMessageIDs []string `json:"processed_message_ids"`
}

// Dedup управляет файлом дедупликации потокобезопасно.
type Dedup struct {
	mu    sync.Mutex
	path  string
	loc   *time.Location
	data  store
	index map[string]struct{} // быстрый поиск по Message-ID
}

// New создаёт менеджер дедупликации с файлом по указанному пути.
// loc — часовой пояс для определения текущего месяца.
func New(path string, loc *time.Location) (*Dedup, error) {
	if loc == nil {
		loc = time.UTC
	}
	d := &Dedup{
		path:  path,
		loc:   loc,
		index: make(map[string]struct{}),
	}
	if err := d.load(); err != nil {
		return nil, err
	}
	return d, nil
}

// currentMonth возвращает текущий месяц в формате "2006-01".
func (d *Dedup) currentMonth() string {
	return time.Now().In(d.loc).Format("2006-01")
}

// load читает файл дедупликации. Если файла нет или месяц устарел — инициализирует пустой список.
func (d *Dedup) load() error {
	month := d.currentMonth()

	data, err := os.ReadFile(d.path)
	if err != nil {
		if os.IsNotExist(err) {
			d.data = store{Month: month, ProcessedMessageIDs: nil}
			d.rebuildIndex()
			return nil
		}
		return fmt.Errorf("не удалось прочитать файл дедупликации %q: %w", d.path, err)
	}

	var s store
	if err := json.Unmarshal(data, &s); err != nil {
		// Повреждённый файл — начинаем с чистого списка, не роняем сервис.
		d.data = store{Month: month, ProcessedMessageIDs: nil}
		d.rebuildIndex()
		return nil
	}

	// Если сохранённый месяц не совпадает с текущим — сбрасываем список.
	if s.Month != month {
		d.data = store{Month: month, ProcessedMessageIDs: nil}
	} else {
		d.data = s
	}
	d.rebuildIndex()
	return nil
}

// rebuildIndex перестраивает индекс из списка Message-ID.
func (d *Dedup) rebuildIndex() {
	d.index = make(map[string]struct{}, len(d.data.ProcessedMessageIDs))
	for _, id := range d.data.ProcessedMessageIDs {
		d.index[id] = struct{}{}
	}
}

// save атомарно записывает файл дедупликации на диск.
func (d *Dedup) save() error {
	if dir := filepath.Dir(d.path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("не удалось создать каталог для файла дедупликации %q: %w", dir, err)
		}
	}
	raw, err := json.MarshalIndent(d.data, "", "  ")
	if err != nil {
		return fmt.Errorf("ошибка сериализации данных дедупликации: %w", err)
	}
	tmp := d.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("не удалось записать временный файл дедупликации: %w", err)
	}
	if err := os.Rename(tmp, d.path); err != nil {
		return fmt.Errorf("не удалось заменить файл дедупликации: %w", err)
	}
	return nil
}

// ensureCurrentMonth сбрасывает список, если наступил новый месяц.
// Должен вызываться под удержанным mu.
func (d *Dedup) ensureCurrentMonth() {
	month := d.currentMonth()
	if d.data.Month != month {
		d.data = store{Month: month, ProcessedMessageIDs: nil}
		d.rebuildIndex()
	}
}

// IsProcessed сообщает, было ли письмо с данным Message-ID уже обработано в текущем месяце.
func (d *Dedup) IsProcessed(messageID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ensureCurrentMonth()
	_, ok := d.index[messageID]
	return ok
}

// MarkProcessed отмечает письмо как обработанное и сохраняет файл.
func (d *Dedup) MarkProcessed(messageID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ensureCurrentMonth()
	if _, ok := d.index[messageID]; ok {
		return nil // уже отмечено
	}
	d.data.ProcessedMessageIDs = append(d.data.ProcessedMessageIDs, messageID)
	d.index[messageID] = struct{}{}
	return d.save()
}

// Count возвращает число обработанных писем в текущем месяце.
func (d *Dedup) Count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ensureCurrentMonth()
	return len(d.data.ProcessedMessageIDs)
}
