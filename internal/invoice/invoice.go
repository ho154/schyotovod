package invoice

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// InvoiceInfo хранит результаты парсинга письма.
type InvoiceInfo struct {
	ClientName string
	InvoiceNo  string
	Amount     float64
}

// Регулярное выражение для поиска Лицензий Cloud, клиента, номера счета и суммы.
// Используем \x{00A0} для поиска неразрывного пробела, а также \s.
var invoiceRegex = regexp.MustCompile(
	`Лицензии\s+Cloud\s+(?P<client>.+?)\s+№\s*(?P<invoice_no>\d+/\d{4}-\d+)\s+от\s+\d{2}\.\d{2}\.\d{4}\s*\(KZT\).*?на\s+сумму\s+(?P<amount>[\d\s\x{00A0}]+(?:[.,]\d+)?)\s*KZT`,
)

// ParseMessageBody анализирует текст письма и извлекает информацию о счете.
func ParseMessageBody(body string) (*InvoiceInfo, error) {
	// Предварительно нормализуем переносы строк и пробелы в теле письма
	bodyNorm := strings.ReplaceAll(body, "\r\n", " ")
	bodyNorm = strings.ReplaceAll(bodyNorm, "\n", " ")

	matches := invoiceRegex.FindStringSubmatch(bodyNorm)
	if matches == nil {
		return nil, fmt.Errorf("информация о счете не найдена в тексте письма")
	}

	clientIndex := invoiceRegex.SubexpIndex("client")
	invoiceNoIndex := invoiceRegex.SubexpIndex("invoice_no")
	amountIndex := invoiceRegex.SubexpIndex("amount")

	client := NormalizeString(matches[clientIndex])
	invoiceNo := strings.TrimSpace(matches[invoiceNoIndex])
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
		return nil, fmt.Errorf("не удалось преобразовать сумму %q в число: %w", amountStr, err)
	}

	if client == "" {
		return nil, fmt.Errorf("наименование клиента пустое")
	}

	return &InvoiceInfo{
		ClientName: client,
		InvoiceNo:  invoiceNo,
		Amount:     amount,
	}, nil
}

// NormalizeString удаляет начальные/конечные пробелы и схлопывает внутренние пробелы
func NormalizeString(s string) string {
	// Заменяем неразрывные пробелы на обычные
	s = strings.ReplaceAll(s, "\u00A0", " ")
	s = strings.ReplaceAll(s, "\u2009", " ")

	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}
