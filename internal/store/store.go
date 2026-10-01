// Package store реализует хранилище состояния сервиса на SQLite: корреляцию
// звонка по Uniqueid, дедупликацию screen-pop и очередь повторной отправки
// записей о разговоре.
//
// Хранилище рассчитано на один писатель: пул соединений ограничен одним
// открытым соединением, журнал — WAL. База переживает рестарт сервиса.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // регистрирует драйвер "sqlite" (чистый Go, без CGO)
)

// ErrNotFound возвращается, когда запись отсутствует в хранилище.
var ErrNotFound = errors.New("запись не найдена")

// timeLayout — единый формат хранения времени (UTC, фиксированная ширина,
// корректная лексикографическая сортировка).
const timeLayout = time.RFC3339

// CallStatus — состояние звонка в очереди.
type CallStatus string

const (
	// StatusQueued — звонок вошёл в очередь, оператору ещё не звонили.
	StatusQueued CallStatus = "queued"
	// StatusAnswered — оператор ответил (AgentConnect).
	StatusAnswered CallStatus = "answered"
	// StatusAbandoned — клиент ушёл, не дождавшись (QueueCallerAbandon).
	StatusAbandoned CallStatus = "abandoned"
)

// Call — корреляция звонка по Uniqueid.
type Call struct {
	Uniqueid    string
	Linkedid    string
	CallerIDNum string
	Queue       string
	Status      CallStatus
	AgentPeer   string
	CreatedAt   time.Time
	AnsweredAt  *time.Time
	FinishedAt  *time.Time
	UpdatedAt   time.Time
}

// RetryItem — запись очереди повторной отправки phone_calls.
type RetryItem struct {
	ID            int64
	CallID        string
	Payload       string
	Attempts      int
	NextAttemptAt time.Time
	LastError     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// NewRetryItem — входные данные для постановки записи в очередь.
type NewRetryItem struct {
	CallID        string
	Payload       string
	NextAttemptAt time.Time
}

// Store — обёртка над SQLite.
type Store struct {
	db *sql.DB
}

// Open открывает (при необходимости — создаёт) базу по пути path, включает
// WAL и auto_vacuum=INCREMENTAL и применяет миграции схемы.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("открытие БД %q: %w", path, err)
	}

	// Один писатель: исключает competition за блокировку записи SQLite.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db}

	// auto_vacuum задаётся до создания первой таблицы; для непустой БД это no-op.
	if _, err := db.Exec(`PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("установка auto_vacuum: %w", err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("включение WAL: %w", err)
	}

	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close закрывает соединение с БД.
func (s *Store) Close() error {
	return s.db.Close()
}

// migration — одна версия схемы.
type migration struct {
	version int
	stmts   []string
}

var migrations = []migration{
	{
		version: 1,
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS calls (
				uniqueid      TEXT PRIMARY KEY,
				linkedid      TEXT NOT NULL DEFAULT '',
				caller_id_num TEXT NOT NULL DEFAULT '',
				queue         TEXT NOT NULL DEFAULT '',
				status        TEXT NOT NULL DEFAULT 'queued',
				agent_peer    TEXT NOT NULL DEFAULT '',
				created_at    TEXT NOT NULL,
				answered_at   TEXT,
				finished_at   TEXT,
				updated_at    TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_calls_updated_at ON calls(updated_at)`,
			`CREATE TABLE IF NOT EXISTS dedup (
				uniqueid   TEXT NOT NULL,
				peer       TEXT NOT NULL,
				created_at TEXT NOT NULL,
				PRIMARY KEY (uniqueid, peer)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_dedup_created_at ON dedup(created_at)`,
			`CREATE TABLE IF NOT EXISTS retry_queue (
				id              INTEGER PRIMARY KEY AUTOINCREMENT,
				call_id         TEXT NOT NULL UNIQUE,
				payload         TEXT NOT NULL,
				attempts        INTEGER NOT NULL DEFAULT 0,
				next_attempt_at TEXT NOT NULL,
				last_error      TEXT NOT NULL DEFAULT '',
				created_at      TEXT NOT NULL,
				updated_at      TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_retry_next_attempt_at ON retry_queue(next_attempt_at)`,
		},
	},
}

// migrate применяет неприменённые миграции, отслеживая версию в PRAGMA user_version.
func (s *Store) migrate(ctx context.Context) error {
	var current int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("чтение user_version: %w", err)
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("начало транзакции миграции v%d: %w", m.version, err)
		}
		for _, stmt := range m.stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("миграция v%d: %w", m.version, err)
			}
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, m.version)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("установка user_version=%d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("фиксация миграции v%d: %w", m.version, err)
		}
	}
	return nil
}

// --- Корреляция звонка ---------------------------------------------------------

// SaveCallQueued сохраняет звонок, вошедший в очередь. Идемпотентно: повторное
// событие QueueCallerJoin обновляет данные, не сбрасывая статус.
func (s *Store) SaveCallQueued(ctx context.Context, c Call) error {
	now := formatTime(time.Now())
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO calls (uniqueid, linkedid, caller_id_num, queue, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(uniqueid) DO UPDATE SET
			linkedid      = excluded.linkedid,
			caller_id_num = excluded.caller_id_num,
			queue         = excluded.queue,
			updated_at    = excluded.updated_at`,
		c.Uniqueid, c.Linkedid, c.CallerIDNum, c.Queue, string(StatusQueued), now, now)
	if err != nil {
		return fmt.Errorf("сохранение звонка %q: %w", c.Uniqueid, err)
	}
	return nil
}

// MarkAnswered фиксирует ответ оператора (AgentConnect).
func (s *Store) MarkAnswered(ctx context.Context, uniqueid, peer string, at time.Time) error {
	return s.updateCall(ctx,
		`UPDATE calls SET status = ?, agent_peer = ?, answered_at = ?, updated_at = ? WHERE uniqueid = ?`,
		string(StatusAnswered), peer, formatTime(at), formatTime(time.Now()), uniqueid)
}

// MarkAbandoned фиксирует уход клиента из очереди (QueueCallerAbandon).
func (s *Store) MarkAbandoned(ctx context.Context, uniqueid string, at time.Time) error {
	return s.updateCall(ctx,
		`UPDATE calls SET status = ?, finished_at = ?, updated_at = ? WHERE uniqueid = ?`,
		string(StatusAbandoned), formatTime(at), formatTime(time.Now()), uniqueid)
}

// MarkFinished фиксирует завершение звонка (Hangup/Cdr).
func (s *Store) MarkFinished(ctx context.Context, uniqueid string, at time.Time) error {
	return s.updateCall(ctx,
		`UPDATE calls SET finished_at = ?, updated_at = ? WHERE uniqueid = ?`,
		formatTime(at), formatTime(time.Now()), uniqueid)
}

// updateCall выполняет UPDATE и возвращает ErrNotFound, если строка не найдена.
func (s *Store) updateCall(ctx context.Context, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("обновление звонка: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("проверка результата обновления: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetCall возвращает корреляцию звонка по Uniqueid.
func (s *Store) GetCall(ctx context.Context, uniqueid string) (Call, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT uniqueid, linkedid, caller_id_num, queue, status, agent_peer,
			created_at, answered_at, finished_at, updated_at
		 FROM calls WHERE uniqueid = ?`, uniqueid)

	c, err := scanCall(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Call{}, ErrNotFound
	}
	if err != nil {
		return Call{}, fmt.Errorf("чтение звонка %q: %w", uniqueid, err)
	}
	return c, nil
}

// DeleteCall удаляет корреляцию звонка.
func (s *Store) DeleteCall(ctx context.Context, uniqueid string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM calls WHERE uniqueid = ?`, uniqueid); err != nil {
		return fmt.Errorf("удаление звонка %q: %w", uniqueid, err)
	}
	return nil
}

// ListCallsOlderThan возвращает звонки, не обновлявшиеся с момента before
// (кандидаты на удаление по TTL).
func (s *Store) ListCallsOlderThan(ctx context.Context, before time.Time) ([]Call, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT uniqueid, linkedid, caller_id_num, queue, status, agent_peer,
			created_at, answered_at, finished_at, updated_at
		 FROM calls WHERE updated_at < ? ORDER BY updated_at`, formatTime(before))
	if err != nil {
		return nil, fmt.Errorf("выборка устаревших звонков: %w", err)
	}
	defer rows.Close()

	var calls []Call
	for rows.Next() {
		c, err := scanCall(rows)
		if err != nil {
			return nil, fmt.Errorf("чтение устаревшего звонка: %w", err)
		}
		calls = append(calls, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("обход устаревших звонков: %w", err)
	}
	return calls, nil
}

// DeleteStaleCalls удаляет звонки, не обновлявшиеся с момента before, и
// возвращает число удалённых строк.
func (s *Store) DeleteStaleCalls(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM calls WHERE updated_at < ?`, formatTime(before))
	if err != nil {
		return 0, fmt.Errorf("удаление устаревших звонков: %w", err)
	}
	return res.RowsAffected()
}

// --- Дедупликация screen-pop ---------------------------------------------------

// ClaimDedup атомарно регистрирует пару (Uniqueid, peer) и сообщает, была ли она
// зарегистрирована впервые. false означает, что событие уже обрабатывалось.
func (s *Store) ClaimDedup(ctx context.Context, uniqueid, peer string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO dedup (uniqueid, peer, created_at) VALUES (?, ?, ?)`,
		uniqueid, peer, formatTime(time.Now()))
	if err != nil {
		return false, fmt.Errorf("дедупликация %q/%q: %w", uniqueid, peer, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("проверка дедупликации: %w", err)
	}
	return n > 0, nil
}

// DeleteDedupByCall удаляет все записи дедупликации звонка.
func (s *Store) DeleteDedupByCall(ctx context.Context, uniqueid string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM dedup WHERE uniqueid = ?`, uniqueid); err != nil {
		return fmt.Errorf("удаление дедупликации %q: %w", uniqueid, err)
	}
	return nil
}

// DeleteStaleDedup удаляет записи дедупликации старше before.
func (s *Store) DeleteStaleDedup(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM dedup WHERE created_at < ?`, formatTime(before))
	if err != nil {
		return 0, fmt.Errorf("удаление устаревшей дедупликации: %w", err)
	}
	return res.RowsAffected()
}

// --- Очередь повторной отправки ------------------------------------------------

// EnqueueRetry ставит запись в очередь. Повторная постановка того же call_id
// игнорируется (created=false): один отложенный журнал на звонок.
func (s *Store) EnqueueRetry(ctx context.Context, item NewRetryItem) (bool, error) {
	now := formatTime(time.Now())
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO retry_queue (call_id, payload, attempts, next_attempt_at, created_at, updated_at)
		 VALUES (?, ?, 0, ?, ?, ?)`,
		item.CallID, item.Payload, formatTime(item.NextAttemptAt), now, now)
	if err != nil {
		return false, fmt.Errorf("постановка в retry %q: %w", item.CallID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("проверка постановки в retry: %w", err)
	}
	return n > 0, nil
}

// DueRetries возвращает записи, готовые к отправке (next_attempt_at <= now),
// в порядке возрастания времени попытки.
func (s *Store) DueRetries(ctx context.Context, now time.Time, limit int) ([]RetryItem, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, call_id, payload, attempts, next_attempt_at, last_error, created_at, updated_at
		 FROM retry_queue WHERE next_attempt_at <= ? ORDER BY next_attempt_at, id LIMIT ?`,
		formatTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("выборка очереди retry: %w", err)
	}
	defer rows.Close()

	var items []RetryItem
	for rows.Next() {
		it, err := scanRetry(rows)
		if err != nil {
			return nil, fmt.Errorf("чтение записи retry: %w", err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("обход очереди retry: %w", err)
	}
	return items, nil
}

// RescheduleRetry увеличивает счётчик попыток и назначает следующее время.
func (s *Store) RescheduleRetry(ctx context.Context, id int64, nextAttemptAt time.Time, lastErr string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE retry_queue
		 SET attempts = attempts + 1, next_attempt_at = ?, last_error = ?, updated_at = ?
		 WHERE id = ?`,
		formatTime(nextAttemptAt), lastErr, formatTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("перенос попытки retry id=%d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("проверка переноса retry: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteRetry удаляет запись очереди (успех или исчерпание попыток).
func (s *Store) DeleteRetry(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM retry_queue WHERE id = ?`, id); err != nil {
		return fmt.Errorf("удаление retry id=%d: %w", id, err)
	}
	return nil
}

// RetryDepth возвращает текущее число записей в очереди retry.
func (s *Store) RetryDepth(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM retry_queue`).Scan(&n); err != nil {
		return 0, fmt.Errorf("подсчёт очереди retry: %w", err)
	}
	return n, nil
}

// IncrementalVacuum возвращает освобождённые страницы файлу БД.
func (s *Store) IncrementalVacuum(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA incremental_vacuum`); err != nil {
		return fmt.Errorf("incremental_vacuum: %w", err)
	}
	return nil
}

// --- Вспомогательные -----------------------------------------------------------

// rowScanner — общий интерфейс *sql.Row и *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanCall(sc rowScanner) (Call, error) {
	var (
		c                      Call
		status                 string
		created, updated       string
		answeredAt, finishedAt sql.NullString
	)
	if err := sc.Scan(&c.Uniqueid, &c.Linkedid, &c.CallerIDNum, &c.Queue, &status,
		&c.AgentPeer, &created, &answeredAt, &finishedAt, &updated); err != nil {
		return Call{}, err
	}
	c.Status = CallStatus(status)

	var err error
	if c.CreatedAt, err = parseTime(created); err != nil {
		return Call{}, err
	}
	if c.UpdatedAt, err = parseTime(updated); err != nil {
		return Call{}, err
	}
	if c.AnsweredAt, err = parseNullTime(answeredAt); err != nil {
		return Call{}, err
	}
	if c.FinishedAt, err = parseNullTime(finishedAt); err != nil {
		return Call{}, err
	}
	return c, nil
}

func scanRetry(sc rowScanner) (RetryItem, error) {
	var (
		it                     RetryItem
		next, created, updated string
	)
	if err := sc.Scan(&it.ID, &it.CallID, &it.Payload, &it.Attempts, &next,
		&it.LastError, &created, &updated); err != nil {
		return RetryItem{}, err
	}
	var err error
	if it.NextAttemptAt, err = parseTime(next); err != nil {
		return RetryItem{}, err
	}
	if it.CreatedAt, err = parseTime(created); err != nil {
		return RetryItem{}, err
	}
	if it.UpdatedAt, err = parseTime(updated); err != nil {
		return RetryItem{}, err
	}
	return it, nil
}

func parseNullTime(v sql.NullString) (*time.Time, error) {
	if !v.Valid || v.String == "" {
		return nil, nil
	}
	t, err := parseTime(v.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("разбор времени %q: %w", s, err)
	}
	return t, nil
}
