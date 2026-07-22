package invoice

import (
	"testing"
)

func TestParseMessageBody(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantClient string
		wantNo     string
		wantAmount float64
		wantErr    bool
	}{
		{
			name: "Шаблон письма из запроса пользователя с неразрывными пробелами",
			body: `Здравствуйте, ЖИВАЕВ ДАНИИЛ НИКОЛАЕВИЧ !

Увеличение цены iiko с 01.03.2026.
Счета по новым ценам с февраля 2026.

Во вложении счёт на оплату за Лицензии Cloud ИП Эксперт № 0707/2026-407 от 07.07.2026 (KZT) на следующий месяц на сумму 29 510 KZT .

Во избежание остановки лицензий и обслуживания просьба оплатить до 25 числа текущего месяца.`,
			wantClient: "ИП Эксперт",
			wantNo:     "0707/2026-407",
			wantAmount: 29510,
			wantErr:    false,
		},
		{
			name:       "Другой клиент и сумма с неразрывным пробелом",
			body:       "Во вложении счёт на оплату за Лицензии Cloud Донер на Абрая 153 № 1234/2026-99 от 15.08.2026 (KZT) на следующий месяц на сумму 150 000 KZT .", // содержит \u00A0
			wantClient: "Донер на Абрая 153",
			wantNo:     "1234/2026-99",
			wantAmount: 150000,
			wantErr:    false,
		},
		{
			name:       "Клиент с ключевым словом Cloud внутри названия",
			body:       "Во вложении счёт на оплату за Лицензии Cloud Облако Cloud LLC № 9999/2026-11 от 01.09.2026 (KZT) на следующий месяц на сумму 5 500 KZT .",
			wantClient: "Облако Cloud LLC",
			wantNo:     "9999/2026-11",
			wantAmount: 5500,
			wantErr:    false,
		},
		{
			name:    "Невалидный формат",
			body:    "Привет, во вложении какой-то файл",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMessageBody(tt.body)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseMessageBody() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if got.ClientName != tt.wantClient {
					t.Errorf("ClientName = %q, want %q", got.ClientName, tt.wantClient)
				}
				if got.InvoiceNo != tt.wantNo {
					t.Errorf("InvoiceNo = %q, want %q", got.InvoiceNo, tt.wantNo)
				}
				if got.Amount != tt.wantAmount {
					t.Errorf("Amount = %v, want %v", got.Amount, tt.wantAmount)
				}
			}
		})
	}
}
