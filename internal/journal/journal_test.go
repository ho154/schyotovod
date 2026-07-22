package journal

import (
	"testing"
	"time"

	"schyotovod/internal/trace"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := New(t.TempDir(), time.UTC)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func TestUpsertAndGet(t *testing.T) {
	m := newTestManager(t)
	m.Upsert(Event{ID: "a@x", MessageID: "a@x", ClientName: "ИП Эксперт", OverallStatus: StatusOK})
	ev, ok := m.Get("a@x")
	if !ok {
		t.Fatal("событие не найдено после Upsert")
	}
	if ev.ClientName != "ИП Эксперт" {
		t.Errorf("ClientName = %q", ev.ClientName)
	}
	if ev.CreatedAt.IsZero() || ev.UpdatedAt.IsZero() {
		t.Error("CreatedAt/UpdatedAt должны проставляться")
	}

	// Повторный Upsert сохраняет CreatedAt.
	created := ev.CreatedAt
	m.Upsert(Event{ID: "a@x", MessageID: "a@x", ClientName: "Новое имя"})
	ev2, _ := m.Get("a@x")
	if !ev2.CreatedAt.Equal(created) {
		t.Error("CreatedAt не должен меняться при обновлении")
	}
	if ev2.ClientName != "Новое имя" {
		t.Errorf("обновление не применилось: %q", ev2.ClientName)
	}
}

func TestListFilterAndSort(t *testing.T) {
	m := newTestManager(t)
	m.Upsert(Event{ID: "1", ClientName: "Альфа", Amount: 100, OverallStatus: StatusOK, MsgDate: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)})
	m.Upsert(Event{ID: "2", ClientName: "Бета", Amount: 300, OverallStatus: StatusFailed, MsgDate: time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)})
	m.Upsert(Event{ID: "3", ClientName: "Альфа Плюс", Amount: 200, OverallStatus: StatusOK, MsgDate: time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)})

	// Фильтр по статусу.
	failed := m.List(ListOptions{Status: "ошибка"})
	if len(failed) != 1 || failed[0].ID != "2" {
		t.Errorf("фильтр по статусу вернул %d записей", len(failed))
	}

	// Фильтр по клиенту (подстрока, без учёта регистра).
	alpha := m.List(ListOptions{Client: "альфа"})
	if len(alpha) != 2 {
		t.Errorf("фильтр по клиенту вернул %d записей, ожидалось 2", len(alpha))
	}

	// Сортировка по сумме по возрастанию.
	bySum := m.List(ListOptions{SortBy: "amount", Descending: false})
	if bySum[0].Amount != 100 || bySum[2].Amount != 300 {
		t.Errorf("сортировка по сумме неверна: %v", []float64{bySum[0].Amount, bySum[1].Amount, bySum[2].Amount})
	}
}

func TestTraceSaveLoad(t *testing.T) {
	m := newTestManager(t)
	steps := []trace.Step{
		{Seq: 1, Kind: trace.KindPyrus, Method: "POST", Endpoint: "https://api/auth", StatusCode: 200},
		{Seq: 2, Kind: trace.KindIMAP, Method: "SEARCH", Endpoint: "IMAP SEARCH INBOX"},
	}
	if err := m.SaveTrace("msg@1", steps); err != nil {
		t.Fatalf("SaveTrace: %v", err)
	}
	got, err := m.LoadTrace("msg@1")
	if err != nil {
		t.Fatalf("LoadTrace: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ожидалось 2 шага, получено %d", len(got))
	}
	// Несуществующий трейс — nil без ошибки.
	none, err := m.LoadTrace("nope")
	if err != nil || none != nil {
		t.Errorf("LoadTrace для отсутствующего должен вернуть nil,nil, получено %v,%v", none, err)
	}
}

func TestCleanup(t *testing.T) {
	m := newTestManager(t)
	old := Event{ID: "old", MessageID: "old", CreatedAt: time.Now().AddDate(0, 0, -100)}
	m.Upsert(old)
	// Upsert перезапишет CreatedAt только если он нулевой; здесь он задан явно,
	// но Upsert для существующего сохраняет old.CreatedAt — здесь запись новая,
	// поэтому переданный CreatedAt сохраняется.
	m.Upsert(Event{ID: "fresh", MessageID: "fresh"})
	_ = m.SaveTrace("old", []trace.Step{{Seq: 1}})

	removed, err := m.Cleanup(30)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if removed != 1 {
		t.Errorf("удалено %d, ожидалось 1", removed)
	}
	if _, ok := m.Get("old"); ok {
		t.Error("старое событие не удалено")
	}
	if _, ok := m.Get("fresh"); !ok {
		t.Error("свежее событие ошибочно удалено")
	}
	if tr, _ := m.LoadTrace("old"); tr != nil {
		t.Error("трейс старого события не удалён")
	}
}

func TestEventTaskURL(t *testing.T) {
	ev := Event{TaskID: 123, ClientName: "ИП Эксперт"}
	url := ev.TaskURL()
	if url != "https://pyrus.com/t#id123 («ИП Эксперт»)" {
		t.Errorf("TaskURL = %q", url)
	}
	if (Event{}).TaskURL() != "" {
		t.Error("TaskURL при нулевом TaskID должен быть пустым")
	}
}
