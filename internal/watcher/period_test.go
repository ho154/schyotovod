package watcher

import (
	"testing"
	"time"
)

func almaty(t *testing.T) *time.Location {
	loc, err := time.LoadLocation("Asia/Almaty")
	if err != nil {
		t.Fatalf("не удалось загрузить часовой пояс: %v", err)
	}
	return loc
}

func TestInPeriod(t *testing.T) {
	loc := almaty(t)
	cases := []struct {
		name     string
		day      int
		start    int
		end      int
		expected bool
	}{
		{"до периода", 19, 20, 29, false},
		{"начало периода", 20, 20, 29, true},
		{"середина периода", 25, 20, 29, true},
		{"конец периода", 29, 20, 29, true},
		{"после периода", 30, 20, 29, false},
		{"первое число", 1, 20, 29, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now := time.Date(2025, time.July, c.day, 12, 0, 0, 0, loc)
			if got := InPeriod(now, c.start, c.end); got != c.expected {
				t.Errorf("InPeriod(day=%d, %d-%d) = %v, ожидалось %v", c.day, c.start, c.end, got, c.expected)
			}
		})
	}
}

func TestInPeriodClampsToMonthEnd(t *testing.T) {
	loc := almaty(t)
	// Февраль 2025 — 28 дней. end=31 должен ограничиться до 28.
	now := time.Date(2025, time.February, 28, 12, 0, 0, 0, loc)
	if !InPeriod(now, 20, 31) {
		t.Error("28 февраля должно попадать в период 20-31 (ограниченный концом месяца)")
	}
}

func TestPeriodBounds(t *testing.T) {
	loc := almaty(t)
	now := time.Date(2025, time.July, 25, 15, 30, 0, 0, loc)
	since, before := PeriodBounds(now, 20, 29)

	wantSince := time.Date(2025, time.July, 20, 0, 0, 0, 0, loc)
	wantBefore := time.Date(2025, time.July, 30, 0, 0, 0, 0, loc) // 29+1

	if !since.Equal(wantSince) {
		t.Errorf("since = %v, ожидалось %v", since, wantSince)
	}
	if !before.Equal(wantBefore) {
		t.Errorf("before = %v, ожидалось %v", before, wantBefore)
	}
}

func TestInPeriodByDate(t *testing.T) {
	loc := almaty(t)
	now := time.Date(2025, time.July, 25, 12, 0, 0, 0, loc)

	// Письмо из текущего месяца в периоде.
	inDate := time.Date(2025, time.July, 22, 9, 0, 0, 0, loc)
	if !InPeriodByDate(inDate, now, 20, 29) {
		t.Error("письмо от 22 июля должно попадать в период 20-29")
	}

	// Письмо из прошлого месяца — не попадает.
	prevMonth := time.Date(2025, time.June, 25, 9, 0, 0, 0, loc)
	if InPeriodByDate(prevMonth, now, 20, 29) {
		t.Error("письмо из прошлого месяца не должно попадать в период")
	}

	// Письмо вне диапазона дней.
	outDay := time.Date(2025, time.July, 5, 9, 0, 0, 0, loc)
	if InPeriodByDate(outDay, now, 20, 29) {
		t.Error("письмо от 5 июля не должно попадать в период 20-29")
	}
}

func TestInPeriodByDateTimezoneConversion(t *testing.T) {
	loc := almaty(t)
	now := time.Date(2025, time.July, 20, 2, 0, 0, 0, loc)

	// Письмо доставлено 19 июля 21:00 UTC = 20 июля 02:00 по Алматы (UTC+5).
	// По алматинскому времени это уже 20-е число — должно попадать в период.
	msgUTC := time.Date(2025, time.July, 19, 21, 0, 0, 0, time.UTC)
	if !InPeriodByDate(msgUTC, now, 20, 29) {
		t.Error("письмо 19 июля 21:00 UTC = 20 июля по Алматы, должно попадать в период 20-29")
	}
}
