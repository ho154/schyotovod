// Package version хранит информацию о версии сборки.
// Значения подставляются при компиляции через -ldflags.
package version

// Version — версия сборки (например, "v1.0.0"). Подставляется линкером:
//
//	go build -ldflags "-X schyotovod/internal/version.Version=v1.0.0"
var Version = "dev"

// Commit — хеш коммита сборки (опционально).
var Commit = ""

// BuildDate — дата сборки (опционально).
var BuildDate = ""
