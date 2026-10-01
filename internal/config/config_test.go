package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fullTOML — корректный конфиг со всеми секциями.
const fullTOML = `[asterisk]
timezone = "Asia/Yekaterinburg"
queue = "support"
ami_host = "localhost"
ami_port = 5038
ami_user = "ast2okdesk"
ami_password = "ami-secret"

[okdesk]
base_url = "https://intellektstroy.okdesk.ru/"
api_token = "okdesk-secret"
telephony_number = 327
search_numbers_count = 10
auto_link_issue = true
timezone = "Europe/Moscow"

[recordings]
base_url = "https://calls.example.ru:8443/records"
files_dir = "/var/calls"

[caddy]
web_port = 8443
dns_provider = "regru"
dns_credentials = "regru-user:regru-pass"
allowed_ips = ["203.0.113.10", "198.51.100.0/24"]

[retry]
max_attempts = 8
initial_backoff_seconds = 5
max_backoff_seconds = 3600

[retention]
correlation_ttl_hours = 24
cleanup_interval_minutes = 10

[[employees]]
sip_peer = "ujin327"
okdesk_telephony_number = 327
`

// minimalTOML — только обязательные поля; остальное берётся из Default.
const minimalTOML = `[asterisk]
timezone = "Asia/Yekaterinburg"
queue = "support"
ami_user = "u"
ami_password = "p"

[okdesk]
base_url = "https://x.example"
api_token = "t"
telephony_number = 100

[recordings]
base_url = "https://calls.example/"

[caddy]
dns_credentials = "user:pass"
allowed_ips = ["10.0.0.1"]
`

// writeTemp создаёт временный файл конфигурации и возвращает его путь.
func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("не удалось записать тестовый конфиг: %v", err)
	}
	return path
}

func TestLoadFull(t *testing.T) {
	cfg, err := Load(writeTemp(t, fullTOML))
	if err != nil {
		t.Fatalf("Load вернул ошибку: %v", err)
	}

	if cfg.Asterisk.Timezone != "Asia/Yekaterinburg" {
		t.Errorf("asterisk.timezone = %q", cfg.Asterisk.Timezone)
	}
	if cfg.Asterisk.Queue != "support" {
		t.Errorf("asterisk.queue = %q", cfg.Asterisk.Queue)
	}
	if cfg.Asterisk.AMIHost != "localhost" || cfg.Asterisk.AMIPort != 5038 {
		t.Errorf("asterisk.ami_* = %s:%d", cfg.Asterisk.AMIHost, cfg.Asterisk.AMIPort)
	}
	if cfg.Okdesk.APIToken != "okdesk-secret" {
		t.Errorf("okdesk.api_token = %q", cfg.Okdesk.APIToken)
	}
	if cfg.Okdesk.TelephonyNumber != 327 {
		t.Errorf("okdesk.telephony_number = %d", cfg.Okdesk.TelephonyNumber)
	}
	if cfg.Okdesk.Timezone != "Europe/Moscow" {
		t.Errorf("okdesk.timezone = %q", cfg.Okdesk.Timezone)
	}
	if !cfg.Okdesk.AutoLinkIssue {
		t.Error("okdesk.auto_link_issue = false, ожидалось true из полного конфига")
	}
	// base_url нормализуется: окдесковый без «/», записи — всегда с «/».
	if cfg.Okdesk.BaseURL != "https://intellektstroy.okdesk.ru" {
		t.Errorf("okdesk.base_url = %q", cfg.Okdesk.BaseURL)
	}
	if cfg.Recordings.BaseURL != "https://calls.example.ru:8443/records/" {
		t.Errorf("recordings.base_url = %q", cfg.Recordings.BaseURL)
	}
	if len(cfg.Employees) != 1 || cfg.Employees[0].SIPPeer != "ujin327" {
		t.Errorf("employees = %+v", cfg.Employees)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(writeTemp(t, minimalTOML))
	if err != nil {
		t.Fatalf("Load вернул ошибку: %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"asterisk.ami_host", cfg.Asterisk.AMIHost, "localhost"},
		{"asterisk.ami_port", cfg.Asterisk.AMIPort, 5038},
		{"okdesk.search_numbers_count", cfg.Okdesk.SearchNumbersCount, 10},
		{"okdesk.auto_link_issue", cfg.Okdesk.AutoLinkIssue, false},
		{"okdesk.timezone", cfg.Okdesk.Timezone, "Europe/Moscow"},
		{"recordings.files_dir", cfg.Recordings.FilesDir, "/var/calls"},
		{"caddy.web_port", cfg.Caddy.WebPort, 8443},
		{"caddy.dns_provider", cfg.Caddy.DNSProvider, "regru"},
		{"retry.max_attempts", cfg.Retry.MaxAttempts, 8},
		{"retry.initial_backoff_seconds", cfg.Retry.InitialBackoffSeconds, 5},
		{"retry.max_backoff_seconds", cfg.Retry.MaxBackoffSeconds, 3600},
		{"retention.correlation_ttl_hours", cfg.Retention.CorrelationTTLHours, 24},
		{"retention.cleanup_interval_minutes", cfg.Retention.CleanupIntervalMinutes, 10},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, ожидалось %v", c.name, c.got, c.want)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "нет.toml")); err == nil {
		t.Fatal("ожидалась ошибка для отсутствующего файла")
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	_, err := Load(writeTemp(t, minimalTOML+"\n[unknown]\nfoo = 1\n"))
	if err == nil {
		t.Fatal("ожидалась ошибка для неизвестного ключа")
	}
	if !strings.Contains(err.Error(), "неизвестные ключи") {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
}

// TestLoadRejectsLegacyNestedAMI страхует миграцию: конфигурация старого формата
// с вложенной секцией [asterisk.ami] должна падать с перечнем неизвестных
// ключей, а не молча игнорировать параметры подключения.
func TestLoadRejectsLegacyNestedAMI(t *testing.T) {
	legacy := `[asterisk]
timezone = "Asia/Yekaterinburg"
queue = "support"

[asterisk.ami]
host = "localhost"
port = 5038
user = "u"
password = "p"

[okdesk]
base_url = "https://x.example"
api_token = "t"
telephony_number = 100

[recordings]
base_url = "https://calls.example/"

[caddy]
dns_credentials = "user:pass"
allowed_ips = ["10.0.0.1"]
`
	_, err := Load(writeTemp(t, legacy))
	if err == nil {
		t.Fatal("ожидалась ошибка для устаревшей секции [asterisk.ami]")
	}
	if !strings.Contains(err.Error(), "неизвестные ключи") {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if !strings.Contains(err.Error(), "asterisk.ami") {
		t.Fatalf("ошибка должна указывать на устаревшие ключи asterisk.ami.*: %v", err)
	}
}

// validConfig возвращает заведомо валидную конфигурацию для табличных тестов.
func validConfig() Config {
	c := Default()
	c.Asterisk.Timezone = "Asia/Yekaterinburg"
	c.Asterisk.Queue = "support"
	c.Asterisk.AMIUser = "ast2okdesk"
	c.Asterisk.AMIPassword = "ami-secret"
	c.Okdesk.BaseURL = "https://intellektstroy.okdesk.ru"
	c.Okdesk.APIToken = "okdesk-secret"
	c.Okdesk.TelephonyNumber = 327
	c.Recordings.BaseURL = "https://calls.example.ru:8443/records/"
	c.Caddy.DNSCredentials = "regru-user:regru-pass"
	c.Caddy.AllowedIPs = []string{"203.0.113.10", "198.51.100.0/24"}
	return c
}

func TestValidateValid(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("валидный конфиг отвергнут: %v", err)
	}
}

func TestValidateErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"пустая очередь", func(c *Config) { c.Asterisk.Queue = "" }, "asterisk.queue"},
		{"пустой timezone Asterisk", func(c *Config) { c.Asterisk.Timezone = "" }, "asterisk.timezone"},
		{"неизвестный timezone Asterisk", func(c *Config) { c.Asterisk.Timezone = "Mars/Olympus" }, "asterisk.timezone"},
		{"пустой ami_host", func(c *Config) { c.Asterisk.AMIHost = "" }, "asterisk.ami_host"},
		{"ami_port вне диапазона", func(c *Config) { c.Asterisk.AMIPort = 70000 }, "asterisk.ami_port"},
		{"пустой ami_user", func(c *Config) { c.Asterisk.AMIUser = "" }, "asterisk.ami_user"},
		{"пустой ami_password", func(c *Config) { c.Asterisk.AMIPassword = "" }, "asterisk.ami_password"},
		{"пустой okdesk.base_url", func(c *Config) { c.Okdesk.BaseURL = "" }, "okdesk.base_url"},
		{"base_url не URL", func(c *Config) { c.Okdesk.BaseURL = "intellektstroy.okdesk.ru" }, "okdesk.base_url"},
		{"пустой api_token", func(c *Config) { c.Okdesk.APIToken = "" }, "okdesk.api_token"},
		{"search_numbers_count=0", func(c *Config) { c.Okdesk.SearchNumbersCount = 0 }, "okdesk.search_numbers_count"},
		{"search_numbers_count=11", func(c *Config) { c.Okdesk.SearchNumbersCount = 11 }, "okdesk.search_numbers_count"},
		{"неизвестный timezone Okdesk", func(c *Config) { c.Okdesk.Timezone = "Mars/Olympus" }, "okdesk.timezone"},
		{"пустой recordings.base_url", func(c *Config) { c.Recordings.BaseURL = "" }, "recordings.base_url"},
		{"пустой recordings.files_dir", func(c *Config) { c.Recordings.FilesDir = "" }, "recordings.files_dir"},
		{"caddy.web_port=80", func(c *Config) { c.Caddy.WebPort = 80 }, "caddy.web_port"},
		{"caddy.web_port вне диапазона", func(c *Config) { c.Caddy.WebPort = 0 }, "caddy.web_port"},
		{"пустой dns_provider", func(c *Config) { c.Caddy.DNSProvider = "" }, "caddy.dns_provider"},
		{"пустые dns_credentials", func(c *Config) { c.Caddy.DNSCredentials = "" }, "caddy.dns_credentials"},
		{"пустой allowlist", func(c *Config) { c.Caddy.AllowedIPs = nil }, "caddy.allowed_ips"},
		{"некорректный allowlist", func(c *Config) { c.Caddy.AllowedIPs = []string{"not-an-ip"} }, "caddy.allowed_ips[0]"},
		{"retry.max_attempts=0", func(c *Config) { c.Retry.MaxAttempts = 0 }, "retry.max_attempts"},
		{"retry.initial_backoff=0", func(c *Config) { c.Retry.InitialBackoffSeconds = 0 }, "retry.initial_backoff_seconds"},
		{"max_backoff < initial", func(c *Config) { c.Retry.MaxBackoffSeconds = 1 }, "retry.max_backoff_seconds"},
		{"retention.correlation_ttl=0", func(c *Config) { c.Retention.CorrelationTTLHours = 0 }, "retention.correlation_ttl_hours"},
		{"retention.cleanup_interval=0", func(c *Config) { c.Retention.CleanupIntervalMinutes = 0 }, "retention.cleanup_interval_minutes"},
		{"employees без sip_peer", func(c *Config) {
			c.Employees = []Employee{{OkdeskTelephonyNumber: 1}}
		}, "employees[0].sip_peer"},
		{"employees без номера", func(c *Config) {
			c.Employees = []Employee{{SIPPeer: "a"}}
		}, "employees[0].okdesk_telephony_number"},
		{"employees дубликат peer", func(c *Config) {
			c.Employees = []Employee{{SIPPeer: "a", OkdeskTelephonyNumber: 1}, {SIPPeer: "a", OkdeskTelephonyNumber: 2}}
		}, "дубликат"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("ожидалась ошибка, содержащая %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ошибка %q не содержит %q", err.Error(), tc.want)
			}
		})
	}
}

func TestValidateEmployeesWithoutTelephonyNumber(t *testing.T) {
	cfg := validConfig()
	cfg.Okdesk.TelephonyNumber = 0
	cfg.Employees = []Employee{{SIPPeer: "ujin327", OkdeskTelephonyNumber: 327}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("при заполненном [[employees]] telephony_number необязателен: %v", err)
	}
}

// TestValidateWithoutTelephonyNumbers проверяет, что конфигурация без
// telephony_number и без [[employees]] валидна: в типовом случае номер оператора
// берётся из имени peer.
func TestValidateWithoutTelephonyNumbers(t *testing.T) {
	cfg := validConfig()
	cfg.Okdesk.TelephonyNumber = 0
	cfg.Employees = nil
	if err := cfg.Validate(); err != nil {
		t.Fatalf("конфигурация без номеров должна быть валидной: %v", err)
	}
}

func TestNormalize(t *testing.T) {
	cfg := validConfig()
	cfg.Okdesk.BaseURL = "https://example.org/"
	cfg.Recordings.BaseURL = "https://calls.example.org:8443/records/"
	cfg.normalize()

	if cfg.Okdesk.BaseURL != "https://example.org" {
		t.Errorf("okdesk.base_url = %q", cfg.Okdesk.BaseURL)
	}
	if cfg.Recordings.BaseURL != "https://calls.example.org:8443/records/" {
		t.Errorf("recordings.base_url = %q", cfg.Recordings.BaseURL)
	}
}

func TestRedacted(t *testing.T) {
	r := validConfig().Redacted()

	if r.Okdesk.APIToken != maskedValue {
		t.Errorf("api_token не замаскирован: %q", r.Okdesk.APIToken)
	}
	if r.Asterisk.AMIPassword != maskedValue {
		t.Errorf("ami_password не замаскирован: %q", r.Asterisk.AMIPassword)
	}
	if r.Caddy.DNSCredentials != maskedValue {
		t.Errorf("dns_credentials не замаскирован: %q", r.Caddy.DNSCredentials)
	}
	if r.Asterisk.Queue != "support" {
		t.Errorf("несекретное значение изменено: %q", r.Asterisk.Queue)
	}
}

func TestLogValueHidesSecrets(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("config", "cfg", validConfig())

	out := buf.String()
	for _, secret := range []string{"okdesk-secret", "ami-secret", "regru-user:regru-pass"} {
		if strings.Contains(out, secret) {
			t.Errorf("секрет %q попал в лог: %s", secret, out)
		}
	}
	if !strings.Contains(out, maskedValue) {
		t.Errorf("в логе нет маски %q: %s", maskedValue, out)
	}
	if !strings.Contains(out, "support") {
		t.Errorf("несекретные значения должны логироваться: %s", out)
	}
}
