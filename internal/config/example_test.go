package config

import (
	"path/filepath"
	"testing"
)

// TestExampleConfigLoads проверяет, что поставляемый пример config.example.toml
// синхронизирован со схемой конфигурации и успешно проходит загрузку и
// валидацию. Так опечатки и расхождения в примере ловятся тестами.
func TestExampleConfigLoads(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.toml")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q) вернул ошибку: %v", path, err)
	}
	if cfg.Asterisk.Queue == "" {
		t.Fatal("в примере не заполнен asterisk.queue")
	}
	if cfg.Okdesk.BaseURL == "" {
		t.Fatal("в примере не заполнен okdesk.base_url")
	}
	if len(cfg.Employees) == 0 {
		t.Fatal("в примере не заполнен список [[employees]]")
	}
	if cfg.Okdesk.AutoLinkIssue {
		t.Fatal("в примере okdesk.auto_link_issue должен быть false (привязку выполняет координатор)")
	}
}
