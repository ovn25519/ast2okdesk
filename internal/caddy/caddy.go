// Package caddy генерирует Caddyfile для HTTPS-раздачи записей разговоров и
// применяет его перезагрузкой Caddy.
//
// Модуль намеренно не управляет жизненным циклом Caddy: сам процесс запускается
// отдельным systemd-юнитом okdesk-caddy.service, а okdesk лишь формирует файл
// конфигурации и просит Caddy перечитать его командой «caddy reload».
package caddy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// defaultRecordsURLPath — путь в URL по умолчанию, если его не удалось вывести
// из recordings.base_url.
const defaultRecordsURLPath = "/records/"

// Config — параметры генерируемого Caddyfile.
type Config struct {
	// SiteAddress — адрес площадки, например https://calls.example.ru:8443.
	SiteAddress string
	// DNSProvider — модуль DNS-01 (например, regru).
	DNSProvider string
	// AllowedIPs — allowlist доступа к записям: IP-адреса или CIDR.
	AllowedIPs []string
	// RecordsPath — каталог с MP3-файлами на диске.
	RecordsPath string
	// RecordsURLPath — URL-префикс раздачи записей (по умолчанию /records/).
	RecordsURLPath string
}

// FromRecordings выводит Config из базового URL раздачи записей: хост и схема
// берутся из base_url, порт — из URL (или webPort, если порт не указан), а
// URL-префикс — из пути base_url.
func FromRecordings(baseURL string, webPort int, dnsProvider string, allowedIPs []string, recordsDir string) (Config, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return Config{}, fmt.Errorf("recordings.base_url: ожидается абсолютный http(s)-URL, получено %q", baseURL)
	}

	port := u.Port()
	if port == "" {
		port = strconv.Itoa(webPort)
	}

	path := strings.TrimSpace(u.Path)
	if path == "" {
		path = defaultRecordsURLPath
	}

	return Config{
		SiteAddress:    "https://" + net.JoinHostPort(u.Hostname(), port),
		DNSProvider:    dnsProvider,
		AllowedIPs:     allowedIPs,
		RecordsPath:    recordsDir,
		RecordsURLPath: path,
	}, nil
}

// normalized приводит URL-префикс записей к виду «/…/».
func (c Config) normalized() Config {
	if strings.TrimSpace(c.RecordsURLPath) == "" {
		c.RecordsURLPath = defaultRecordsURLPath
	}
	if !strings.HasPrefix(c.RecordsURLPath, "/") {
		c.RecordsURLPath = "/" + c.RecordsURLPath
	}
	if !strings.HasSuffix(c.RecordsURLPath, "/") {
		c.RecordsURLPath += "/"
	}
	return c
}

// pathPrefixPattern возвращает маску Caddy для URL-префикса записей, например
// «/records/*».
func (c Config) pathPrefixPattern() string {
	return strings.TrimSuffix(c.RecordsURLPath, "/") + "/*"
}

// Render возвращает содержимое Caddyfile.
func (c Config) Render() string {
	c = c.normalized()

	var b strings.Builder
	b.WriteString("# Файл сгенерирован сервисом ast2okdesk, ручные правки будут перезаписаны.\n")
	b.WriteString("# Листинг каталогов отключён: Caddy не отдаёт содержимое каталогов по умолчанию.\n")
	// Глобальные параметры: отключаем автоматический HTTP->HTTPS-редирект, иначе
	// Caddy пытается слушать порт 80, недоступный непривилегированному
	// пользователю. Раздача идёт только на caddy.web_port (TLS через DNS-01).
	b.WriteString("{\n")
	b.WriteString("\tauto_https disable_redirects\n")
	b.WriteString("}\n\n")
	b.WriteString(c.SiteAddress)
	b.WriteString(" {\n")
	b.WriteString("\ttls {\n")
	b.WriteString("\t\tdns ")
	b.WriteString(c.DNSProvider)
	b.WriteString(" {\n")
	// Учётные данные DNS-провайдера (reg.ru) подставляет Caddy из окружения
	// процесса (EnvironmentFile=/opt/ast2okdesk/caddy.env), поэтому в файле
	// хранятся только плейсхолдеры, а не секреты.
	b.WriteString("\t\t\tusername {$REGRU_USERNAME}\n")
	b.WriteString("\t\t\tpassword {$REGRU_PASSWORD}\n")
	b.WriteString("\t\t}\n")
	b.WriteString("\t}\n\n")
	b.WriteString("\thandle_path ")
	b.WriteString(c.pathPrefixPattern())
	b.WriteString(" {\n")
	b.WriteString("\t\t@denied not remote_ip ")
	b.WriteString(strings.Join(c.AllowedIPs, " "))
	b.WriteString("\n")
	b.WriteString("\t\trespond @denied 403\n\n")
	b.WriteString("\t\troot * ")
	b.WriteString(c.RecordsPath)
	b.WriteString("\n")
	b.WriteString("\t\tfile_server\n")
	b.WriteString("\t}\n\n")
	b.WriteString("\trespond 404\n")
	b.WriteString("}\n")
	return b.String()
}

// Validate проверяет обязательные параметры и формат allowlist.
func (c Config) Validate() error {
	c = c.normalized()

	var errs []string
	if strings.TrimSpace(c.SiteAddress) == "" || strings.ContainsAny(c.SiteAddress, " \t\r\n") {
		errs = append(errs, "site address: обязательное поле без пробелов")
	}
	if strings.TrimSpace(c.DNSProvider) == "" {
		errs = append(errs, "dns provider: обязательное поле не заполнено")
	}
	if len(c.AllowedIPs) == 0 {
		errs = append(errs, "allowed ips: требуется хотя бы один IP-адрес или CIDR")
	}
	for i, raw := range c.AllowedIPs {
		if !validIPOrCIDR(raw) {
			errs = append(errs, fmt.Sprintf("allowed_ips[%d]: %q не является IP-адресом или CIDR", i, raw))
		}
	}
	if strings.TrimSpace(c.RecordsPath) == "" {
		errs = append(errs, "records path: обязательное поле не заполнено")
	}
	if !strings.HasPrefix(c.RecordsURLPath, "/") {
		errs = append(errs, "records url path: должен начинаться с «/»")
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("конфигурация caddy невалидна: %s", strings.Join(errs, "; "))
}

// Runner выполняет внешнюю команду с учётом контекста.
type Runner func(ctx context.Context, name string, args ...string) error

// Manager записывает Caddyfile и перезагружает Caddy.
type Manager struct {
	cfg        Config
	configPath string
	binary     string
	run        Runner
	log        *slog.Logger
}

// NewManager создаёт Manager. binary — путь к исполняемому файлу caddy.
func NewManager(cfg Config, configPath, binary string, logger *slog.Logger) (*Manager, error) {
	if strings.TrimSpace(configPath) == "" {
		return nil, fmt.Errorf("caddy: не задан путь к Caddyfile")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		cfg:        cfg,
		configPath: configPath,
		binary:     binary,
		run:        execRunner,
		log:        logger,
	}, nil
}

// ConfigPath возвращает путь к Caddyfile.
func (m *Manager) ConfigPath() string { return m.configPath }

// Render возвращает содержимое Caddyfile.
func (m *Manager) Render() string { return m.cfg.Render() }

// Apply записывает Caddyfile и просит Caddy перечитать конфигурацию. Если путь к
// исполняемому файлу не задан, запись выполняется без перезагрузки.
func (m *Manager) Apply(ctx context.Context) error {
	if err := m.writeFile(); err != nil {
		return err
	}
	if strings.TrimSpace(m.binary) == "" {
		m.log.Warn("caddy: путь к исполняемому файлу не задан, перезагрузка пропущена",
			"config", m.configPath)
		return nil
	}
	return m.Reload(ctx)
}

// writeFile атомарно записывает Caddyfile: сначала во временный файл в том же
// каталоге, затем переименование.
func (m *Manager) writeFile() error {
	dir := filepath.Dir(m.configPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("caddy: подготовка каталога %q: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".Caddyfile-*")
	if err != nil {
		return fmt.Errorf("caddy: создание временного файла: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.WriteString(m.Render()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("caddy: запись Caddyfile: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("caddy: права на Caddyfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("caddy: закрытие временного файла: %w", err)
	}
	if err := os.Rename(tmpName, m.configPath); err != nil {
		return fmt.Errorf("caddy: публикация Caddyfile %q: %w", m.configPath, err)
	}
	return nil
}

// Reload выполняет «caddy reload --config <path> --adapter caddyfile».
func (m *Manager) Reload(ctx context.Context) error {
	args := []string{"reload", "--config", m.configPath, "--adapter", "caddyfile"}
	if err := m.run(ctx, m.binary, args...); err != nil {
		return fmt.Errorf("caddy: перезагрузка конфигурации: %w", err)
	}
	m.log.Info("caddy: конфигурация применена", "config", m.configPath)
	return nil
}

// execRunner — Runner по умолчанию.
func execRunner(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
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
