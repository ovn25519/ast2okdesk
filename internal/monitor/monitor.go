// Package monitor реализует наблюдаемость сервиса: потокобезопасные счётчики
// обращений к Okdesk и периодический дамп состояния в журнал (доступность AMI,
// глубина очереди retry, приход событий Cdr, доступность каталога записей).
//
// Выделенный HTTP-эндпоинт метрик не используется — по требованиям достаточно
// структурированных записей в логе.
package monitor

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/ovn25519/ast2okdesk/internal/ami"
)

// defaultInterval — период дампа по умолчанию.
const defaultInterval = time.Minute

// Counters — счётчики обращений к Okdesk. Реализует okdesk.Observer.
type Counters struct {
	apiTotal  atomic.Int64
	apiFailed atomic.Int64
}

// ObserveAPI учитывает результат одного обращения к Okdesk.
func (c *Counters) ObserveAPI(_ string, err error) {
	c.apiTotal.Add(1)
	if err != nil {
		c.apiFailed.Add(1)
	}
}

// Snapshot — снимок счётчиков.
type Snapshot struct {
	// APITotal — всего обращений к Okdesk.
	APITotal int64
	// APIFailed — обращений, завершившихся ошибкой.
	APIFailed int64
}

// Snapshot возвращает текущие значения счётчиков.
func (c *Counters) Snapshot() Snapshot {
	return Snapshot{APITotal: c.apiTotal.Load(), APIFailed: c.apiFailed.Load()}
}

// AMISource предоставляет статистику AMI-подключения.
type AMISource interface {
	Stats() ami.Stats
}

// RetrySource предоставляет глубину очереди retry.
type RetrySource interface {
	RetryDepth(ctx context.Context) (int, error)
}

// CDRSource предоставляет число принятых событий Cdr.
type CDRSource interface {
	CDRCount() int64
}

// Config — параметры монитора.
type Config struct {
	// Interval — период дампа; при <= 0 используется 1 минута.
	Interval time.Duration
	// FilesDir — каталог записей; пустая строка отключает проверку.
	FilesDir string
}

// Monitor периодически публикует состояние сервиса в журнал.
type Monitor struct {
	cfg      Config
	log      *slog.Logger
	counters *Counters
	ami      AMISource
	retry    RetrySource
	cdr      CDRSource
}

// New создаёт монитор. Любой из источников может быть nil — тогда
// соответствующая группа полей опускается.
func New(cfg Config, counters *Counters, amiSrc AMISource, retry RetrySource, cdr CDRSource, logger *slog.Logger) *Monitor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Monitor{cfg: cfg, log: logger, counters: counters, ami: amiSrc, retry: retry, cdr: cdr}
}

// Run публикует дампы до отмены контекста.
func (m *Monitor) Run(ctx context.Context) error {
	interval := m.cfg.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.Report(ctx)
		}
	}
}

// Report публикует один снимок состояния. Метод пригоден для вызова из тестов и
// по требованию.
func (m *Monitor) Report(ctx context.Context) {
	attrs := make([]any, 0, 16)

	if m.counters != nil {
		s := m.counters.Snapshot()
		attrs = append(attrs, "api_total", s.APITotal, "api_failed", s.APIFailed)
	}
	if m.cdr != nil {
		attrs = append(attrs, "cdr_received", m.cdr.CDRCount())
	}
	if m.retry != nil {
		if depth, err := m.retry.RetryDepth(ctx); err != nil {
			attrs = append(attrs, "retry_depth_error", err.Error())
		} else {
			attrs = append(attrs, "retry_depth", depth)
		}
	}
	if m.ami != nil {
		s := m.ami.Stats()
		attrs = append(attrs,
			"ami_connected", s.Connected,
			"ami_reconnects", s.Reconnects,
			"ami_frames", s.FramesReceived)
		if !s.LastEventAt.IsZero() {
			attrs = append(attrs, "ami_last_event", s.LastEventAt.Format(time.RFC3339))
		}
	}
	if m.cfg.FilesDir != "" {
		if _, err := os.Stat(m.cfg.FilesDir); err != nil {
			m.log.Warn("файл-сервер записей недоступен", "dir", m.cfg.FilesDir, "error", err)
			attrs = append(attrs, "files_dir_ok", false)
		} else {
			attrs = append(attrs, "files_dir_ok", true)
		}
	}

	m.log.Info("мониторинг", attrs...)
}
