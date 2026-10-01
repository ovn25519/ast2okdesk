package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// openTestStore создаёт БД во временном каталоге и закрывает её по завершении теста.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "okdesk.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func fixedTime() time.Time {
	return time.Date(2026, 9, 30, 9, 16, 0, 0, time.UTC)
}

func TestOpenPragmas(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	var journalMode string
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %q, ожидался wal", journalMode)
	}

	var autoVacuum int
	if err := s.db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&autoVacuum); err != nil {
		t.Fatalf("PRAGMA auto_vacuum: %v", err)
	}
	if autoVacuum != 2 { // 0=NONE, 1=FULL, 2=INCREMENTAL
		t.Errorf("auto_vacuum = %d, ожидалось 2 (INCREMENTAL)", autoVacuum)
	}

	var userVersion int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&userVersion); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if userVersion != len(migrations) {
		t.Errorf("user_version = %d, ожидалось %d", userVersion, len(migrations))
	}
}

func TestCallLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.SaveCallQueued(ctx, Call{
		Uniqueid:    "u-1",
		Linkedid:    "l-1",
		CallerIDNum: "+79991234567",
		Queue:       "support",
	}); err != nil {
		t.Fatalf("SaveCallQueued: %v", err)
	}

	c, err := s.GetCall(ctx, "u-1")
	if err != nil {
		t.Fatalf("GetCall: %v", err)
	}
	if c.Status != StatusQueued {
		t.Errorf("status = %q, ожидался queued", c.Status)
	}
	if c.CallerIDNum != "+79991234567" || c.Queue != "support" || c.Linkedid != "l-1" {
		t.Errorf("некорректные данные звонка: %+v", c)
	}
	if c.AnsweredAt != nil || c.FinishedAt != nil {
		t.Errorf("answered_at/finished_at должны быть пустыми: %+v", c)
	}

	at := fixedTime()
	if err := s.MarkAnswered(ctx, "u-1", "ujin327", at); err != nil {
		t.Fatalf("MarkAnswered: %v", err)
	}
	c, err = s.GetCall(ctx, "u-1")
	if err != nil {
		t.Fatalf("GetCall после MarkAnswered: %v", err)
	}
	if c.Status != StatusAnswered || c.AgentPeer != "ujin327" {
		t.Errorf("после ответа: %+v", c)
	}
	if c.AnsweredAt == nil || !c.AnsweredAt.Equal(at) {
		t.Errorf("answered_at = %v, ожидалось %v", c.AnsweredAt, at)
	}

	if err := s.MarkFinished(ctx, "u-1", at.Add(time.Minute)); err != nil {
		t.Fatalf("MarkFinished: %v", err)
	}
	c, _ = s.GetCall(ctx, "u-1")
	if c.FinishedAt == nil || !c.FinishedAt.Equal(at.Add(time.Minute)) {
		t.Errorf("finished_at = %v", c.FinishedAt)
	}

	if err := s.DeleteCall(ctx, "u-1"); err != nil {
		t.Fatalf("DeleteCall: %v", err)
	}
	if _, err := s.GetCall(ctx, "u-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("после удаления ожидался ErrNotFound, получено %v", err)
	}
}

func TestSaveCallQueuedIdempotentKeepsStatus(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := fixedTime()

	call := Call{Uniqueid: "u-1", CallerIDNum: "+79990000000", Queue: "support"}
	if err := s.SaveCallQueued(ctx, call); err != nil {
		t.Fatalf("SaveCallQueued: %v", err)
	}
	if err := s.MarkAnswered(ctx, "u-1", "peer", at); err != nil {
		t.Fatalf("MarkAnswered: %v", err)
	}
	// Повторное QueueCallerJoin не должно сбрасывать статус.
	if err := s.SaveCallQueued(ctx, call); err != nil {
		t.Fatalf("повторный SaveCallQueued: %v", err)
	}
	c, err := s.GetCall(ctx, "u-1")
	if err != nil {
		t.Fatalf("GetCall: %v", err)
	}
	if c.Status != StatusAnswered {
		t.Errorf("статус сброшен на %q", c.Status)
	}
}

func TestMarkAbandoned(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.SaveCallQueued(ctx, Call{Uniqueid: "u-2", CallerIDNum: "123", Queue: "q"}); err != nil {
		t.Fatalf("SaveCallQueued: %v", err)
	}
	if err := s.MarkAbandoned(ctx, "u-2", fixedTime()); err != nil {
		t.Fatalf("MarkAbandoned: %v", err)
	}
	c, err := s.GetCall(ctx, "u-2")
	if err != nil {
		t.Fatalf("GetCall: %v", err)
	}
	if c.Status != StatusAbandoned || c.FinishedAt == nil {
		t.Errorf("после ухода клиента: %+v", c)
	}
}

func TestUpdateCallNotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.MarkAnswered(ctx, "нет", "peer", fixedTime()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("MarkAnswered: ожидался ErrNotFound, получено %v", err)
	}
	if err := s.MarkAbandoned(ctx, "нет", fixedTime()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("MarkAbandoned: ожидался ErrNotFound, получено %v", err)
	}
	if err := s.MarkFinished(ctx, "нет", fixedTime()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("MarkFinished: ожидался ErrNotFound, получено %v", err)
	}
}

func TestClaimDedup(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	first, err := s.ClaimDedup(ctx, "u-1", "peer1")
	if err != nil {
		t.Fatalf("ClaimDedup: %v", err)
	}
	if !first {
		t.Error("первая регистрация должна вернуть true")
	}
	first, err = s.ClaimDedup(ctx, "u-1", "peer1")
	if err != nil {
		t.Fatalf("повторный ClaimDedup: %v", err)
	}
	if first {
		t.Error("повторная регистрация должна вернуть false")
	}
	// Другой peer для того же звонка — отдельная запись (стратегия ringall).
	other, err := s.ClaimDedup(ctx, "u-1", "peer2")
	if err != nil {
		t.Fatalf("ClaimDedup peer2: %v", err)
	}
	if !other {
		t.Error("другой peer должен регистрироваться отдельно")
	}

	if err := s.DeleteDedupByCall(ctx, "u-1"); err != nil {
		t.Fatalf("DeleteDedupByCall: %v", err)
	}
	again, err := s.ClaimDedup(ctx, "u-1", "peer1")
	if err != nil {
		t.Fatalf("ClaimDedup после удаления: %v", err)
	}
	if !again {
		t.Error("после удаления дедупликации регистрация снова должна возвращать true")
	}
}

func TestEnqueueRetry(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := fixedTime()

	created, err := s.EnqueueRetry(ctx, NewRetryItem{CallID: "u-1", Payload: `{"a":1}`, NextAttemptAt: now})
	if err != nil {
		t.Fatalf("EnqueueRetry: %v", err)
	}
	if !created {
		t.Error("первая постановка должна вернуть created=true")
	}
	created, err = s.EnqueueRetry(ctx, NewRetryItem{CallID: "u-1", Payload: `{"a":2}`, NextAttemptAt: now})
	if err != nil {
		t.Fatalf("повторная EnqueueRetry: %v", err)
	}
	if created {
		t.Error("повторная постановка того же call_id не должна создавать запись")
	}

	depth, err := s.RetryDepth(ctx)
	if err != nil {
		t.Fatalf("RetryDepth: %v", err)
	}
	if depth != 1 {
		t.Errorf("глубина очереди = %d, ожидалось 1", depth)
	}
}

func TestDueRetriesOrderAndLimit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := fixedTime()

	items := []NewRetryItem{
		{CallID: "c", Payload: "3", NextAttemptAt: base.Add(2 * time.Second)},
		{CallID: "a", Payload: "1", NextAttemptAt: base},                      // готов
		{CallID: "b", Payload: "2", NextAttemptAt: base.Add(1 * time.Second)}, // готов
		{CallID: "d", Payload: "4", NextAttemptAt: base.Add(time.Hour)},       // ещё не готов
	}
	for _, it := range items {
		if _, err := s.EnqueueRetry(ctx, it); err != nil {
			t.Fatalf("EnqueueRetry %q: %v", it.CallID, err)
		}
	}

	due, err := s.DueRetries(ctx, base.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("DueRetries: %v", err)
	}
	if len(due) != 3 {
		t.Fatalf("готовых записей = %d, ожидалось 3", len(due))
	}
	wantOrder := []string{"a", "b", "c"}
	for i, it := range due {
		if it.CallID != wantOrder[i] {
			t.Errorf("порядок[%d] = %q, ожидалось %q", i, it.CallID, wantOrder[i])
		}
	}
	if due[0].Attempts != 0 {
		t.Errorf("начальное число попыток = %d", due[0].Attempts)
	}

	limited, err := s.DueRetries(ctx, base.Add(time.Minute), 1)
	if err != nil {
		t.Fatalf("DueRetries с limit: %v", err)
	}
	if len(limited) != 1 || limited[0].CallID != "a" {
		t.Errorf("limit не соблюдён: %+v", limited)
	}
}

func TestRescheduleAndDeleteRetry(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := fixedTime()

	if _, err := s.EnqueueRetry(ctx, NewRetryItem{CallID: "u-1", Payload: "p", NextAttemptAt: base}); err != nil {
		t.Fatalf("EnqueueRetry: %v", err)
	}
	due, err := s.DueRetries(ctx, base, 1)
	if err != nil {
		t.Fatalf("DueRetries: %v", err)
	}
	id := due[0].ID

	next := base.Add(30 * time.Second)
	if err := s.RescheduleRetry(ctx, id, next, "сеть недоступна"); err != nil {
		t.Fatalf("RescheduleRetry: %v", err)
	}
	// До next запись не готова.
	if items, _ := s.DueRetries(ctx, base.Add(time.Second), 10); len(items) != 0 {
		t.Errorf("отложенная запись не должна быть готова: %+v", items)
	}
	// После next — готова, с увеличенным счётчиком и текстом ошибки.
	items, err := s.DueRetries(ctx, next, 10)
	if err != nil {
		t.Fatalf("DueRetries после переноса: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("записей = %d, ожидалась 1", len(items))
	}
	if items[0].Attempts != 1 || items[0].LastError != "сеть недоступна" {
		t.Errorf("после переноса: %+v", items[0])
	}

	if err := s.DeleteRetry(ctx, id); err != nil {
		t.Fatalf("DeleteRetry: %v", err)
	}
	if depth, _ := s.RetryDepth(ctx); depth != 0 {
		t.Errorf("после удаления глубина = %d", depth)
	}
	if err := s.RescheduleRetry(ctx, id, next, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RescheduleRetry удалённой записи: ожидался ErrNotFound, получено %v", err)
	}
}

func TestRetryPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "okdesk.db")
	ctx := context.Background()
	base := fixedTime()

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open #1: %v", err)
	}
	if _, err := s1.EnqueueRetry(ctx, NewRetryItem{CallID: "u-1", Payload: "p", NextAttemptAt: base}); err != nil {
		t.Fatalf("EnqueueRetry: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close #1: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("Open #2: %v", err)
	}
	defer s2.Close()

	items, err := s2.DueRetries(ctx, base.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("DueRetries после рестарта: %v", err)
	}
	if len(items) != 1 || items[0].CallID != "u-1" {
		t.Fatalf("очередь retry не пережила рестарт: %+v", items)
	}
}

func TestStaleCleanup(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)

	// Свежий и устаревший звонок.
	if err := s.SaveCallQueued(ctx, Call{Uniqueid: "fresh", CallerIDNum: "1", Queue: "q"}); err != nil {
		t.Fatalf("SaveCallQueued fresh: %v", err)
	}
	if err := s.SaveCallQueued(ctx, Call{Uniqueid: "stale", CallerIDNum: "2", Queue: "q"}); err != nil {
		t.Fatalf("SaveCallQueued stale: %v", err)
	}
	// Искусственно старим updated_at у одного звонка.
	if _, err := s.db.ExecContext(ctx, `UPDATE calls SET updated_at = ? WHERE uniqueid = ?`, formatTime(old), "stale"); err != nil {
		t.Fatalf("старение звонка: %v", err)
	}

	stale, err := s.ListCallsOlderThan(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("ListCallsOlderThan: %v", err)
	}
	if len(stale) != 1 || stale[0].Uniqueid != "stale" {
		t.Fatalf("устаревшие звонки: %+v", stale)
	}
	deleted, err := s.DeleteStaleCalls(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("DeleteStaleCalls: %v", err)
	}
	if deleted != 1 {
		t.Errorf("удалено звонков = %d, ожидалось 1", deleted)
	}
	if _, err := s.GetCall(ctx, "fresh"); err != nil {
		t.Errorf("свежий звонок удалён: %v", err)
	}

	// Дедупликация.
	if _, err := s.ClaimDedup(ctx, "d-stale", "peer"); err != nil {
		t.Fatalf("ClaimDedup: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE dedup SET created_at = ? WHERE uniqueid = ?`, formatTime(old), "d-stale"); err != nil {
		t.Fatalf("старение дедупликации: %v", err)
	}
	n, err := s.DeleteStaleDedup(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("DeleteStaleDedup: %v", err)
	}
	if n != 1 {
		t.Errorf("удалено записей дедупликации = %d, ожидалось 1", n)
	}
}

func TestIncrementalVacuum(t *testing.T) {
	s := openTestStore(t)
	if err := s.IncrementalVacuum(context.Background()); err != nil {
		t.Fatalf("IncrementalVacuum: %v", err)
	}
}
