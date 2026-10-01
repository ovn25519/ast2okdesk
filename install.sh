#!/usr/bin/env bash
#
# Установщик ast2okdesk — интеграция Asterisk 16 ↔ Okdesk.
#
# Поддерживается только Linux/amd64, запуск от имени root.
#
# Переменные окружения:
#   OKDESK_VERSION  — версия релиза (по умолчанию latest).
#
# Установщик идемпотентен: повторный запуск обновляет бинарники и пример
# конфигурации, но НИКОГДА не перезаписывает config.toml, caddy.env и okdesk.db.
#
set -euo pipefail

REPO="ovn25519/ast2okdesk"
INSTALL_DIR="/opt/ast2okdesk"
SERVICE_USER="okdesk"
SERVICE_GROUP="okdesk"
SERVICE_OKDESK="okdesk.service"
SERVICE_CADDY="okdesk-caddy.service"
SYSTEMD_DIR="/etc/systemd/system"

log() { printf 'ast2okdesk: %s\n' "$*"; }
warn() { printf 'ast2okdesk: %s\n' "$*" >&2; }
die() {
    printf 'ast2okdesk: ошибка: %s\n' "$*" >&2
    exit 1
}

require_root() {
    [[ "$(id -u)" -eq 0 ]] || die "запустите установщик от имени root"
}

check_platform() {
    [[ "$(uname -s)" == "Linux" ]] || die "поддерживается только Linux (обнаружено: $(uname -s))"
    local arch
    arch="$(uname -m)"
    [[ "$arch" == "x86_64" || "$arch" == "amd64" ]] || die "поддерживается только amd64 (обнаружено: $arch)"
    command -v curl >/dev/null 2>&1 || die "требуется curl"
    command -v tar >/dev/null 2>&1 || die "требуется tar"
    command -v sha256sum >/dev/null 2>&1 || die "требуется sha256sum"
    command -v systemctl >/dev/null 2>&1 || die "требуется systemd (systemctl)"
}

get_latest_version() {
    curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" |
        grep -m1 '"tag_name":' |
        sed -E 's/.*"([^"]+)".*/\1/'
}

download() {
    local url="$1" dest="$2"
    curl -fsSL "$url" -o "$dest" || die "не удалось скачать ${url}"
}

verify_checksum() {
    local file="$1" name="$2" sums="$3"
    local expected actual
    expected="$(awk -v n="$name" '$2 == n || $2 == "*" n {print $1; exit}' "$sums")"
    [[ -n "$expected" ]] || die "в sha256sum.txt нет контрольной суммы для ${name}"
    actual="$(sha256sum "$file" | awk '{print $1}')"
    [[ "$actual" == "$expected" ]] ||
        die "контрольная сумма ${name} не совпала (ожидалось ${expected}, получено ${actual})"
    log "контрольная сумма ${name} подтверждена"
}

group_exists() {
    if command -v getent >/dev/null 2>&1; then
        getent group "$1" >/dev/null 2>&1
    else
        grep -q "^$1:" /etc/group
    fi
}

nologin_shell() {
    local shell
    for shell in /usr/sbin/nologin /sbin/nologin /bin/false; do
        if [[ -x "$shell" ]]; then
            printf '%s' "$shell"
            return 0
        fi
    done
    printf '%s' "/bin/false"
}

ensure_user() {
    if group_exists "$SERVICE_GROUP"; then
        log "группа ${SERVICE_GROUP} уже существует"
    else
        groupadd --system "$SERVICE_GROUP"
        log "создана группа ${SERVICE_GROUP}"
    fi

    if id -u "$SERVICE_USER" >/dev/null 2>&1; then
        log "пользователь ${SERVICE_USER} уже существует"
        return 0
    fi

    useradd --system \
        --gid "$SERVICE_GROUP" \
        --home-dir "$INSTALL_DIR" \
        --no-create-home \
        --shell "$(nologin_shell)" \
        "$SERVICE_USER"
    log "создан системный пользователь ${SERVICE_USER}"
}

install_config() {
    if [[ -f "${INSTALL_DIR}/config.toml" ]]; then
        log "config.toml уже существует — оставляем без изменений"
        return 0
    fi
    if [[ -f "${INSTALL_DIR}/config.example.toml" ]]; then
        install -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 0600 \
            "${INSTALL_DIR}/config.example.toml" "${INSTALL_DIR}/config.toml"
        log "создан ${INSTALL_DIR}/config.toml (0600) из примера"
    else
        warn "не найден config.example.toml — создайте config.toml вручную"
    fi
}

install_caddy_env() {
    if [[ -f "${INSTALL_DIR}/caddy.env" ]]; then
        log "caddy.env уже существует — оставляем без изменений"
        return 0
    fi
    cat >"${INSTALL_DIR}/caddy.env" <<'EOF'
# Учётные данные DNS-провайдера для выпуска TLS-сертификатов Caddy.
# Заполните значения и перезапустите okdesk-caddy.service.
# Для провайдера regru (см. caddy.dns_provider в config.toml):
REGRU_USERNAME=
REGRU_PASSWORD=
EOF
    chown "${SERVICE_USER}:${SERVICE_GROUP}" "${INSTALL_DIR}/caddy.env"
    chmod 0600 "${INSTALL_DIR}/caddy.env"
    log "создан ${INSTALL_DIR}/caddy.env (0600) — заполните учётные данные DNS"
}

install_units() {
    local extract="$1" src installed=0
    for src in "${extract}"/deploy/*.service; do
        [[ -f "$src" ]] || continue
        install -m 0644 "$src" "${SYSTEMD_DIR}/$(basename "$src")"
        installed=1
    done
    if [[ "$installed" -eq 1 ]]; then
        log "systemd-юниты установлены в ${SYSTEMD_DIR}"
    else
        warn "в архиве нет systemd-юнитов — установите deploy/*.service вручную"
    fi
}

main() {
    require_root
    check_platform

    local requested_version="${OKDESK_VERSION:-latest}"
    local version="$requested_version"
    if [[ "$version" == "latest" ]]; then
        log "определяем последнюю версию релиза…"
        version="$(get_latest_version)"
        [[ -n "$version" ]] || die "не удалось определить версию релиза"
    fi
    log "устанавливаем ast2okdesk ${version} (linux/amd64) в ${INSTALL_DIR}"

    local base_url
    if [[ "$requested_version" == "latest" ]]; then
        base_url="https://github.com/${REPO}/releases/latest/download"
    else
        base_url="https://github.com/${REPO}/releases/download/${version}"
    fi

    local archive="ast2okdesk_${version}_linux_amd64.tar.gz"
    local caddy_asset="caddy-regru"

    local tmp
    tmp="$(mktemp -d)"
    trap 'rm -rf "${tmp:-}"' EXIT

    download "${base_url}/${archive}" "${tmp}/${archive}"
    download "${base_url}/${caddy_asset}" "${tmp}/${caddy_asset}"
    download "${base_url}/sha256sum.txt" "${tmp}/sha256sum.txt"

    verify_checksum "${tmp}/${archive}" "$archive" "${tmp}/sha256sum.txt"
    verify_checksum "${tmp}/${caddy_asset}" "$caddy_asset" "${tmp}/sha256sum.txt"

    local extract="${tmp}/extract"
    mkdir -p "$extract"
    tar -xzf "${tmp}/${archive}" -C "$extract"
    [[ -f "${extract}/okdesk" ]] || die "в архиве отсутствует основной бинарник okdesk"

    ensure_user
    install -d -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 0755 "$INSTALL_DIR"
    install -d -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 0755 "${INSTALL_DIR}/data"

    install -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 0755 \
        "${extract}/okdesk" "${INSTALL_DIR}/okdesk"
    install -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 0755 \
        "${tmp}/${caddy_asset}" "${INSTALL_DIR}/caddy"

    local extra
    for extra in config.example.toml README.md; do
        [[ -f "${extract}/${extra}" ]] || continue
        install -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 0644 \
            "${extract}/${extra}" "${INSTALL_DIR}/${extra}"
    done

    local config_existed=0 caddy_env_existed=0
    [[ -f "${INSTALL_DIR}/config.toml" ]] && config_existed=1
    [[ -f "${INSTALL_DIR}/caddy.env" ]] && caddy_env_existed=1

    install_config
    install_caddy_env
    install_units "$extract"

    systemctl daemon-reload
    if systemctl enable "$SERVICE_CADDY" "$SERVICE_OKDESK" >/dev/null 2>&1; then
        log "автозапуск служб включён"
    else
        warn "не удалось включить автозапуск служб — проверьте вручную"
    fi

    if [[ "$config_existed" -eq 1 && "$caddy_env_existed" -eq 1 ]]; then
        log "обновляем и перезапускаем службы…"
        systemctl restart "$SERVICE_CADDY" ||
            warn "не удалось перезапустить ${SERVICE_CADDY}"
        systemctl restart "$SERVICE_OKDESK" ||
            warn "не удалось перезапустить ${SERVICE_OKDESK}"
        log "службы перезапущены"
    else
        warn "требуется первичная настройка: заполните ${INSTALL_DIR}/config.toml и ${INSTALL_DIR}/caddy.env, затем выполните:"
        warn "  systemctl start ${SERVICE_OKDESK}   # сгенерирует Caddyfile"
        warn "  systemctl start ${SERVICE_CADDY}    # выпустит сертификат и начнёт раздачу записей"
    fi

    log "установка завершена. Проверка состояния: systemctl status ${SERVICE_OKDESK}"
}

main "$@"
