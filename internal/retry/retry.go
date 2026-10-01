// Package retry реализует воркер повторной доставки записей о телефонном
// разговоре в Okdesk. Записи хранятся в SQLite (переживают рестарт сервиса),
// отправляются с экспоненциальной задержкой; после исчерпания попыток запись
// удаляется с аварийным сообщением в журнал.
package retry

import (
	"context"
	"log/slog"
	"time"

	"github.com/ovn25519/ast2okdesk/internal/store"
)

// Значения по умолчанию.
const (
	defaultMaxAttempts    = 8
	defaultInitialBackoff = 5 * time.Second
	defaultMaxBackoff     = time.Hour
	defaultPollInterval   = 10 * time.Second
	defaultBatchSize      = 50
)

// Store — подмножество хранилища, необходимое воркеру.
type Store interface {
	DueRetries(ctx context.Context, now time.Time, limit int) ([]store.RetryItem, error)
	RescheduleRetry(ctx context.Context, id int64, nextAttemptAt time.Time, lastErr string) error
	DeleteRetry(ctx context.Context, id int64) error
}

// Sender отправляет заранее собранный payload phone_call.
type Sender interface {
	SendPhoneCall(ctx context.Context, payload []byte) error
}

// Config — параметры воркера.
type Config struct {
	// MaxAttempts — предел попыток (включая первичную отправку).
	MaxAttempts int
	// InitialBackoff — задержка перед второй попыткой.
	InitialBackoff time.Duration
	// MaxBackoff — верхняя граница экспоненциальной задержки.
	MaxBackoff time.Duration
	// PollInterval — период опроса очереди.
	PollInterval time.Duration
	// BatchSize — максимум записей за один проход.
	BatchSize int
}

// Worker разбирает очередь retry и досылает записи.
type Worker struct {
	store  Store
	sender Sender
	cfg    Config
	log    *slog.Logger
	now    func() time.Time
}

// New создаёт воркер, подставляя значения по умолчанию.
func New(st Store, sender Sender, cfg Config, logger *slog.Logger) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultMaxAttempts
	}
	if cfg.InitialBackoff <= 0 {
		cfg.InitialBackoff = defaultInitialBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = defaultMaxBackoff
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultBatchSize
	}
	return &Worker{store: st, sender: sender, cfg: cfg, log: logger, now: time.Now}
}

// Run разбирает очередь до отмены контекста.
func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()

	// Первый проход сразу при старте: поднимаем очередь после рестарта.
	if err := w.ProcessDue(ctx); err != nil {
		w.log.Error("retry: стартовый проход", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := w.ProcessDue(ctx); err != nil {
				w.log.Error("retry: проход очереди", "error", err)
			}
		}
	}
}

// ProcessDue обрабатывает все записи, срок которых наступил.
func (w *Worker) ProcessDue(ctx context.Context) error {
	items, err := w.store.DueRetries(ctx, w.now(), w.cfg.BatchSize)
	if err != nil {
		return err
	}
	for _, item := range items {
		if err := w.process(ctx, item); err != nil {
			w.log.Error("retry: обработка записи", "call_id", item.CallID, "error", err)
		}
	}
	return nil
}

// process обрабатывает одну запись очереди.
func (w *Worker) process(ctx context.Context, item store.RetryItem) error {
	// Попытки могли быть исчерпаны ещё до рестарта.
	if item.Attempts >= w.cfg.MaxAttempts {
		w.alert(item, "исчерпано попыток до повторной отправки")
		return w.store.DeleteRetry(ctx, item.ID)
	}

	if err := w.sender.SendPhoneCall(ctx, []byte(item.Payload)); err != nil {
		attempt := item.Attempts + 1
		if attempt >= w.cfg.MaxAttempts {
			w.alert(item, err.Error())
			return w.store.DeleteRetry(ctx, item.ID)
		}
		next := w.now().Add(w.backoff(attempt))
		w.log.Warn("retry: повторная отправка не удалась",
			"call_id", item.CallID, "attempt", attempt, "next_attempt_at", next, "error", err)
		return w.store.RescheduleRetry(ctx, item.ID, next, err.Error())
	}

	w.log.Info("retry: запись о звонке доставлена", "call_id", item.CallID, "attempt", item.Attempts+1)
	return w.store.DeleteRetry(ctx, item.ID)
}

// backoff вычисляет задержку перед попыткой номер attempt (1 — первая
// повторная).
func (w *Worker) backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := w.cfg.InitialBackoff
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= w.cfg.MaxBackoff {
			return w.cfg.MaxBackoff
		}
	}
	if d > w.cfg.MaxBackoff {
		d = w.cfg.MaxBackoff
	}
	return d
}

// alert фиксирует аварийную ситуацию: запись о звонке не доставлена.
func (w *Worker) alert(item store.RetryItem, reason string) {
	w.log.Error("ALERT: запись о звонке не доставлена в Okdesk, запись удалена из очереди",
		"call_id", item.CallID, "attempts", item.Attempts, "reason", reason)
}

// Убеждаемся, что SQLite-хранилище удовлетворяет интерфейсу воркера.
var _ Store = (*store.Store)(nil)
