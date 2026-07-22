package updater

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"schyotovod/internal/config"
	"schyotovod/internal/logger"
)

// selfCheckArg — аргумент, с которым запускается новый бинарник для self-check.
// Новая версия при получении этого флага должна проверить работоспособность
// (чтение конфига и т.п.) и завершиться с кодом 0 при успехе.
const selfCheckArg = "--self-check"

// Manager управляет расписанием проверки обновлений, применением и откатом.
// Реализует интерфейс web.UpdateApplier.
type Manager struct {
	cfgMgr *config.Manager
	log    *logger.Logger

	mu           sync.Mutex
	lastErr      string
	stopOnce     sync.Once
	stop         chan struct{}
	lastCheckDay string
}

// NewManager создаёт менеджер обновлений.
func NewManager(cfgMgr *config.Manager, log *logger.Logger) *Manager {
	return &Manager{
		cfgMgr: cfgMgr,
		log:    log,
		stop:   make(chan struct{}),
	}
}

// Stop останавливает планировщик.
func (m *Manager) Stop() {
	m.stopOnce.Do(func() { close(m.stop) })
}

// newUpdater создаёт updater из текущего конфига.
func (m *Manager) newUpdater() *Updater {
	cfg := m.cfgMgr.Get()
	return New(cfg.Update.GitHubRepo, currentVersionString())
}

// CheckForUpdate проверяет наличие новой версии. Возвращает релиз и признак «новее».
func (m *Manager) CheckForUpdate(ctx context.Context) (*Release, bool, error) {
	u := m.newUpdater()
	rel, err := u.LatestRelease(ctx)
	if err != nil {
		return nil, false, err
	}
	return rel, u.IsNewer(rel), nil
}

// ApplyUpdate скачивает, проверяет, применяет обновление и перезапускает сервис.
// При неудачном self-check выполняет откат и записывает полную ошибку.
func (m *Manager) ApplyUpdate(ctx context.Context, rel *Release) error {
	u := m.newUpdater()

	m.log.Info("Загрузка обновления %s…", rel.TagName)
	bin, err := u.DownloadAndVerify(ctx, rel)
	if err != nil {
		m.setLastErr(err.Error())
		m.log.Error("Обновление до %s отменено: %v", rel.TagName, err)
		return err
	}

	m.log.Info("Контрольная сумма проверена, установка новой версии…")
	backupPath, err := u.Apply(bin)
	if err != nil {
		m.setLastErr(err.Error())
		m.log.Error("Не удалось установить обновление %s: %v", rel.TagName, err)
		return err
	}

	// Self-check: запускаем новый бинарник с флагом проверки.
	if err := m.runSelfCheck(); err != nil {
		full := fmt.Sprintf("Новая версия %s не прошла проверку работоспособности: %v", rel.TagName, err)
		m.log.Error("%s. Выполняется откат на предыдущую версию", full)
		if rbErr := Rollback(backupPath); rbErr != nil {
			m.log.Error("Откат не удался: %v", rbErr)
			m.setLastErr(full + " | Откат также не удался: " + rbErr.Error())
			return fmt.Errorf("%s (откат не удался: %v)", full, rbErr)
		}
		m.log.Info("Откат на предыдущую версию выполнен успешно")
		m.setLastErr(full + " — выполнен автоматический откат на предыдущую версию.")
		return errors.New(full)
	}

	m.clearLastErr()
	m.log.Info("Обновление до %s успешно установлено, перезапуск сервиса", rel.TagName)
	m.restart()
	return nil
}

// runSelfCheck запускает текущий (уже заменённый) исполняемый файл с флагом
// self-check и ждёт его успешного завершения.
func (m *Manager) runSelfCheck() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe, selfCheckArg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v (вывод: %s)", err, string(out))
	}
	return nil
}

// restart перезапускает сервис. Под systemd с Restart=on-failure достаточно
// завершить процесс — systemd поднимет его заново уже с новым бинарником.
func (m *Manager) restart() {
	// Небольшая задержка, чтобы HTTP-ответ успел уйти клиенту.
	go func() {
		time.Sleep(1 * time.Second)
		os.Exit(0)
	}()
}

// LastUpdateError возвращает текст последней ошибки обновления (для отображения в админке).
func (m *Manager) LastUpdateError() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

func (m *Manager) setLastErr(s string) {
	m.mu.Lock()
	m.lastErr = s
	m.mu.Unlock()
}

func (m *Manager) clearLastErr() {
	m.mu.Lock()
	m.lastErr = ""
	m.mu.Unlock()
}

// StartScheduler запускает фоновую проверку обновлений по расписанию из конфига.
func (m *Manager) StartScheduler() {
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-m.stop:
				return
			case <-ticker.C:
				m.maybeAutoUpdate()
			}
		}
	}()
}

// maybeAutoUpdate проверяет, наступило ли время автообновления, и выполняет его.
func (m *Manager) maybeAutoUpdate() {
	cfg := m.cfgMgr.Get()
	if !cfg.Update.AutoUpdate || cfg.Update.GitHubRepo == "" {
		return
	}

	loc, _ := cfg.Location()
	now := time.Now().In(loc)
	if now.Format("15:04") != cfg.Update.CheckTime {
		return
	}
	// Не запускаем повторно в ту же минуту/день.
	day := now.Format("2006-01-02")
	m.mu.Lock()
	if m.lastCheckDay == day {
		m.mu.Unlock()
		return
	}
	m.lastCheckDay = day
	m.mu.Unlock()

	m.log.Info("Плановая проверка обновлений")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rel, newer, err := m.CheckForUpdate(ctx)
	if err != nil {
		m.log.Error("Плановая проверка обновлений не удалась: %v", err)
		return
	}
	if !newer {
		m.log.Info("Обновлений нет, установлена последняя версия")
		return
	}
	m.log.Info("Найдена новая версия %s, начинается автоматическое обновление", rel.TagName)
	if err := m.ApplyUpdate(ctx, rel); err != nil {
		m.log.Error("Автоматическое обновление не удалось: %v", err)
	}
}

// currentVersionString получает текущую версию (обёртка, чтобы не создавать
// циклический импорт с пакетом version — значение передаётся при сборке).
var currentVersionGetter = func() string { return "dev" }

// SetVersionGetter позволяет main задать функцию получения текущей версии.
func SetVersionGetter(f func() string) {
	if f != nil {
		currentVersionGetter = f
	}
}

func currentVersionString() string {
	return currentVersionGetter()
}
