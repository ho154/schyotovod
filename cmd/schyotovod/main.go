package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"schyotovod/internal/attempts"
	"schyotovod/internal/auth"
	"schyotovod/internal/config"
	"schyotovod/internal/dedup"
	"schyotovod/internal/invoice"
	"schyotovod/internal/journal"
	"schyotovod/internal/logger"
	"schyotovod/internal/updater"
	"schyotovod/internal/version"
	"schyotovod/internal/watcher"
	"schyotovod/internal/web"
)

func main() {
	var (
		dataDir           = flag.String("data-dir", defaultDataDir(), "каталог для config.json, логов и данных")
		selfCheck         = flag.Bool("self-check", false, "проверка работоспособности (для самообновления)")
		resetPassword     = flag.Bool("reset-admin-password", false, "сбросить пароль администратора панели")
		resetLogin        = flag.Bool("reset-admin-login", false, "также сбросить логин администратора (вместе с --reset-admin-password)")
		showVersion       = flag.Bool("version", false, "показать версию и выйти")
		initAdminLogin    = flag.String("init-admin-login", "", "задать логин администратора (для установщика)")
		initAdminPassword = flag.String("init-admin-password", "", "задать пароль администратора (для установщика)")
		parseFile         = flag.String("parse-file", "", "протестировать парсинг указанного текстового файла с письмом")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("Schyotovod %s\n", version.Version)
		return
	}

	if *parseFile != "" {
		data, err := os.ReadFile(*parseFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Ошибка чтения файла: %v\n", err)
			os.Exit(1)
		}
		info, err := invoice.ParseMessageBody(string(data))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Ошибка парсинга: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Результаты парсинга:")
		fmt.Printf("  Клиент: %s\n", info.ClientName)
		fmt.Printf("  Номер лицензии: %s\n", info.LicenseNo)
		if !info.LicenseDate.IsZero() {
			fmt.Printf("  Дата лицензии: %s\n", info.LicenseDate.Format("02.01.2006"))
		}
		fmt.Printf("  Сумма: %.2f\n", info.Amount)
		return
	}

	configPath := filepath.Join(*dataDir, "config.json")
	cfgMgr, err := config.NewManager(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка загрузки конфигурации: %v\n", err)
		os.Exit(1)
	}

	// Режим self-check: проверяем, что конфиг читается, и выходим.
	if *selfCheck {
		if err := runSelfCheck(cfgMgr); err != nil {
			fmt.Fprintf(os.Stderr, "self-check не пройден: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("self-check пройден")
		return
	}

	// Режим сброса пароля/логина администратора.
	if *resetPassword {
		if err := runResetAdmin(cfgMgr, *dataDir, *resetLogin); err != nil {
			fmt.Fprintf(os.Stderr, "Ошибка сброса доступа: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Режим инициализации администратора (для install.sh).
	if *initAdminLogin != "" && *initAdminPassword != "" {
		if err := runInitAdmin(cfgMgr, *initAdminLogin, *initAdminPassword); err != nil {
			fmt.Fprintf(os.Stderr, "Ошибка инициализации администратора: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Логин и пароль администратора установлены.")
		return
	}

	runService(cfgMgr, *dataDir)
}

// runService запускает основной режим: логгер, наблюдатель за почтой,
// планировщик обновлений и веб-панель.
func runService(cfgMgr *config.Manager, dataDir string) {
	cfg := cfgMgr.Get()
	loc, locErr := cfg.Location()

	logDir := filepath.Join(dataDir, "logs")
	log, err := logger.New(logDir, loc, cfg.General.LogRetentionDays)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка инициализации логов: %v\n", err)
		os.Exit(1)
	}
	defer log.Close()
	log.SetDebug(cfg.General.DebugLogging)

	log.Info("=== Запуск Schyotovod %s ===", version.Version)
	if locErr != nil {
		log.Warn("Проблема с часовым поясом: %v. Используется UTC", locErr)
	}

	stop := make(chan struct{})
	log.StartCleanupScheduler(stop)

	// Дедупликация.
	dedupPath := filepath.Join(dataDir, "dedup.json")
	dd, err := dedup.New(dedupPath, loc)
	if err != nil {
		log.Error("Ошибка инициализации дедупликации: %v", err)
		os.Exit(1)
	}

	// Менеджер попыток.
	attemptsPath := filepath.Join(dataDir, "attempts.json")
	attMgr, err := attempts.New(attemptsPath, loc)
	if err != nil {
		log.Error("Ошибка инициализации менеджера попыток: %v", err)
		os.Exit(1)
	}

	// Журнал событий (структурированные записи + трейсы запросов).
	journalDir := filepath.Join(dataDir, "journal")
	jMgr, err := journal.New(journalDir, loc)
	if err != nil {
		log.Error("Ошибка инициализации журнала событий: %v", err)
		os.Exit(1)
	}
	// Очистка журнала по тому же сроку хранения, что и текстовые логи.
	startJournalCleanup(stop, jMgr, log, func() int { return cfgMgr.Get().General.LogRetentionDays })

	// Наблюдатель за почтой.
	updater.SetVersionGetter(func() string { return version.Version })
	w := watcher.New(cfgMgr, dd, attMgr, jMgr, log)
	go w.Run()

	// Менеджер обновлений.
	updMgr := updater.NewManager(cfgMgr, log)
	updMgr.StartScheduler()

	// Веб-панель.
	srv, err := web.New(cfgMgr, log, w, updMgr, jMgr)
	if err != nil {
		log.Error("Ошибка инициализации веб-панели: %v", err)
		os.Exit(1)
	}

	addr := fmt.Sprintf(":%d", cfg.Web.Port)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		log.Info("Веб-панель управления доступна на порту %d", cfg.Web.Port)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("Ошибка веб-сервера: %v", err)
		}
	}()

	if cfg.Web.AdminLogin == "" || cfg.Web.AdminPasswordHash == "" {
		log.Warn("Логин/пароль администратора не заданы. Задайте их через установщик или команду --reset-admin-password")
	}
	if !cfg.IsReady() {
		log.Info("Ожидание заполнения настроек через веб-панель (http://<адрес-сервера>:%d)", cfg.Web.Port)
	}

	// Ожидание сигнала завершения.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	log.Info("Получен сигнал завершения, остановка сервиса…")
	close(stop)
	w.Stop()
	updMgr.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	log.Info("Сервис остановлен")
}

// runSelfCheck выполняет минимальную проверку работоспособности новой версии.
func runSelfCheck(cfgMgr *config.Manager) error {
	_ = cfgMgr.Get()
	return nil
}

// runResetAdmin сбрасывает пароль (и опционально логин) администратора,
// выводя новые значения в терминал. Пишет событие в аудит-лог.
func runResetAdmin(cfgMgr *config.Manager, dataDir string, resetLogin bool) error {
	cfg := cfgMgr.Get()

	login := cfg.Web.AdminLogin
	if resetLogin || login == "" {
		suffix, err := auth.GenerateToken(3)
		if err != nil {
			return err
		}
		login = "admin-" + shorten(suffix, 4)
	}

	newPass, err := auth.GenerateReadablePassword(12)
	if err != nil {
		return err
	}
	hash, err := auth.HashPassword(newPass)
	if err != nil {
		return err
	}

	cfg.Web.AdminLogin = login
	cfg.Web.AdminPasswordHash = hash
	cfg.Web.MustChangePassword = true
	if err := cfgMgr.Set(cfg); err != nil {
		return err
	}

	// Аудит-лог (в общий лог сервиса).
	loc, _ := cfg.Location()
	if log, err := logger.New(filepath.Join(dataDir, "logs"), loc, cfg.General.LogRetentionDays); err == nil {
		osUser := os.Getenv("USER")
		if osUser == "" {
			osUser = os.Getenv("USERNAME")
		}
		log.Info("Выполнен сброс пароля администратора через консоль сервера (пользователь ОС: %s)", osUser)
		_ = log.Close()
	}

	fmt.Println()
	fmt.Println("Пароль администратора сброшен.")
	fmt.Println()
	fmt.Println("Для входа в панель управления Schyotovod используйте:")
	fmt.Println()
	fmt.Printf("  Адрес:  http://<ip-сервера>:%d\n", cfg.Web.Port)
	fmt.Printf("  Логин:  %s\n", login)
	fmt.Printf("  Пароль: %s\n", newPass)
	fmt.Println()
	fmt.Println("При первом входе с этим паролем система попросит задать новый пароль.")
	fmt.Println()
	return nil
}

// runInitAdmin задаёт логин/пароль администратора (используется install.sh).
func runInitAdmin(cfgMgr *config.Manager, login, password string) error {
	cfg := cfgMgr.Get()
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	cfg.Web.AdminLogin = login
	cfg.Web.AdminPasswordHash = hash
	cfg.Web.MustChangePassword = false
	return cfgMgr.Set(cfg)
}

// startJournalCleanup запускает фоновую очистку журнала событий раз в сутки,
// синхронно с очисткой текстовых логов, используя тот же срок хранения
// (general.log_retention_days).
func startJournalCleanup(stop <-chan struct{}, jMgr *journal.Manager, log *logger.Logger, retention func() int) {
	go func() {
		clean := func() {
			if n, err := jMgr.Cleanup(retention()); err != nil {
				log.Error("Очистка журнала событий: %v", err)
			} else if n > 0 {
				log.Info("Очистка журнала событий: удалено устаревших записей — %d", n)
			}
		}
		clean()
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				clean()
			}
		}
	}()
}

// defaultDataDir возвращает каталог данных по умолчанию в зависимости от ОС.
func defaultDataDir() string {
	if dir := os.Getenv("SCHYOTOVOD_DATA_DIR"); dir != "" {
		return dir
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}

// shorten возвращает первые n символов строки.
func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
