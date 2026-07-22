// Package gmail реализует работу с почтой Gmail по IMAP:
// подключение через App Password, поиск писем по отправителю и периоду,
// извлечение вложений (счетов). Режим IMAP IDLE и цикл наблюдения
// реализованы в watcher.go.
//
// Документация по Gmail IMAP собрана в docs/gmail-imap/.
package gmail

import (
	"fmt"
	"io"
	"mime"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // регистрация кодировок (в т.ч. для кириллицы)
	"github.com/emersion/go-message/mail"
)

// Attachment — извлечённое вложение письма.
type Attachment struct {
	Filename string
	Content  []byte
}

// Message — сведения о письме, релевантные для обработки.
type Message struct {
	SeqNum      uint32
	UID         imap.UID
	MessageID   string
	From        string
	Subject     string
	Date        time.Time
	Body        string
	Attachments []Attachment
}

// DialConfig — параметры подключения к IMAP.
type DialConfig struct {
	Host        string
	Port        int
	Email       string
	AppPassword string
}

// Address формирует строку host:port для подключения.
func (d DialConfig) Address() string {
	host := d.Host
	if host == "" {
		host = "imap.gmail.com"
	}
	port := d.Port
	if port == 0 {
		port = 993
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// Connect подключается к серверу по TLS и выполняет авторизацию через App Password.
// mailboxHandler (может быть nil) вызывается при unilateral-обновлениях мейлбокса
// (используется для IMAP IDLE, чтобы узнать о новых письмах).
func Connect(cfg DialConfig, mailboxHandler func()) (*imapclient.Client, error) {
	var opts imapclient.Options
	if mailboxHandler != nil {
		opts.UnilateralDataHandler = &imapclient.UnilateralDataHandler{
			Mailbox: func(_ *imapclient.UnilateralDataMailbox) {
				mailboxHandler()
			},
		}
	}

	client, err := imapclient.DialTLS(cfg.Address(), &opts)
	if err != nil {
		return nil, fmt.Errorf("не удалось подключиться к почтовому серверу %s: %w", cfg.Address(), err)
	}

	if err := client.Login(cfg.Email, cfg.AppPassword).Wait(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ошибка авторизации на почте (проверьте адрес и пароль приложения): %w", err)
	}
	return client, nil
}

// SelectInbox выбирает папку INBOX.
func SelectInbox(client *imapclient.Client) error {
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		return fmt.Errorf("не удалось открыть папку «Входящие»: %w", err)
	}
	return nil
}

// SearchCriteria описывает условия поиска писем.
type SearchCriteria struct {
	SenderEmail string
	Since       time.Time // дата начала периода (включительно)
	Before      time.Time // дата конца периода (не включая этот день)
}

// Search выполняет поиск писем по отправителю и диапазону дат.
// Возвращает список UID подходящих писем.
func Search(client *imapclient.Client, c SearchCriteria) ([]imap.UID, error) {
	criteria := &imap.SearchCriteria{}
	if c.SenderEmail != "" {
		criteria.Header = append(criteria.Header, imap.SearchCriteriaHeaderField{
			Key:   "From",
			Value: c.SenderEmail,
		})
	}
	if !c.Since.IsZero() {
		criteria.Since = c.Since
	}
	if !c.Before.IsZero() {
		criteria.Before = c.Before
	}

	data, err := client.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("ошибка поиска писем: %w", err)
	}
	return data.AllUIDs(), nil
}

// FetchMessage скачивает письмо целиком по UID и разбирает его MIME-структуру,
// извлекая метаданные и вложения (части с Content-Disposition: attachment).
func FetchMessage(client *imapclient.Client, uid imap.UID) (*Message, error) {
	uidSet := imap.UIDSetNum(uid)
	opts := &imap.FetchOptions{
		Envelope:    true,
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{{}}, // всё письмо целиком
	}

	msgs, err := client.Fetch(uidSet, opts).Collect()
	if err != nil {
		return nil, fmt.Errorf("не удалось скачать письмо: %w", err)
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("письмо с UID %v не найдено", uid)
	}
	buf := msgs[0]

	m := &Message{
		SeqNum: buf.SeqNum,
		UID:    buf.UID,
	}
	if buf.Envelope != nil {
		m.MessageID = strings.Trim(buf.Envelope.MessageID, "<>")
		m.Subject = buf.Envelope.Subject
		m.Date = buf.Envelope.Date
		if len(buf.Envelope.From) > 0 {
			addr := buf.Envelope.From[0]
			m.From = addr.Addr()
		}
	}

	// Находим тело письма (единственная запрошенная секция).
	var raw []byte
	for _, bs := range buf.BodySection {
		raw = bs.Bytes
		break
	}
	if raw == nil {
		return m, nil
	}

	bodyText, attachments, err := extractMessageParts(raw)
	if err != nil {
		return m, fmt.Errorf("не удалось разобрать тело и вложения письма: %w", err)
	}
	m.Body = bodyText
	m.Attachments = attachments
	return m, nil
}

// extractAttachments разбирает сырое MIME-письмо и возвращает вложения
// (части с Content-Disposition: attachment). Inline-контент пропускается.
func extractMessageParts(raw []byte) (string, []Attachment, error) {
	mr, err := mail.CreateReader(strings.NewReader(string(raw)))
	if err != nil {
		// Не удалось разобрать как multipart mail. Попробуем извлечь как простое body
		return string(raw), nil, nil
	}

	var attachments []Attachment
	var bodyBuilder strings.Builder

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if message.IsUnknownCharset(err) {
				continue
			}
			break
		}

		switch h := part.Header.(type) {
		case *mail.AttachmentHeader:
			filename, _ := h.Filename()
			filename = decodeFilename(filename)
			content, err := io.ReadAll(part.Body)
			if err != nil {
				continue
			}
			if filename == "" {
				filename = "attachment"
			}
			attachments = append(attachments, Attachment{Filename: filename, Content: content})
		case *mail.InlineHeader:
			contentType, _, _ := h.ContentType()
			content, err := io.ReadAll(part.Body)
			if err != nil {
				continue
			}
			// Извлекаем текстовую часть (предпочтительно plain text)
			if contentType == "text/plain" {
				bodyBuilder.Write(content)
			} else if contentType == "text/html" && bodyBuilder.Len() == 0 {
				// Если text/plain еще не было, временно берем HTML (позже можно почистить от тегов при надобности, но для regex сойдет)
				bodyBuilder.Write(content)
			}
		}
	}
	return bodyBuilder.String(), attachments, nil
}

// decodeFilename декодирует имя файла из MIME-кодировки (RFC 2047), если нужно.
func decodeFilename(name string) string {
	if name == "" {
		return ""
	}
	dec := new(mime.WordDecoder)
	if decoded, err := dec.DecodeHeader(name); err == nil {
		return decoded
	}
	return name
}
