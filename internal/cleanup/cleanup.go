// Package cleanup реализует фоновый джоб обслуживания SQLite: удаляет
// «осиротевшие» корреляции звонков и записи дедупликации старше TTL и
// возвращает освобождённые страницы файлу базы (incremental_vacuum).
package cleanup

import (
	"context"
	"log/slog"
	"time"

	"github.com/ovn25519/ast2okdesk/internal/store"
)

// Store — подмножество хранилища, необходимое джобу.
type Store interface {
	DeleteStaleCalls(ctx context.Context, before time.Time) (int64, error)
	DeleteStaleDedup(ctx context.Context, before time.Time) (int64, error)
	IncrementalVacuum(ctx context.Context) error
}

// Config — параметры уборки.
type Config struct {
	// TTL — возраст, после которого корреляция/дедуп считаются осиротевшими.
	TTL time.Duration
	// Interval — период запуска уборки.
	Interval time.Duration
}

// Result — итог одной уборки.
type Result struct {
	Calls int64
	Dedup int64
}

// Cleaner периодически выполняет уборку.
type Cleaner struct {
	store Store
	cfg   Config
	log   *slog.Logger
	now   func() time.Time
}

// New создаёт уборщик. При нулевом TTL записи не удаляются (защита от
// случайного стирания всей базы).
func New(st Store, cfg Config, logger *slog.Logger) *Cleaner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Cleaner{store: st, cfg: cfg, log: logger, now: time.Now}
}

// Run выполняет уборку до отмены контекста.
func (c *Cleaner) Run(ctx context.Context) error {
	if c.cfg.Interval <= 0 {
		c.log.Warn("интервал уборки не задан, джоб не запущен")
		return nil
	}
	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if _, err := c.Once(ctx); err != nil {
				c.log.Error("уборка хранилища", "error", err)
			}
		}
	}
}

// Once выполняет один проход уборки и возвращает число удалённых записей.
func (c *Cleaner) Once(ctx context.Context) (Result, error) {
	if c.cfg.TTL <= 0 {
		return Result{}, nil
	}
	before := c.now().Add(-c.cfg.TTL)

	calls, err := c.store.DeleteStaleCalls(ctx, before)
	if err != nil {
		return Result{}, err
	}
	dedup, err := c.store.DeleteStaleDedup(ctx, before)
	if err != nil {
		return Result{Calls: calls}, err
	}
	if err := c.store.IncrementalVacuum(ctx); err != nil {
		return Result{Calls: calls, Dedup: dedup}, err
	}

	res := Result{Calls: calls, Dedup: dedup}
	if calls > 0 || dedup > 0 {
		c.log.Info("уборка хранилища", "calls", calls, "dedup", dedup)
	}
	return res, nil
}

// Убеждаемся, что SQLite-хранилище удовлетворяет интерфейсу джоба.
var _ Store = (*store.Store)(nil)
