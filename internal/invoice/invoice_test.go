package invoice

import (
	"errors"
	"testing"
	"time"
)

func TestParseMessageBody(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantClient  string
		wantLicense string
		wantAmount  float64
		wantErr     error // nil = без ошибки; иначе конкретная типизированная ошибка
	}{
		{
			name: "Шаблон письма из запроса пользователя с неразрывными пробелами",
			body: `Здравствуйте, ЖИВАЕВ ДАНИИЛ НИКОЛАЕВИЧ !

Увеличение цены iiko с 01.03.2026.
Счета по новым ценам с февраля 2026.

Во вложении счёт на оплату за Лицензии Cloud ИП Эксперт № 0707/2026-407 от 07.07.2026 (KZT) на следующий месяц на сумму 29 510 KZT .

Во избежание остановки лицензий и обслуживания просьба оплатить до 25 числа текущего месяца.`,
			wantClient:  "ИП Эксперт",
			wantLicense: "0707/2026-407",
			wantAmount:  29510,
		},
		{
			name:        "Другой клиент и сумма с неразрывным пробелом",
			body:        "Во вложении счёт на оплату за Лицензии Cloud Донер на Абрая 153 № 1234/2026-99 от 15.08.2026 (KZT) на следующий месяц на сумму 150 000 KZT .",
			wantClient:  "Донер на Абрая 153",
			wantLicense: "1234/2026-99",
			wantAmount:  150000,
		},
		{
			name:        "Клиент с ключевым словом Cloud внутри названия",
			body:        "Во вложении счёт на оплату за Лицензии Cloud Облако Cloud LLC № 9999/2026-11 от 01.09.2026 (KZT) на следующий месяц на сумму 5 500 KZT .",
			wantClient:  "Облако Cloud LLC",
			wantLicense: "9999/2026-11",
			wantAmount:  5500,
		},
		{
			name:    "Невалидный формат",
			body:    "Привет, во вложении какой-то файл",
			wantErr: ErrTemplateNotMatched,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMessageBody(tt.body)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("ParseMessageBody() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseMessageBody() неожиданная ошибка: %v", err)
			}
			if got.ClientName != tt.wantClient {
				t.Errorf("ClientName = %q, want %q", got.ClientName, tt.wantClient)
			}
			if got.LicenseNo != tt.wantLicense {
				t.Errorf("LicenseNo = %q, want %q", got.LicenseNo, tt.wantLicense)
			}
			if got.Amount != tt.wantAmount {
				t.Errorf("Amount = %v, want %v", got.Amount, tt.wantAmount)
			}
		})
	}
}

func TestParseMessageBodyLicenseDate(t *testing.T) {
	body := "Лицензии Cloud ИП Эксперт № 0707/2026-407 от 07.07.2026 (KZT) на сумму 29 510 KZT"
	got, err := ParseMessageBody(body)
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	want := time.Date(2026, time.July, 7, 0, 0, 0, 0, time.UTC)
	if !got.LicenseDate.Equal(want) {
		t.Errorf("LicenseDate = %v, want %v", got.LicenseDate, want)
	}
}

func TestParseInvoiceFromFilename(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		wantNo   string
		wantDate time.Time
		wantErr  error
	}{
		{
			name:     "Формат с подчёркиваниями из письма пользователя",
			filename: "Счет_на_оплату_№_6954_от_20_июля_2026 г_.pdf",
			wantNo:   "6954",
			wantDate: time.Date(2026, time.July, 20, 0, 0, 0, 0, time.UTC),
		},
		{
			name:     "Формат с пробелами",
			filename: "Счет на оплату № 12345 от 3 марта 2026 г.pdf",
			wantNo:   "12345",
			wantDate: time.Date(2026, time.March, 3, 0, 0, 0, 0, time.UTC),
		},
		{
			name:     "Другой месяц (декабрь), номер длинный",
			filename: "Счет_на_оплату_№_100500_от_31_декабря_2026_г.pdf",
			wantNo:   "100500",
			wantDate: time.Date(2026, time.December, 31, 0, 0, 0, 0, time.UTC),
		},
		{
			name:     "Имя файла не подходит под шаблон",
			filename: "Документ_без_номера.pdf",
			wantErr:  ErrFilenameNotMatched,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			no, date, err := ParseInvoiceFromFilename(tt.filename)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			if no != tt.wantNo {
				t.Errorf("InvoiceNo = %q, want %q", no, tt.wantNo)
			}
			if !date.Equal(tt.wantDate) {
				t.Errorf("InvoiceDate = %v, want %v", date, tt.wantDate)
			}
		})
	}
}
