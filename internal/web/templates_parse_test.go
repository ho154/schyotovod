package web

import (
	"testing"

	"schyotovod/internal/config"
	"schyotovod/internal/logger"
)

// TestTemplatesParse проверяет, что все встроенные шаблоны (включая обновлённый
// logs.html с таблицей журнала) успешно разбираются при создании сервера.
func TestTemplatesParse(t *testing.T) {
	cfgMgr, err := config.NewManager(t.TempDir() + "/config.json")
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	log, err := logger.New(t.TempDir(), nil, 7)
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	defer log.Close()

	// checker/applier/journal — nil: New не должен на этом падать при парсинге.
	srv, err := New(cfgMgr, log, nil, nil, nil)
	if err != nil {
		t.Fatalf("New вернул ошибку (вероятно, ошибка парсинга шаблона): %v", err)
	}
	if srv == nil {
		t.Fatal("New вернул nil-сервер")
	}
	for _, name := range []string{"logs.html", "settings.html", "login.html", "updates.html"} {
		if _, ok := srv.templates[name]; !ok {
			t.Errorf("шаблон %s не загружен", name)
		}
	}
}
