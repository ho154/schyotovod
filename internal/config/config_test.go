package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtractFormID(t *testing.T) {
	tests := []struct {
		input    string
		expected int
	}{
		{"12345", 12345},
		{"  67890  ", 67890},
		{"https://pyrus.com/t#uf1089723", 1089723},
		{"https://pyrus.com/t#id123456", 123456},
		{"invalid-string", 0},
		{"", 0},
	}

	for _, tt := range tests {
		got := ExtractFormID(tt.input)
		if got != tt.expected {
			t.Errorf("ExtractFormID(%q) = %d, expected %d", tt.input, got, tt.expected)
		}
	}
}

func TestConfigLocation(t *testing.T) {
	c := Default()

	// Default timezone
	loc, err := c.Location()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loc.String() != "Asia/Almaty" {
		t.Errorf("expected Asia/Almaty, got %s", loc.String())
	}

	// Invalid timezone
	c.General.Timezone = "Invalid/Timezone"
	loc, err = c.Location()
	if err == nil {
		t.Error("expected error for invalid timezone, got nil")
	}
	if loc.String() != "UTC" {
		t.Errorf("expected fallback to UTC, got %s", loc.String())
	}
}

func TestConfigReadyAndConfigured(t *testing.T) {
	c := Default()

	// Initially, it should not be ready/configured since key parameters are empty/zero
	if c.IsReady() {
		t.Error("expected IsReady() to be false")
	}
	if c.IsGmailConfigured() {
		t.Error("expected IsGmailConfigured() to be false")
	}
	if c.IsPyrusConfigured() {
		t.Error("expected IsPyrusConfigured() to be false")
	}

	// Configure Gmail
	c.Gmail.Email = "test@gmail.com"
	c.Gmail.AppPassword = "app-password"
	c.Filter.SenderEmail = "sender@gmail.com"

	if !c.IsGmailConfigured() {
		t.Error("expected IsGmailConfigured() to be true")
	}
	if c.IsReady() {
		t.Error("expected IsReady() to be false (Pyrus not configured yet)")
	}

	// Configure Pyrus
	c.Pyrus.Login = "bot-login"
	c.Pyrus.SecurityKey = "sec-key"
	c.Pyrus.FormID = 111
	c.Pyrus.AttachmentFieldID = 222
	c.Pyrus.ClientNameFieldID = 333
	c.Pyrus.AmountFieldID = 444

	if !c.IsPyrusConfigured() {
		t.Error("expected IsPyrusConfigured() to be true")
	}
	if !c.IsReady() {
		t.Error("expected IsReady() to be true")
	}
}

func TestManagerLoadSave(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "config-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	configPath := filepath.Join(tmpDir, "config.json")

	// 1. NewManager with non-existent path (loads default config)
	m, err := NewManager(configPath)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	if m.Path() != configPath {
		t.Errorf("expected path %q, got %q", configPath, m.Path())
	}

	cfg := m.Get()
	if cfg.Gmail.IMAPHost != "imap.gmail.com" {
		t.Errorf("expected default IMAP host, got %q", cfg.Gmail.IMAPHost)
	}

	// 2. Save config
	cfg.Gmail.Email = "manager-test@gmail.com"
	if err := m.Set(cfg); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Fatalf("config file was not created: %v", err)
	}

	// 3. NewManager with existing path (loads saved config)
	m2, err := NewManager(configPath)
	if err != nil {
		t.Fatalf("NewManager on existing file failed: %v", err)
	}

	cfg2 := m2.Get()
	if cfg2.Gmail.Email != "manager-test@gmail.com" {
		t.Errorf("expected loaded Email %q, got %q", "manager-test@gmail.com", cfg2.Gmail.Email)
	}

	// 4. Test Load manually
	m3 := &Manager{path: configPath}
	if err := m3.Load(); err != nil {
		t.Fatalf("Load manually failed: %v", err)
	}
	if m3.Get().Gmail.Email != "manager-test@gmail.com" {
		t.Errorf("expected loaded Email %q, got %q", "manager-test@gmail.com", m3.Get().Gmail.Email)
	}
}
