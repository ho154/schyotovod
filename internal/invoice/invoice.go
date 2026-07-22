package invoice

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Типизированные ошибки парсинга — позволяют вызывающему коду понять, ЧТО
// именно не удалось распознать, и написать в лог/журнал точную причину, а не
// общее «информация не найдена».
var (
	// ErrTemplateNotMatched — тело письма вообще не подошло под шаблон
	// «Лицензии Cloud <клиент> № <номер> от <дата> (KZT) … на сумму <сумма> KZT».
	ErrTemplateNotMatched = errors.New("шаблон письма со счётом не распознан")
	// ErrClientEmpty — шаблон совпал, но наименование клиента пустое.
	ErrClientEmpty = errors.New("наименование клиента не распознано")
	// ErrAmountInvalid — не удалось преобразовать сумму в число.
	ErrAmountInvalid = errors.New("сумма счёта не распознана")
	// ErrFilenameNotMatched — имя файла вложения не подошло под шаблон
	// «Счет_на_оплату_№_<номер>_от_<день>_<месяц словом>_<год>».
	ErrFilenameNotMatched = errors.New("имя файла счёта не распознано")
)

// InvoiceInfo хранит результаты парсинга ТЕЛА письма.
//
// Важно: номер и дата ЛИЦЕНЗИИ (LicenseNo/LicenseDate) берутся из текста
// письма, а номер и дата СЧЁТА (InvoiceNo/InvoiceDate) — из имени PDF-файла
// вложения (см. ParseInvoiceFromFilename), это две разные сущности.
type InvoiceInfo struct {
	ClientName  string
	LicenseNo   string    // номер лицензии Cloud из текста письма, например "0707/2026-407"
	LicenseDate time.Time // дата лицензии из текста письма
	Amount      float64
}

// Регулярное выражение для поиска Лицензий Cloud, клиента, номера лицензии,
// даты лицензии и суммы в теле письма. Используем \x{00A0} для неразрывного
// пробела, а также \s.
var invoiceRegex = regexp.MustCompile(
	`Лицензии\s+Cloud\s+(?P<client>.+?)\s+№\s*(?P<license_no>\d+/\d{4}-\d+)\s+от\s+(?P<license_date>\d{2}\.\d{2}\.\d{4})\s*\(KZT\).*?на\s+сумму\s+(?P<amount>[\d\s\x{00A0}]+(?:[.,]\d+)?)\s*KZT`,
)

// ParseMessageBody анализирует текст письма и извлекает информацию о лицензии,
// клиенте и сумме. Номер и дата СЧЁТА берутся отдельно из имени файла.
func ParseMessageBody(body string) (*InvoiceInfo, error) {
	// Предварительно нормализуем переносы строк и пробелы в теле письма
	bodyNorm := strings.ReplaceAll(body, "\r\n", " ")
	bodyNorm = strings.ReplaceAll(bodyNorm, "\n", " ")

	matches := invoiceRegex.FindStringSubmatch(bodyNorm)
	if matches == nil {
		return nil, ErrTemplateNotMatched
	}

	clientIndex := invoiceRegex.SubexpIndex("client")
	licenseNoIndex := invoiceRegex.SubexpIndex("license_no")
	licenseDateIndex := invoiceRegex.SubexpIndex("license_date")
	amountIndex := invoiceRegex.SubexpIndex("amount")

	client := NormalizeString(matches[clientIndex])
	licenseNo := strings.TrimSpace(matches[licenseNoIndex])
	amountStr := matches[amountIndex]

	// Нормализуем сумму (удаляем любые виды пробелов: обычные, неразрывные, узкие и т.п.)
	amountStrNorm := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\u00A0' || r == '\u2009' || r == '\t' {
			return -1
		}
		if r == ',' {
			return '.'
		}
		return r
	}, amountStr)

	amount, err := strconv.ParseFloat(amountStrNorm, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: не удалось преобразовать %q в число", ErrAmountInvalid, amountStr)
	}

	if client == "" {
		return nil, ErrClientEmpty
	}

	// Дата лицензии (не критично, если не распарсится — оставим нулевой).
	var licenseDate time.Time
	if idx := licenseDateIndex; idx >= 0 && idx < len(matches) {
		if d, e := time.Parse("02.01.2006", matches[idx]); e == nil {
			licenseDate = d
		}
	}

	return &InvoiceInfo{
		ClientName:  client,
		LicenseNo:   licenseNo,
		LicenseDate: licenseDate,
		Amount:      amount,
	}, nil
}

// monthNames сопоставляет русские названия месяцев (в родительном падеже, как
// они пишутся в именах файлов «… от 20 июля 2026 …») числовому месяцу.
var monthNames = map[string]time.Month{
	"января":   time.January,
	"февраля":  time.February,
	"марта":    time.March,
	"апреля":   time.April,
	"мая":      time.May,
	"июня":     time.June,
	"июля":     time.July,
	"августа":  time.August,
	"сентября": time.September,
	"октября":  time.October,
	"ноября":   time.November,
	"декабря":  time.December,
}

// filenameRegex извлекает номер счёта и его дату (день, месяц словом, год) из
// имени PDF-файла вида «Счет_на_оплату_№_6954_от_20_июля_2026 г_.pdf».
// Разделители между словами могут быть подчёркиваниями или пробелами.
var filenameRegex = regexp.MustCompile(
	`№[_\s]*(?P<number>\d+)[_\s]+от[_\s]+(?P<day>\d{1,2})[_\s]+(?P<month>[А-Яа-яёЁ]+)[_\s]+(?P<year>\d{4})`,
)

// ParseInvoiceFromFilename извлекает номер и дату счёта из имени файла вложения.
// Возвращает номер (строкой) и дату. При несоответствии шаблону возвращает
// ErrFilenameNotMatched.
func ParseInvoiceFromFilename(filename string) (invoiceNo string, invoiceDate time.Time, err error) {
	m := filenameRegex.FindStringSubmatch(filename)
	if m == nil {
		return "", time.Time{}, fmt.Errorf("%w: %q", ErrFilenameNotMatched, filename)
	}
	numIdx := filenameRegex.SubexpIndex("number")
	dayIdx := filenameRegex.SubexpIndex("day")
	monthIdx := filenameRegex.SubexpIndex("month")
	yearIdx := filenameRegex.SubexpIndex("year")

	invoiceNo = m[numIdx]

	day, _ := strconv.Atoi(m[dayIdx])
	year, _ := strconv.Atoi(m[yearIdx])
	monthWord := strings.ToLower(m[monthIdx])
	month, ok := monthNames[monthWord]
	if !ok {
		// Номер распознали, а месяц — нет: возвращаем номер, но дату нулевую.
		return invoiceNo, time.Time{}, nil
	}
	invoiceDate = time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	return invoiceNo, invoiceDate, nil
}

// NormalizeString удаляет начальные/конечные пробелы и схлопывает внутренние пробелы
func NormalizeString(s string) string {
	// Заменяем неразрывные пробелы на обычные
	s = strings.ReplaceAll(s, "\u00A0", " ")
	s = strings.ReplaceAll(s, "\u2009", " ")

	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}
