package auth

import (
	"testing"
	"time"
)

func TestHashAndCheckPassword(t *testing.T) {
	password := "my-secure-password"

	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword failed: %v", err)
	}

	if hash == "" {
		t.Error("HashPassword returned empty hash")
	}

	if !CheckPassword(hash, password) {
		t.Error("CheckPassword failed to verify correct password")
	}

	if CheckPassword(hash, "wrong-password") {
		t.Error("CheckPassword verified incorrect password")
	}
}

func TestGenerateToken(t *testing.T) {
	t.Run("default length", func(t *testing.T) {
		token, err := GenerateToken(0)
		if err != nil {
			t.Fatalf("GenerateToken failed: %v", err)
		}
		if len(token) == 0 {
			t.Error("GenerateToken returned empty token")
		}
	})

	t.Run("specific length", func(t *testing.T) {
		token, err := GenerateToken(16)
		if err != nil {
			t.Fatalf("GenerateToken failed: %v", err)
		}
		if len(token) == 0 {
			t.Error("GenerateToken returned empty token")
		}
	})
}

func TestGenerateReadablePassword(t *testing.T) {
	t.Run("default length", func(t *testing.T) {
		pass, err := GenerateReadablePassword(0)
		if err != nil {
			t.Fatalf("GenerateReadablePassword failed: %v", err)
		}
		if len(pass) != 12 {
			t.Errorf("expected password length 12, got %d", len(pass))
		}
	})

	t.Run("specific length", func(t *testing.T) {
		pass, err := GenerateReadablePassword(16)
		if err != nil {
			t.Fatalf("GenerateReadablePassword failed: %v", err)
		}
		if len(pass) != 16 {
			t.Errorf("expected password length 16, got %d", len(pass))
		}
	})
}

func TestSessionStore(t *testing.T) {
	store := NewSessionStore()

	// Создание сессии
	token, err := store.Create()
	if err != nil {
		t.Fatalf("Create session failed: %v", err)
	}

	if !store.Valid(token) {
		t.Error("Session should be valid after creation")
	}

	if store.Valid("non-existent-token") {
		t.Error("Non-existent session should not be valid")
	}

	if store.Valid("") {
		t.Error("Empty token should not be valid")
	}

	// Удаление одной сессии
	store.Delete(token)
	if store.Valid(token) {
		t.Error("Session should not be valid after deletion")
	}

	// Создание нескольких сессий
	token1, _ := store.Create()
	token2, _ := store.Create()

	if !store.Valid(token1) || !store.Valid(token2) {
		t.Error("Both sessions should be valid")
	}

	// Очистка всех сессий
	store.Clear()
	if store.Valid(token1) || store.Valid(token2) {
		t.Error("Sessions should not be valid after clear")
	}
}

func TestSessionStoreExpirationAndCleanup(t *testing.T) {
	store := NewSessionStore()

	// Вручную добавим истекшую сессию для тестирования
	store.mu.Lock()
	expiredToken := "expired-token"
	store.sessions[expiredToken] = session{
		expires: time.Now().Add(-1 * time.Hour),
	}
	validToken := "valid-token"
	store.sessions[validToken] = session{
		expires: time.Now().Add(1 * time.Hour),
	}
	store.mu.Unlock()

	// Проверяем валидность
	if store.Valid(expiredToken) {
		t.Error("Expired session should not be valid")
	}

	// Valid() должен был удалить expiredToken из мапы
	store.mu.Lock()
	_, exists := store.sessions[expiredToken]
	store.mu.Unlock()
	if exists {
		t.Error("Valid() did not remove expired session")
	}

	// Добавим снова expired-token для проверки CleanupExpired
	store.mu.Lock()
	store.sessions[expiredToken] = session{
		expires: time.Now().Add(-1 * time.Hour),
	}
	store.mu.Unlock()

	// Запуск ручной очистки
	store.CleanupExpired()

	store.mu.Lock()
	_, expiredExists := store.sessions[expiredToken]
	_, validExists := store.sessions[validToken]
	store.mu.Unlock()

	if expiredExists {
		t.Error("CleanupExpired did not remove expired session")
	}
	if !validExists {
		t.Error("CleanupExpired removed valid session")
	}
}
