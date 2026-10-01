// Package config отвечает за загрузку, валидацию и безопасное логирование
// конфигурации сервиса ast2okdesk.
//
// Формат конфигурации — TOML. Значения по умолчанию задаются функцией Default
// и перекрываются только теми ключами, которые присутствуют в файле. Любые
// неизвестные ключи считаются ошибкой: это защищает от опечаток в именах
// параметров, из-за которых настройка молча игнорировалась бы.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// maskedValue — значение, которым заменяются секреты при логировании.
const maskedValue = "***"

// Config — корневая структура конфигурации.
type Config struct {
	Asterisk   Asterisk   `toml:"asterisk"`
	Okdesk     Okdesk     `toml:"okdesk"`
	Recordings Recordings `toml:"recordings"`
	Caddy      Caddy      `toml:"caddy"`
	Retry      Retry      `toml:"retry"`
	Retention  Retention  `toml:"retention"`
	Employees  []Employee `toml:"employees"`
}

// Asterisk — параметры Asterisk: очередь, часовой пояс и доступ к AMI.
//
// Параметры подключения к Asterisk Manager Interface заданы плоскими ключами
// ami_* внутри этой же секции (отступление от ТЗ: вместо секций [route] и
// [ami]).
type Asterisk struct {
	// Timezone — часовой пояс сервера Asterisk (IANA), в котором трактуются
	// времена из события Cdr. Обязательное поле, значение по умолчанию отсутствует.
	Timezone string `toml:"timezone"`
	// Queue — имя мониторируемой очереди (бывш. route.asterisk_queue).
	Queue string `toml:"queue"`
	// AMIHost — адрес сервера AMI.
	AMIHost string `toml:"ami_host"`
	// AMIPort — TCP-порт AMI.
	AMIPort int `toml:"ami_port"`
	// AMIUser — логин AMI (доступ только на чтение).
	AMIUser string `toml:"ami_user"`
	// AMIPassword — пароль AMI. Храните только в config.toml (0600).
	AMIPassword string `toml:"ami_password"`
}

// Okdesk — параметры REST API Okdesk.
type Okdesk struct {
	// BaseURL — базовый URL аккаунта, например https://intellektstroy.okdesk.ru
	BaseURL  string `toml:"base_url"`
	APIToken string `toml:"api_token"`
	// TelephonyNumber — внутренний номер УЗ оператора (fallback, если peer
	// отсутствует в списке [[employees]]).
	TelephonyNumber int `toml:"telephony_number"`
	// IncomingPhoneNumber — входящий номер линии, попадает в receiver_phone.
	IncomingPhoneNumber string `toml:"incoming_phone_number"`
	// SearchNumbersCount — сколько последних цифр номера клиента использовать
	// при поиске в Okdesk (1..10).
	SearchNumbersCount int `toml:"search_numbers_count"`
	// AutoLinkIssue — автоматически подбирать заявку для привязки записи о
	// звонке. По умолчанию выключено: привязку выполняет координатор вручную.
	AutoLinkIssue bool `toml:"auto_link_issue"`
	// Timezone — часовой пояс аккаунта Okdesk (IANA); применяется к started_at
	// и finished_at в API.
	Timezone string `toml:"timezone"`
}

// Recordings — раздача записей разговоров.
type Recordings struct {
	// BaseURL — базовый URL для file_url, всегда оканчивается на «/».
	BaseURL string `toml:"base_url"`
	// FilesDir — каталог с MP3-файлами на сервере.
	FilesDir string `toml:"files_dir"`
}

// Caddy — параметры HTTPS-сервера раздачи записей.
type Caddy struct {
	// WebPort — порт HTTPS (не 80/443: они заняты другим сервисом).
	WebPort int `toml:"web_port"`
	// DNSProvider — DNS-провайдер для DNS-01 (например, regru).
	DNSProvider string `toml:"dns_provider"`
	// DNSCredentials — учётные данные DNS-провайдера для выпуска сертификата.
	DNSCredentials string `toml:"dns_credentials"`
	// AllowedIPs — allowlist доступа к записям: IP-адреса или CIDR.
	AllowedIPs []string `toml:"allowed_ips"`
}

// Retry — параметры досылки записей о звонках.
type Retry struct {
	MaxAttempts           int `toml:"max_attempts"`
	InitialBackoffSeconds int `toml:"initial_backoff_seconds"`
	MaxBackoffSeconds     int `toml:"max_backoff_seconds"`
}

// Retention — хранение и чистка SQLite.
type Retention struct {
	CorrelationTTLHours    int `toml:"correlation_ttl_hours"`
	CleanupIntervalMinutes int `toml:"cleanup_interval_minutes"`
}

// Employee — сопоставление SIP-peer оператора и внутреннего номера Okdesk.
type Employee struct {
	SIPPeer               string `toml:"sip_peer"`
	OkdeskTelephonyNumber int    `toml:"okdesk_telephony_number"`
}

// Default возвращает конфигурацию со значениями по умолчанию. Эти же значения
// продублированы в config.example.toml.
func Default() Config {
	return Config{
		Asterisk: Asterisk{
			AMIHost: "localhost",
			AMIPort: 5038,
		},
		Okdesk: Okdesk{
			SearchNumbersCount: 10,
			Timezone:           "Europe/Moscow",
		},
		Recordings: Recordings{
			FilesDir: "/var/calls",
		},
		Caddy: Caddy{
			WebPort:     8443,
			DNSProvider: "regru",
		},
		Retry: Retry{
			MaxAttempts:           8,
			InitialBackoffSeconds: 5,
			MaxBackoffSeconds:     3600,
		},
		Retention: Retention{
			CorrelationTTLHours:    24,
			CleanupIntervalMinutes: 10,
		},
	}
}

// Load читает конфигурацию из файла path, применяет значения по умолчанию,
// нормализует значения и проверяет корректность.
func Load(path string) (Config, error) {
	cfg := Default()

	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return Config{}, fmt.Errorf("чтение конфигурации %q: %w", path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		return Config{}, fmt.Errorf("конфигурация %q содержит неизвестные ключи: %s", path, strings.Join(keys, ", "))
	}

	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("конфигурация %q: %w", path, err)
	}
	return cfg, nil
}

// normalize приводит значения к каноническому виду: убирает завершающие «/» у
// базовых URL и гарантирует, что recordings.base_url оканчивается на «/»
// (file_url собирается как конкатенация base_url и имени файла).
func (c *Config) normalize() {
	c.Okdesk.BaseURL = strings.TrimRight(strings.TrimSpace(c.Okdesk.BaseURL), "/")

	base := strings.TrimRight(strings.TrimSpace(c.Recordings.BaseURL), "/")
	if base != "" {
		base += "/"
	}
	c.Recordings.BaseURL = base
}

// Validate проверяет конфигурацию и возвращает составную ошибку со списком всех
// найденных проблем (чтобы не исправлять их по одной).
func (c Config) Validate() error {
	var errs []string
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	// --- Asterisk ---
	if strings.TrimSpace(c.Asterisk.Queue) == "" {
		add("asterisk.queue: обязательное поле не заполнено")
	}
	if !validTimezone(c.Asterisk.Timezone) {
		add("asterisk.timezone: некорректный часовой пояс %q", c.Asterisk.Timezone)
	}
	if strings.TrimSpace(c.Asterisk.AMIHost) == "" {
		add("asterisk.ami_host: обязательное поле не заполнено")
	}
	if c.Asterisk.AMIPort < 1 || c.Asterisk.AMIPort > 65535 {
		add("asterisk.ami_port: должно быть в диапазоне 1..65535, получено %d", c.Asterisk.AMIPort)
	}
	if strings.TrimSpace(c.Asterisk.AMIUser) == "" {
		add("asterisk.ami_user: обязательное поле не заполнено")
	}
	if c.Asterisk.AMIPassword == "" {
		add("asterisk.ami_password: обязательное поле не заполнено")
	}

	// --- Okdesk ---
	if msg := validateHTTPURL("okdesk.base_url", c.Okdesk.BaseURL); msg != "" {
		add("%s", msg)
	}
	if strings.TrimSpace(c.Okdesk.APIToken) == "" {
		add("okdesk.api_token: обязательное поле не заполнено")
	}
	if c.Okdesk.TelephonyNumber <= 0 && len(c.Employees) == 0 {
		add("okdesk.telephony_number: должен быть > 0, если не задан список [[employees]]")
	}
	if strings.TrimSpace(c.Okdesk.IncomingPhoneNumber) == "" {
		add("okdesk.incoming_phone_number: обязательное поле не заполнено")
	}
	if c.Okdesk.SearchNumbersCount < 1 || c.Okdesk.SearchNumbersCount > 10 {
		add("okdesk.search_numbers_count: должно быть в диапазоне 1..10, получено %d", c.Okdesk.SearchNumbersCount)
	}
	if !validTimezone(c.Okdesk.Timezone) {
		add("okdesk.timezone: некорректный часовой пояс %q", c.Okdesk.Timezone)
	}

	// --- Recordings ---
	if msg := validateHTTPURL("recordings.base_url", c.Recordings.BaseURL); msg != "" {
		add("%s", msg)
	}
	if strings.TrimSpace(c.Recordings.FilesDir) == "" {
		add("recordings.files_dir: обязательное поле не заполнено")
	}

	// --- Caddy ---
	if c.Caddy.WebPort < 1 || c.Caddy.WebPort > 65535 {
		add("caddy.web_port: должно быть в диапазоне 1..65535, получено %d", c.Caddy.WebPort)
	} else if c.Caddy.WebPort == 80 || c.Caddy.WebPort == 443 {
		add("caddy.web_port: порты 80 и 443 заняты другим сервисом, выберите другой порт")
	}
	if strings.TrimSpace(c.Caddy.DNSProvider) == "" {
		add("caddy.dns_provider: обязательное поле не заполнено")
	}
	if strings.TrimSpace(c.Caddy.DNSCredentials) == "" {
		add("caddy.dns_credentials: обязательное поле не заполнено")
	}
	if len(c.Caddy.AllowedIPs) == 0 {
		add("caddy.allowed_ips: требуется хотя бы один IP-адрес или CIDR (ограничение доступа к записям)")
	}
	for i, raw := range c.Caddy.AllowedIPs {
		if !validIPOrCIDR(raw) {
			add("caddy.allowed_ips[%d]: %q не является IP-адресом или CIDR", i, raw)
		}
	}

	// --- Retry ---
	if c.Retry.MaxAttempts < 1 {
		add("retry.max_attempts: должно быть >= 1, получено %d", c.Retry.MaxAttempts)
	}
	if c.Retry.InitialBackoffSeconds < 1 {
		add("retry.initial_backoff_seconds: должно быть >= 1, получено %d", c.Retry.InitialBackoffSeconds)
	}
	if c.Retry.MaxBackoffSeconds < c.Retry.InitialBackoffSeconds {
		add("retry.max_backoff_seconds: должно быть >= retry.initial_backoff_seconds (%d), получено %d",
			c.Retry.InitialBackoffSeconds, c.Retry.MaxBackoffSeconds)
	}

	// --- Retention ---
	if c.Retention.CorrelationTTLHours < 1 {
		add("retention.correlation_ttl_hours: должно быть >= 1, получено %d", c.Retention.CorrelationTTLHours)
	}
	if c.Retention.CleanupIntervalMinutes < 1 {
		add("retention.cleanup_interval_minutes: должно быть >= 1, получено %d", c.Retention.CleanupIntervalMinutes)
	}

	// --- Employees ---
	seenPeers := make(map[string]int, len(c.Employees))
	for i, e := range c.Employees {
		switch {
		case strings.TrimSpace(e.SIPPeer) == "":
			add("employees[%d].sip_peer: обязательное поле не заполнено", i)
		default:
			if prev, ok := seenPeers[e.SIPPeer]; ok {
				add("employees[%d].sip_peer: дубликат %q (уже задан в employees[%d])", i, e.SIPPeer, prev)
			} else {
				seenPeers[e.SIPPeer] = i
			}
		}
		if e.OkdeskTelephonyNumber <= 0 {
			add("employees[%d].okdesk_telephony_number: должно быть > 0, получено %d", i, e.OkdeskTelephonyNumber)
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("конфигурация невалидна:\n  - %s", strings.Join(errs, "\n  - "))
}

// Redacted возвращает копию конфигурации с замаскированными секретами.
func (c Config) Redacted() Config {
	r := c
	r.Okdesk.APIToken = mask(c.Okdesk.APIToken)
	r.Asterisk.AMIPassword = mask(c.Asterisk.AMIPassword)
	r.Caddy.DNSCredentials = mask(c.Caddy.DNSCredentials)
	return r
}

// LogValue реализует slog.LogValuer: конфигурация всегда логируется в
// замаскированном виде, даже при случайном slog.Any("config", cfg).
func (c Config) LogValue() slog.Value {
	r := c.Redacted()
	return slog.GroupValue(
		slog.Any("asterisk", r.Asterisk),
		slog.Any("okdesk", r.Okdesk),
		slog.Any("recordings", r.Recordings),
		slog.Any("caddy", r.Caddy),
		slog.Any("retry", r.Retry),
		slog.Any("retention", r.Retention),
		slog.Any("employees", r.Employees),
	)
}

// mask прячет непустой секрет, оставляя пустую строку пустой.
func mask(secret string) string {
	if secret == "" {
		return ""
	}
	return maskedValue
}

// validTimezone сообщает, загружается ли часовой пояс tz (IANA).
func validTimezone(tz string) bool {
	if strings.TrimSpace(tz) == "" {
		return false
	}
	_, err := time.LoadLocation(tz)
	return err == nil
}

// validIPOrCIDR сообщает, является ли строка IP-адресом или CIDR.
func validIPOrCIDR(raw string) bool {
	v := strings.TrimSpace(raw)
	if v == "" {
		return false
	}
	if net.ParseIP(v) != nil {
		return true
	}
	_, _, err := net.ParseCIDR(v)
	return err == nil
}

// validateHTTPURL возвращает текст ошибки для поля field или пустую строку.
func validateHTTPURL(field, raw string) string {
	if strings.TrimSpace(raw) == "" {
		return field + ": обязательное поле не заполнено"
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Sprintf("%s: ожидается абсолютный http(s)-URL, получено %q", field, raw)
	}
	return ""
}
