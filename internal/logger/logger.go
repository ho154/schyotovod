// Package logger реализует файловое логирование с ротацией по дням и
// автоматической очисткой старых файлов (retention). Все сообщения — на русском.
// Логи одновременно пишутся в файл и в стандартный поток (для journalctl systemd).
package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Level — уровень важности сообщения.
type Level string

const (
	LevelInfo  Level = "ИНФО"
	LevelWarn  Level = "ПРЕДУПРЕЖДЕНИЕ"
	LevelError Level = "ОШИБКА"
	// LevelDebug — отладочные сообщения (полные тела API-запросов/ответов и т.п.),
	// пишутся только когда включён режим отладки (см. SetDebug).
	LevelDebug Level = "ОТЛАДКА"
)

// Logger пишет сообщения в файл вида logs/YYYY-MM-DD.log и в stdout.
// При смене суток автоматически переключается на новый файл.
type Logger struct {
	mu            sync.Mutex
	dir           string
	loc           *time.Location
	retentionDays int
	curDate       string
	file          *os.File
	debug         bool

	// subMu/subs реализуют онлайн-трансляцию новых строк лога подписчикам
	// (используется веб-панелью для отображения логов в реальном времени
	// через Server-Sent Events).
	subMu sync.Mutex
	subs  map[chan string]struct{}
}

// dateFileRe соответствует именам файлов логов вида 2025-07-20.log.
var dateFileRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})\.log$`)

// New создаёт логгер, пишущий в каталог dir. loc — часовой пояс для меток времени
// и имён файлов, retentionDays — сколько дней хранить логи.
func New(dir string, loc *time.Location, retentionDays int) (*Logger, error) {
	if loc == nil {
		loc = time.UTC
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("не удалось создать каталог логов %q: %w", dir, err)
	}
	l := &Logger{
		dir:           dir,
		loc:           loc,
		retentionDays: retentionDays,
		subs:          make(map[chan string]struct{}),
	}
	if err := l.rotate(time.Now().In(loc)); err != nil {
		return nil, err
	}
	return l, nil
}

// rotate открывает файл лога для указанной даты, закрывая предыдущий при смене суток.
// Вызывается при инициализации и при каждой записи, если сменились сутки.
// Должен вызываться под удержанным mu.
func (l *Logger) rotate(now time.Time) error {
	date := now.Format("2006-01-02")
	if date == l.curDate && l.file != nil {
		return nil
	}
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	path := filepath.Join(l.dir, date+".log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("не удалось открыть файл лога %q: %w", path, err)
	}
	l.file = f
	l.curDate = date
	return nil
}

// SetLocation обновляет часовой пояс логгера (например, после изменения конфига).
func (l *Logger) SetLocation(loc *time.Location) {
	if loc == nil {
		return
	}
	l.mu.Lock()
	l.loc = loc
	l.mu.Unlock()
}

// SetRetention обновляет срок хранения логов.
func (l *Logger) SetRetention(days int) {
	l.mu.Lock()
	l.retentionDays = days
	l.mu.Unlock()
}

// SetDebug включает/выключает режим отладки: при включении в лог начинают
// попадать сообщения уровня Debug (полные тела запросов/ответов к внешним API
// и другие подробности), при выключении такие сообщения перестают писаться.
func (l *Logger) SetDebug(enabled bool) {
	l.mu.Lock()
	l.debug = enabled
	l.mu.Unlock()
}

// IsDebug сообщает, включён ли режим отладки.
func (l *Logger) IsDebug() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.debug
}

// write формирует строку лога, пишет её в файл, stdout и рассылает всем
// текущим подписчикам (см. Subscribe) — это обеспечивает онлайн-отображение
// логов в веб-панели без перезагрузки страницы.
func (l *Logger) write(level Level, msg string) {
	l.mu.Lock()
	now := time.Now().In(l.loc)
	if err := l.rotate(now); err != nil {
		// Если файл открыть не удалось — хотя бы выведем в stdout.
		fmt.Fprintf(os.Stderr, "%s [%s] %s\n", now.Format("2006-01-02 15:04:05"), LevelError,
			"ошибка ротации лога: "+err.Error())
	}
	line := fmt.Sprintf("%s [%s] %s\n", now.Format("2006-01-02 15:04:05"), level, msg)
	if l.file != nil {
		_, _ = l.file.WriteString(line)
	}
	l.mu.Unlock()

	// Дублируем в stdout для journalctl.
	fmt.Print(line)

	l.publish(line)
}

// Info пишет информационное сообщение.
func (l *Logger) Info(format string, args ...any) {
	l.write(LevelInfo, fmt.Sprintf(format, args...))
}

// Warn пишет предупреждение.
func (l *Logger) Warn(format string, args ...any) {
	l.write(LevelWarn, fmt.Sprintf(format, args...))
}

// Error пишет сообщение об ошибке.
func (l *Logger) Error(format string, args ...any) {
	l.write(LevelError, fmt.Sprintf(format, args...))
}

// Debug пишет отладочное сообщение (полные тела API-запросов/ответов и т.п.),
// но только если включён режим отладки (см. SetDebug). Если отладка выключена,
// вызов практически ничего не стоит — сообщение не форматируется и не пишется.
func (l *Logger) Debug(format string, args ...any) {
	if !l.IsDebug() {
		return
	}
	l.write(LevelDebug, fmt.Sprintf(format, args...))
}

// Subscribe регистрирует нового подписчика на поток новых строк лога.
// Возвращает канал, в который будут отправляться отформатированные строки
// (включая символ переноса строки) по мере их записи. Вызывающий код обязан
// вызвать Unsubscribe с этим же каналом, когда подписка больше не нужна
// (например, при закрытии SSE-соединения клиента), чтобы не течь горутинами/памятью.
// Канал буферизован, чтобы медленный подписчик не блокировал запись логов;
// при переполнении буфера новые строки для этого подписчика молча отбрасываются.
func (l *Logger) Subscribe() chan string {
	ch := make(chan string, 100)
	l.subMu.Lock()
	l.subs[ch] = struct{}{}
	l.subMu.Unlock()
	return ch
}

// Unsubscribe отменяет подписку, созданную Subscribe, и закрывает канал.
func (l *Logger) Unsubscribe(ch chan string) {
	l.subMu.Lock()
	if _, ok := l.subs[ch]; ok {
		delete(l.subs, ch)
		close(ch)
	}
	l.subMu.Unlock()
}

// publish рассылает готовую строку лога всем текущим подписчикам, не блокируясь
// на медленных/переполненных получателях.
func (l *Logger) publish(line string) {
	l.subMu.Lock()
	defer l.subMu.Unlock()
	for ch := range l.subs {
		select {
		case ch <- line:
		default:
			// Подписчик не успевает читать — пропускаем строку для него,
			// чтобы не заблокировать запись логов остальным получателям.
		}
	}
}

// Cleanup удаляет файлы логов старше retentionDays. Возвращает число удалённых файлов.
func (l *Logger) Cleanup() (int, error) {
	l.mu.Lock()
	retention := l.retentionDays
	loc := l.loc
	l.mu.Unlock()

	if retention <= 0 {
		return 0, nil
	}

	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return 0, fmt.Errorf("не удалось прочитать каталог логов %q: %w", l.dir, err)
	}

	cutoff := time.Now().In(loc).AddDate(0, 0, -retention)
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := dateFileRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		fileDate, err := time.ParseInLocation("2006-01-02", m[1], loc)
		if err != nil {
			continue
		}
		if fileDate.Before(cutoff) {
			if err := os.Remove(filepath.Join(l.dir, e.Name())); err == nil {
				removed++
			}
		}
	}
	return removed, nil
}

// StartCleanupScheduler запускает фоновую очистку логов раз в сутки.
// Останавливается при закрытии канала stop.
func (l *Logger) StartCleanupScheduler(stop <-chan struct{}) {
	go func() {
		// Первая очистка сразу при старте.
		if n, err := l.Cleanup(); err != nil {
			l.Error("Очистка логов: %v", err)
		} else if n > 0 {
			l.Info("Очистка логов: удалено устаревших файлов — %d", n)
		}
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if n, err := l.Cleanup(); err != nil {
					l.Error("Очистка логов: %v", err)
				} else if n > 0 {
					l.Info("Очистка логов: удалено устаревших файлов — %d", n)
				}
			}
		}
	}()
}

// Close закрывает текущий файл лога.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		err := l.file.Close()
		l.file = nil
		return err
	}
	return nil
}

// ListLogDates возвращает отсортированный (по убыванию) список дат, за которые есть логи.
func (l *Logger) ListLogDates() ([]string, error) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать каталог логов %q: %w", l.dir, err)
	}
	var dates []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if m := dateFileRe.FindStringSubmatch(e.Name()); m != nil {
			dates = append(dates, m[1])
		}
	}
	// Сортировка по убыванию (новые сверху).
	for i := 0; i < len(dates); i++ {
		for j := i + 1; j < len(dates); j++ {
			if dates[j] > dates[i] {
				dates[i], dates[j] = dates[j], dates[i]
			}
		}
	}
	return dates, nil
}

// ReadLog возвращает содержимое лога за указанную дату (формат YYYY-MM-DD).
// Если задан levelFilter (непустой), возвращаются только строки с этим уровнем.
func (l *Logger) ReadLog(date string, levelFilter Level) (string, error) {
	if !dateFileRe.MatchString(date + ".log") {
		return "", fmt.Errorf("некорректная дата лога: %q", date)
	}
	path := filepath.Join(l.dir, date+".log")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("не удалось прочитать лог за %s: %w", date, err)
	}
	if levelFilter == "" {
		return string(data), nil
	}
	var b strings.Builder
	needle := "[" + string(levelFilter) + "]"
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, needle) {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String(), nil
}
