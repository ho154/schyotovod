// Package auth реализует аутентификацию администратора веб-панели:
// хеширование пароля через bcrypt и управление сессиями через cookie.
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// sessionTTL — срок жизни сессии.
const sessionTTL = 12 * time.Hour

// HashPassword возвращает bcrypt-хеш пароля.
func HashPassword(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// CheckPassword сверяет пароль с bcrypt-хешем.
func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// GenerateToken генерирует случайный токен (для сессий, временных паролей и т.п.).
func GenerateToken(nBytes int) (string, error) {
	if nBytes <= 0 {
		nBytes = 32
	}
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// GenerateReadablePassword генерирует читаемый пароль без похожих символов (0/O, 1/l/I).
func GenerateReadablePassword(length int) (string, error) {
	if length <= 0 {
		length = 12
	}
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b), nil
}

// session — активная сессия администратора.
type session struct {
	expires time.Time
}

// SessionStore хранит активные сессии в памяти.
type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]session
}

// NewSessionStore создаёт хранилище сессий.
func NewSessionStore() *SessionStore {
	return &SessionStore{sessions: make(map[string]session)}
}

// Create создаёт новую сессию и возвращает её токен.
func (s *SessionStore) Create() (string, error) {
	token, err := GenerateToken(32)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.sessions[token] = session{expires: time.Now().Add(sessionTTL)}
	s.mu.Unlock()
	return token, nil
}

// Valid проверяет, действительна ли сессия по токену.
func (s *SessionStore) Valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(sess.expires) {
		delete(s.sessions, token)
		return false
	}
	return true
}

// Delete удаляет сессию (при выходе).
func (s *SessionStore) Delete(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

// Clear удаляет все сессии (например, после смены пароля).
func (s *SessionStore) Clear() {
	s.mu.Lock()
	s.sessions = make(map[string]session)
	s.mu.Unlock()
}

// CleanupExpired удаляет истёкшие сессии.
func (s *SessionStore) CleanupExpired() {
	now := time.Now()
	s.mu.Lock()
	for t, sess := range s.sessions {
		if now.After(sess.expires) {
			delete(s.sessions, t)
		}
	}
	s.mu.Unlock()
}
