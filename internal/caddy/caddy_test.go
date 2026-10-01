package caddy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testConfig() Config {
	return Config{
		SiteAddress:    "https://calls.example.ru:8443",
		DNSProvider:    "regru",
		AllowedIPs:     []string{"203.0.113.10", "198.51.100.0/24"},
		RecordsPath:    "/var/calls",
		RecordsURLPath: "/records/",
	}
}

func TestRenderGolden(t *testing.T) {
	got := testConfig().Render()

	want, err := os.ReadFile(filepath.Join("testdata", "Caddyfile.golden"))
	if err != nil {
		t.Fatalf("чтение эталона: %v", err)
	}
	if got != string(want) {
		t.Errorf("сгенерированный Caddyfile отличается от эталона:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRenderIncludesDNSCredentials(t *testing.T) {
	got := testConfig().Render()

	for _, want := range []string{
		"dns regru {",
		"username {$REGRU_USERNAME}",
		"password {$REGRU_PASSWORD}",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("в Caddyfile не найдено %q:\n%s", want, got)
		}
	}
}

func TestRenderDisablesAutoHTTPSRedirects(t *testing.T) {
	got := testConfig().Render()

	if !strings.Contains(got, "{\n\tauto_https disable_redirects\n}") {
		t.Errorf("ожидалось отключение авторедиректа HTTP->HTTPS, получено:\n%s", got)
	}
}

func TestRenderNormalizesURLPath(t *testing.T) {
	cfg := testConfig()
	cfg.RecordsURLPath = "records"

	got := cfg.Render()
	if !strings.Contains(got, "handle_path /records/* {") {
		t.Errorf("ожидалась маска /records/*, получено:\n%s", got)
	}
}

func TestFromRecordings(t *testing.T) {
	tests := []struct {
		name       string
		baseURL    string
		webPort    int
		wantSite   string
		wantPrefix string
	}{
		{
			name:       "порт из URL",
			baseURL:    "https://calls.example.ru:8443/records/",
			webPort:    8443,
			wantSite:   "https://calls.example.ru:8443",
			wantPrefix: "/records/",
		},
		{
			name:       "порт по умолчанию",
			baseURL:    "https://calls.example.ru/records/",
			webPort:    9443,
			wantSite:   "https://calls.example.ru:9443",
			wantPrefix: "/records/",
		},
		{
			name:       "пустой путь",
			baseURL:    "https://calls.example.ru:8443",
			webPort:    8443,
			wantSite:   "https://calls.example.ru:8443",
			wantPrefix: "/records/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := FromRecordings(tt.baseURL, tt.webPort, "regru", []string{"10.0.0.1"}, "/var/calls")
			if err != nil {
				t.Fatalf("FromRecordings: %v", err)
			}
			if cfg.SiteAddress != tt.wantSite {
				t.Errorf("SiteAddress = %q, хотим %q", cfg.SiteAddress, tt.wantSite)
			}
			if cfg.RecordsURLPath != tt.wantPrefix {
				t.Errorf("RecordsURLPath = %q, хотим %q", cfg.RecordsURLPath, tt.wantPrefix)
			}
			if cfg.DNSProvider != "regru" || cfg.RecordsPath != "/var/calls" {
				t.Errorf("прочие поля заданы неверно: %+v", cfg)
			}
		})
	}
}

func TestFromRecordingsInvalid(t *testing.T) {
	for _, raw := range []string{"", "calls.example.ru/records/", "ftp://calls.example.ru"} {
		if _, err := FromRecordings(raw, 8443, "regru", []string{"10.0.0.1"}, "/var/calls"); err == nil {
			t.Errorf("FromRecordings(%q): ожидалась ошибка", raw)
		}
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"валидная", func(c *Config) {}, false},
		{"пустой адрес", func(c *Config) { c.SiteAddress = "" }, true},
		{"адрес с пробелом", func(c *Config) { c.SiteAddress = "https://a b:8443" }, true},
		{"пустой провайдер", func(c *Config) { c.DNSProvider = "" }, true},
		{"пустой allowlist", func(c *Config) { c.AllowedIPs = nil }, true},
		{"битый allowlist", func(c *Config) { c.AllowedIPs = []string{"not-an-ip"} }, true},
		{"пустой каталог", func(c *Config) { c.RecordsPath = "" }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("ожидалась ошибка валидации")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
		})
	}
}

func TestNewManagerValidates(t *testing.T) {
	if _, err := NewManager(Config{}, "Caddyfile", "caddy", silentLogger()); err == nil {
		t.Error("ожидалась ошибка для пустой конфигурации")
	}
	if _, err := NewManager(testConfig(), "", "caddy", silentLogger()); err == nil {
		t.Error("ожидалась ошибка для пустого пути к Caddyfile")
	}
}

func TestApplyWritesAndReloads(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "Caddyfile")

	m, err := NewManager(testConfig(), configPath, "/opt/ast2okdesk/caddy", silentLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	var gotName string
	var gotArgs []string
	m.run = func(_ context.Context, name string, args ...string) error {
		gotName = name
		gotArgs = append([]string(nil), args...)
		return nil
	}

	if err := m.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("чтение Caddyfile: %v", err)
	}
	if string(content) != m.Render() {
		t.Error("записанный Caddyfile не совпадает с Render")
	}

	if gotName != "/opt/ast2okdesk/caddy" {
		t.Errorf("выполнен %q, ожидался /opt/ast2okdesk/caddy", gotName)
	}
	want := []string{"reload", "--config", configPath, "--adapter", "caddyfile"}
	if len(gotArgs) != len(want) {
		t.Fatalf("аргументы reload = %v, хотим %v", gotArgs, want)
	}
	for i := range want {
		if gotArgs[i] != want[i] {
			t.Errorf("аргумент reload[%d] = %q, хотим %q", i, gotArgs[i], want[i])
		}
	}
}

func TestApplyWithoutBinarySkipsReload(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(testConfig(), filepath.Join(dir, "Caddyfile"), "", silentLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	called := false
	m.run = func(context.Context, string, ...string) error {
		called = true
		return nil
	}

	if err := m.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if called {
		t.Error("reload не должен вызываться без пути к бинарнику")
	}
}

func TestApplyReloadErrorStillWrites(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "Caddyfile")
	m, err := NewManager(testConfig(), configPath, "caddy", silentLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	m.run = func(context.Context, string, ...string) error {
		return errors.New("caddy недоступен")
	}

	if err := m.Apply(context.Background()); err == nil {
		t.Fatal("ожидалась ошибка перезагрузки")
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Errorf("Caddyfile должен быть записан несмотря на ошибку reload: %v", err)
	}
}

func TestDeployUnits(t *testing.T) {
	tests := []struct {
		file     string
		required []string
	}{
		{
			file: "okdesk.service",
			required: []string{
				"[Unit]",
				"[Service]",
				"[Install]",
				"ExecStart=/opt/ast2okdesk/okdesk",
				"User=okdesk",
				"WantedBy=multi-user.target",
			},
		},
		{
			file: "okdesk-caddy.service",
			required: []string{
				"[Unit]",
				"[Service]",
				"[Install]",
				"EnvironmentFile=",
				"ExecStart=/opt/ast2okdesk/caddy run",
				"ExecReload=/opt/ast2okdesk/caddy reload",
				"WantedBy=multi-user.target",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", "deploy", tt.file))
			if err != nil {
				t.Fatalf("чтение юнита: %v", err)
			}
			for _, want := range tt.required {
				if !strings.Contains(string(data), want) {
					t.Errorf("в %s не найдено %q", tt.file, want)
				}
			}
		})
	}
}
