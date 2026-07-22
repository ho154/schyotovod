#!/usr/bin/env bash
#
# Установщик Schyotovod — сервиса автоматической выгрузки счетов из Gmail в Pyrus.
#
# Использование:
#   curl -sSL https://raw.githubusercontent.com/<owner>/<repo>/main/deploy/install.sh | sudo bash
#   либо с указанием каталога установки:
#   curl -sSL .../install.sh | sudo bash -s -- /opt/schyotovod
#
# Скрипт:
#   1. Скачивает последний релиз бинарника с GitHub (с проверкой SHA256).
#   2. Создаёт системного пользователя и каталог установки.
#   3. Интерактивно запрашивает логин/пароль администратора панели.
#   4. Устанавливает и запускает systemd-сервис (автозапуск после перезагрузки).

set -euo pipefail

# --- Параметры (при необходимости отредактируйте REPO под ваш форк) ---
REPO="${SCHYOTOVOD_REPO:-ho154/schyotovod}"
INSTALL_DIR="${1:-/opt/schyotovod}"
SERVICE_USER="schyotovod"
SERVICE_NAME="schyotovod"
BIN_NAME="schyotovod"

log()  { echo -e "\033[1;34m[Schyotovod]\033[0m $*"; }
err()  { echo -e "\033[1;31m[Ошибка]\033[0m $*" >&2; }
die()  { err "$*"; exit 1; }

[ "$(id -u)" -eq 0 ] || die "Запустите установщик с правами root (sudo)."

# --- Определение платформы ---
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) die "Неподдерживаемая архитектура: $ARCH" ;;
esac
ASSET="${BIN_NAME}_${OS}_${ARCH}"
SUM_ASSET="${ASSET}.sha256"

# --- Проверка зависимостей ---
command -v curl >/dev/null 2>&1 || die "Требуется curl."
command -v sha256sum >/dev/null 2>&1 || die "Требуется sha256sum."

log "Определение последней версии из репозитория $REPO…"
API_URL="https://api.github.com/repos/${REPO}/releases/latest"
DOWNLOAD_BASE="$(curl -sSL "$API_URL" | grep -oE '"browser_download_url": *"[^"]+"' | grep "$ASSET" | head -n1 | sed -E 's/.*"([^"]+)".*/\1/' || true)"

if [ -z "$DOWNLOAD_BASE" ]; then
  die "Не удалось найти файл релиза $ASSET. Проверьте, что репозиторий $REPO публичный и содержит релизы."
fi
SUM_URL="$(curl -sSL "$API_URL" | grep -oE '"browser_download_url": *"[^"]+"' | grep "$SUM_ASSET" | head -n1 | sed -E 's/.*"([^"]+)".*/\1/' || true)"

# --- Загрузка бинарника и контрольной суммы ---
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

log "Загрузка бинарника…"
curl -sSL -o "$TMP/$BIN_NAME" "$DOWNLOAD_BASE"

if [ -n "$SUM_URL" ]; then
  log "Проверка контрольной суммы…"
  curl -sSL -o "$TMP/$SUM_ASSET" "$SUM_URL"
  EXPECTED="$(awk '{print $1}' "$TMP/$SUM_ASSET")"
  ACTUAL="$(sha256sum "$TMP/$BIN_NAME" | awk '{print $1}')"
  [ "$EXPECTED" = "$ACTUAL" ] || die "Контрольная сумма не совпадает — файл повреждён при загрузке."
  log "Контрольная сумма верна."
else
  err "Файл контрольной суммы не найден — установка продолжается без проверки целостности."
fi

# --- Создание пользователя и каталогов ---
if ! id "$SERVICE_USER" >/dev/null 2>&1; then
  log "Создание системного пользователя $SERVICE_USER…"
  useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
fi

log "Установка в $INSTALL_DIR…"
mkdir -p "$INSTALL_DIR/bin" "$INSTALL_DIR/logs"
install -m 0755 "$TMP/$BIN_NAME" "$INSTALL_DIR/bin/$BIN_NAME"

# Символическая ссылка для удобного вызова: schyotovod --reset-admin-password
ln -sf "$INSTALL_DIR/bin/$BIN_NAME" /usr/local/bin/$BIN_NAME

# --- Интерактивный ввод логина/пароля администратора ---
echo
log "Настройка доступа к панели управления"
echo "Придумайте логин и пароль для входа в панель управления сервисом."
echo "(это НЕ логин/пароль от почты и НЕ от Pyrus — только для доступа к этой админке)"
echo

# Читаем из /dev/tty, чтобы работало и при запуске через curl | bash.
read -r -p "Логин администратора: " ADMIN_LOGIN < /dev/tty
while :; do
  read -r -s -p "Пароль администратора: " ADMIN_PASS < /dev/tty; echo
  read -r -s -p "Повторите пароль: " ADMIN_PASS2 < /dev/tty; echo
  [ -n "$ADMIN_PASS" ] || { err "Пароль не может быть пустым."; continue; }
  [ "$ADMIN_PASS" = "$ADMIN_PASS2" ] || { err "Пароли не совпадают, попробуйте снова."; continue; }
  break
done

log "Применение логина и пароля администратора…"
"$INSTALL_DIR/bin/$BIN_NAME" --data-dir "$INSTALL_DIR" \
  --init-admin-login "$ADMIN_LOGIN" --init-admin-password "$ADMIN_PASS"

# Права на каталог — только пользователю сервиса.
chown -R "$SERVICE_USER:$SERVICE_USER" "$INSTALL_DIR"
chmod 700 "$INSTALL_DIR"
[ -f "$INSTALL_DIR/config.json" ] && chmod 600 "$INSTALL_DIR/config.json"

# --- Установка systemd-сервиса ---
log "Установка systemd-сервиса…"
cat > "/etc/systemd/system/${SERVICE_NAME}.service" <<UNIT
[Unit]
Description=Schyotovod — автоматическая выгрузка счетов из Gmail в Pyrus
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$SERVICE_USER
Group=$SERVICE_USER
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/bin/$BIN_NAME --data-dir $INSTALL_DIR
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=true
PrivateTmp=true
ReadWritePaths=$INSTALL_DIR

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now "${SERVICE_NAME}.service"

# --- Определение порта и адреса ---
PORT="$(grep -oE '"port": *[0-9]+' "$INSTALL_DIR/config.json" 2>/dev/null | grep -oE '[0-9]+' | head -n1 || echo 47291)"
IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
[ -n "$IP" ] || IP="<адрес-сервера>"

echo
log "Готово! Сервис Schyotovod установлен и запущен."
echo
echo "  Откройте панель управления:  http://${IP}:${PORT}"
echo "  Логин: $ADMIN_LOGIN"
echo
echo "  Управление сервисом:"
echo "    systemctl status $SERVICE_NAME     # статус"
echo "    journalctl -u $SERVICE_NAME -f     # логи"
echo "    schyotovod --reset-admin-password  # сброс доступа к панели"
echo
