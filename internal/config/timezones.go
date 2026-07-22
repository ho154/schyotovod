package config

// TimezoneOption — один пункт выпадающего списка часовых поясов в веб-панели.
// Value — это IANA-идентификатор (например, "Asia/Almaty"), который реально
// используется в коде для конвертации времени (time.LoadLocation). Label —
// человекочитаемое название с городом и смещением GMT для отображения в
// интерфейсе (GMT-обозначение используется только для внешнего вида,
// в самом коде часовые расчёты по-прежнему выполняются через IANA tz database).
type TimezoneOption struct {
	Value string
	Label string
}

// Timezones — список часовых поясов для выпадающего списка в настройках.
// Список составлен по наиболее употребимым городам/поясам стран СНГ и мира,
// с фиксированным смещением GMT в подписи (без летнего времени, т.к. большинство
// современных зон его не используют).
var Timezones = []TimezoneOption{
	{Value: "Pacific/Midway", Label: "(GMT-11:00) Мидуэй, Самоа"},
	{Value: "Pacific/Honolulu", Label: "(GMT-10:00) Гонолулу"},
	{Value: "America/Anchorage", Label: "(GMT-09:00) Анкоридж"},
	{Value: "America/Los_Angeles", Label: "(GMT-08:00) Лос-Анджелес, Ванкувер"},
	{Value: "America/Denver", Label: "(GMT-07:00) Денвер, Феникс"},
	{Value: "America/Chicago", Label: "(GMT-06:00) Чикаго, Мехико"},
	{Value: "America/New_York", Label: "(GMT-05:00) Нью-Йорк, Торонто"},
	{Value: "America/Halifax", Label: "(GMT-04:00) Галифакс, Каракас"},
	{Value: "America/Sao_Paulo", Label: "(GMT-03:00) Сан-Паулу, Буэнос-Айрес"},
	{Value: "Atlantic/Azores", Label: "(GMT-01:00) Азорские острова"},
	{Value: "Europe/London", Label: "(GMT+00:00) Лондон, Лиссабон"},
	{Value: "Europe/Paris", Label: "(GMT+01:00) Париж, Берлин, Мадрид"},
	{Value: "Europe/Kaliningrad", Label: "(GMT+02:00) Калининград, Киев, Каир"},
	{Value: "Europe/Moscow", Label: "(GMT+03:00) Москва, Стамбул, Минск"},
	{Value: "Asia/Dubai", Label: "(GMT+04:00) Дубай, Тбилиси, Ереван, Баку"},
	{Value: "Asia/Yekaterinburg", Label: "(GMT+05:00) Екатеринбург, Ташкент, Душанбе"},
	{Value: "Asia/Almaty", Label: "(GMT+05:00) Алматы, Астана, Бишкек"},
	{Value: "Asia/Karachi", Label: "(GMT+05:00) Карачи, Исламабад"},
	{Value: "Asia/Kolkata", Label: "(GMT+05:30) Дели, Мумбаи"},
	{Value: "Asia/Dhaka", Label: "(GMT+06:00) Дакка, Омск"},
	{Value: "Asia/Bangkok", Label: "(GMT+07:00) Бангкок, Джакарта, Красноярск"},
	{Value: "Asia/Shanghai", Label: "(GMT+08:00) Шанхай, Сингапур, Иркутск"},
	{Value: "Asia/Tokyo", Label: "(GMT+09:00) Токио, Сеул, Якутск"},
	{Value: "Australia/Sydney", Label: "(GMT+10:00) Сидней, Владивосток"},
	{Value: "Asia/Magadan", Label: "(GMT+11:00) Магадан, Соломоновы острова"},
	{Value: "Pacific/Auckland", Label: "(GMT+12:00) Окленд, Камчатка"},
	{Value: "UTC", Label: "(GMT+00:00) UTC (всемирное координированное время)"},
}
