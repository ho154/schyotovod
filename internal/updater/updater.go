// Package updater реализует самообновление сервиса по модели pull:
// приложение само проверяет GitHub Releases API (анонимно, без токена и
// без GitHub-аккаунта у клиента), скачивает бинарник новой версии, проверяет
// его контрольную сумму SHA256, атомарно заменяет исполняемый файл и
// перезапускается. При неудачном запуске новой версии выполняется откат.
//
// Публичный репозиторий GitHub позволяет обращаться к Releases API анонимно
// (лимит 60 запросов в час на IP — с запасом для проверки раз в сутки).
package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Release — сведения о релизе GitHub.
type Release struct {
	TagName string  `json:"tag_name"`
	Name    string  `json:"name"`
	Body    string  `json:"body"` // changelog
	Assets  []Asset `json:"assets"`
}

// Asset — файл, приложенный к релизу.
type Asset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
	Size        int64  `json:"size"`
}

// Updater управляет проверкой и установкой обновлений.
type Updater struct {
	repo           string // "owner/repo"
	currentVersion string
	http           *http.Client
}

// New создаёт Updater. repo — репозиторий вида "owner/repo",
// currentVersion — текущая версия бинарника (например, "v1.0.0").
func New(repo, currentVersion string) *Updater {
	return &Updater{
		repo:           repo,
		currentVersion: currentVersion,
		http:           &http.Client{Timeout: 5 * time.Minute},
	}
}

// LatestRelease запрашивает последний релиз через GitHub Releases API (анонимно).
func (u *Updater) LatestRelease(ctx context.Context) (*Release, error) {
	if u.repo == "" {
		return nil, fmt.Errorf("не задан репозиторий обновлений")
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", u.repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := u.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("не удалось запросить сведения о последней версии: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub вернул ошибку при проверке обновлений (HTTP %d)", resp.StatusCode)
	}
	var rel Release
	if err := json.Unmarshal(data, &rel); err != nil {
		return nil, fmt.Errorf("не удалось разобрать ответ GitHub: %w", err)
	}
	return &rel, nil
}

// IsNewer сообщает, новее ли версия релиза текущей.
func (u *Updater) IsNewer(rel *Release) bool {
	if rel == nil {
		return false
	}
	return normalizeVersion(rel.TagName) != normalizeVersion(u.currentVersion)
}

// assetNames возвращает ожидаемые имена бинарника и файла контрольной суммы
// для текущей платформы (например, schyotovod_linux_amd64 и его .sha256).
func (u *Updater) assetNames() (binName, sumName string) {
	binName = fmt.Sprintf("schyotovod_%s_%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	sumName = binName + ".sha256"
	return binName, sumName
}

// findAsset ищет asset по имени.
func findAsset(rel *Release, name string) *Asset {
	for i := range rel.Assets {
		if rel.Assets[i].Name == name {
			return &rel.Assets[i]
		}
	}
	return nil
}

// download скачивает содержимое asset по URL.
func (u *Updater) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ошибка загрузки (HTTP %d)", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// DownloadAndVerify скачивает бинарник новой версии и проверяет его SHA256.
// Возвращает содержимое проверенного бинарника.
func (u *Updater) DownloadAndVerify(ctx context.Context, rel *Release) ([]byte, error) {
	binName, sumName := u.assetNames()

	binAsset := findAsset(rel, binName)
	if binAsset == nil {
		return nil, fmt.Errorf("в релизе %s нет файла для вашей платформы (%s)", rel.TagName, binName)
	}
	sumAsset := findAsset(rel, sumName)
	if sumAsset == nil {
		return nil, fmt.Errorf("в релизе %s нет файла контрольной суммы (%s)", rel.TagName, sumName)
	}

	binData, err := u.download(ctx, binAsset.DownloadURL)
	if err != nil {
		return nil, fmt.Errorf("не удалось скачать новую версию: %w", err)
	}
	sumData, err := u.download(ctx, sumAsset.DownloadURL)
	if err != nil {
		return nil, fmt.Errorf("не удалось скачать контрольную сумму: %w", err)
	}

	expected := parseSHA256(string(sumData))
	if expected == "" {
		return nil, fmt.Errorf("не удалось прочитать контрольную сумму из файла релиза")
	}
	actual := sha256Hex(binData)
	if !strings.EqualFold(actual, expected) {
		return nil, fmt.Errorf("контрольная сумма не совпадает: файл повреждён при загрузке "+
			"(ожидалось %s, получено %s)", expected, actual)
	}
	return binData, nil
}

// Apply атомарно заменяет текущий исполняемый файл на новую версию,
// сохраняя предыдущую как <path>.old для возможного отката.
// Возвращает путь к бэкапу предыдущей версии.
func (u *Updater) Apply(newBinary []byte) (backupPath string, err error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("не удалось определить путь к исполняемому файлу: %w", err)
	}
	exePath, _ = filepath.EvalSymlinks(exePath)

	dir := filepath.Dir(exePath)
	tmpPath := filepath.Join(dir, ".schyotovod_new")
	backupPath = exePath + ".old"

	// Пишем новую версию во временный файл.
	if err := os.WriteFile(tmpPath, newBinary, 0o755); err != nil {
		return "", fmt.Errorf("не удалось записать новую версию: %w", err)
	}

	// Сохраняем текущий бинарник как .old (перезаписывая предыдущий бэкап).
	_ = os.Remove(backupPath)
	if err := copyFile(exePath, backupPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("не удалось создать резервную копию текущей версии: %w", err)
	}

	// Атомарно заменяем.
	if err := os.Rename(tmpPath, exePath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("не удалось заменить исполняемый файл: %w", err)
	}
	return backupPath, nil
}

// Rollback восстанавливает предыдущую версию из бэкапа.
func Rollback(backupPath string) error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	exePath, _ = filepath.EvalSymlinks(exePath)
	if _, err := os.Stat(backupPath); err != nil {
		return fmt.Errorf("резервная копия для отката не найдена: %w", err)
	}
	if err := copyFile(backupPath, exePath); err != nil {
		return fmt.Errorf("не удалось восстановить предыдущую версию: %w", err)
	}
	return nil
}

// copyFile копирует файл с сохранением прав на исполнение.
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o755)
}

// sha256Hex возвращает hex-представление SHA256 данных.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// parseSHA256 извлекает hex-хеш из содержимого .sha256 файла
// (поддерживает форматы "<hash>" и "<hash>  <filename>").
func parseSHA256(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	fields := strings.Fields(content)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// normalizeVersion приводит версию к сравнимому виду (убирает префикс "v", пробелы).
func normalizeVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}
