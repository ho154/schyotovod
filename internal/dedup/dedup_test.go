package dedup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMarkAndCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dedup.json")
	d, err := New(path, time.UTC)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	id := "<msg-1@mail.gmail.com>"
	if d.IsProcessed(id) {
		t.Error("сообщение не должно быть отмечено до MarkProcessed")
	}
	if err := d.MarkProcessed(id); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}
	if !d.IsProcessed(id) {
		t.Error("сообщение должно быть отмечено после MarkProcessed")
	}
	if d.Count() != 1 {
		t.Errorf("Count = %d, ожидалось 1", d.Count())
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dedup.json")

	d1, _ := New(path, time.UTC)
	_ = d1.MarkProcessed("<a@x>")
	_ = d1.MarkProcessed("<b@x>")

	// Новый экземпляр читает тот же файл.
	d2, err := New(path, time.UTC)
	if err != nil {
		t.Fatalf("New (повторно): %v", err)
	}
	if !d2.IsProcessed("<a@x>") || !d2.IsProcessed("<b@x>") {
		t.Error("отметки должны сохраняться между запусками")
	}
}

func TestMonthReset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dedup.json")

	// Записываем файл вручную с прошлым месяцем.
	old := store{
		Month:               "2020-01",
		ProcessedMessageIDs: []string{"<old@x>"},
	}
	raw, _ := json.MarshalIndent(old, "", "  ")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("подготовка файла: %v", err)
	}

	// При загрузке месяц не совпадает с текущим — список должен сброситься.
	d, err := New(path, time.UTC)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if d.IsProcessed("<old@x>") {
		t.Error("отметка из прошлого месяца должна быть сброшена")
	}
	if d.Count() != 0 {
		t.Errorf("после сброса Count = %d, ожидалось 0", d.Count())
	}
}

func TestDuplicateMarkNoError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dedup.json")
	d, _ := New(path, time.UTC)

	_ = d.MarkProcessed("<dup@x>")
	if err := d.MarkProcessed("<dup@x>"); err != nil {
		t.Errorf("повторная отметка не должна возвращать ошибку: %v", err)
	}
	if d.Count() != 1 {
		t.Errorf("Count = %d, ожидалось 1 (без дублей)", d.Count())
	}
}
