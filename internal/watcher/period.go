// Package watcher реализует наблюдение за почтой: цикл IMAP IDLE с
// автопереустановкой, резервный поллинг и обработку писем со счетами.
package watcher

import "time"

// InPeriod сообщает, находится ли момент now (в часовом поясе loc) внутри
// активного периода проверки [startDay; endDay] текущего месяца.
//
// Учитывает случай, когда endDay превышает число дней в месяце (например,
// endDay=31 в феврале) — тогда верхняя граница ограничивается последним днём месяца.
func InPeriod(now time.Time, startDay, endDay int) bool {
	day := now.Day()
	lastDay := daysInMonth(now.Year(), now.Month(), now.Location())

	start := clamp(startDay, 1, lastDay)
	end := clamp(endDay, 1, lastDay)
	if start > end {
		start, end = end, start
	}
	return day >= start && day <= end
}

// PeriodBounds возвращает границы периода [since; before) для поиска писем
// за текущий месяц. since — начало дня startDay, before — начало дня, следующего
// за endDay (верхняя граница не включается). Используется в IMAP SEARCH
// (учитывается только дата, без времени).
func PeriodBounds(now time.Time, startDay, endDay int) (since, before time.Time) {
	loc := now.Location()
	year, month := now.Year(), now.Month()
	lastDay := daysInMonth(year, month, loc)

	start := clamp(startDay, 1, lastDay)
	end := clamp(endDay, 1, lastDay)
	if start > end {
		start, end = end, start
	}

	since = time.Date(year, month, start, 0, 0, 0, 0, loc)
	// before — начало дня, следующего за end (полночь end+1).
	before = time.Date(year, month, end, 0, 0, 0, 0, loc).AddDate(0, 0, 1)
	return since, before
}

// InPeriodByDate проверяет, попадает ли дата письма msgDate (приведённая к loc)
// в период [startDay; endDay] текущего месяца now. Используется для точной
// проверки по заголовку Date письма после выборки (IMAP SEARCH по дате
// работает грубее — по дате доставки в TZ сервера).
func InPeriodByDate(msgDate, now time.Time, startDay, endDay int) bool {
	loc := now.Location()
	local := msgDate.In(loc)
	// Письмо должно относиться к текущему месяцу и попадать в диапазон дней.
	if local.Year() != now.Year() || local.Month() != now.Month() {
		return false
	}
	return InPeriod(local, startDay, endDay)
}

// daysInMonth возвращает число дней в указанном месяце.
func daysInMonth(year int, month time.Month, loc *time.Location) int {
	// Первый день следующего месяца минус один день = последний день текущего.
	firstNext := time.Date(year, month+1, 1, 0, 0, 0, 0, loc)
	last := firstNext.AddDate(0, 0, -1)
	return last.Day()
}

// clamp ограничивает значение диапазоном [min; max].
func clamp(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
