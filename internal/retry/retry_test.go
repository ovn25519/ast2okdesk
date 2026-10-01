package retry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ovn25519/ast2okdesk/internal/store"
)

// --- Подделки -----------------------------------------------------------------

type reschedule struct {
	id      int64
	nextAt  time.Time
	lastErr string
}

type fakeStore struct {
	mu          sync.Mutex
	due         []store.RetryItem
	reschedules []reschedule
	deleted     []int64
	dueErr      error
}

func (s *fakeStore) DueRetries(_ context.Context, _ time.Time, _ int) ([]store.RetryItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dueErr != nil {
		return nil, s.dueErr
	}
	return append([]store.RetryItem(nil), s.due...), nil
}

func (s *fakeStore) RescheduleRetry(_ context.Context, id int64, next time.Time, lastErr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reschedules = append(s.reschedules, reschedule{id: id, nextAt: next, lastErr: lastErr})
	return nil
}

func (s *fakeStore) DeleteRetry(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, id)
	return nil
}

type fakeSender struct {
	mu    sync.Mutex
	count int
	err   error
}

func (s *fakeSender) SendPhoneCall(_ context.Context, _ []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.count++
	return nil
}

func (s *fakeSender) sent() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

var retryNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func newWorker(st Store, snd Sender, cfg Config) (*Worker, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	w := New(st, snd, cfg, slog.New(slog.NewTextHandler(buf, nil)))
	w.now = func() time.Time { return retryNow }
	return w, buf
}

func item(id int64, attempts int) store.RetryItem {
	return store.RetryItem{
		ID:            id,
		CallID:        "u1",
		Payload:       `{"call_id":"u1"}`,
		Attempts:      attempts,
		NextAttemptAt: retryNow,
	}
}

// --- Тесты --------------------------------------------------------------------

func TestProcessDueSuccess(t *testing.T) {
	st := &fakeStore{due: []store.RetryItem{item(1, 0)}}
	snd := &fakeSender{}
	w, _ := newWorker(st, snd, Config{MaxAttempts: 8, InitialBackoff: 5 * time.Second, MaxBackoff: time.Hour})

	if err := w.ProcessDue(context.Background()); err != nil {
		t.Fatalf("ProcessDue: %v", err)
	}
	if snd.sent() != 1 {
		t.Errorf("отправок = %d, ожидалась 1", snd.sent())
	}
	if len(st.deleted) != 1 || st.deleted[0] != 1 {
		t.Errorf("удалено = %v, ожидалось [1]", st.deleted)
	}
	if len(st.reschedules) != 0 {
		t.Errorf("не ожидалось перепланирований: %v", st.reschedules)
	}
}

func TestProcessDueRescheduleOnFailure(t *testing.T) {
	st := &fakeStore{due: []store.RetryItem{item(7, 0)}}
	snd := &fakeSender{err: errors.New("нет связи")}
	w, _ := newWorker(st, snd, Config{MaxAttempts: 8, InitialBackoff: 5 * time.Second, MaxBackoff: time.Hour})

	if err := w.ProcessDue(context.Background()); err != nil {
		t.Fatalf("ProcessDue: %v", err)
	}
	if len(st.deleted) != 0 {
		t.Errorf("запись не должна удаляться: %v", st.deleted)
	}
	if len(st.reschedules) != 1 {
		t.Fatalf("перепланирований = %d, ожидалось 1", len(st.reschedules))
	}
	got := st.reschedules[0]
	if got.id != 7 || got.lastErr == "" {
		t.Errorf("перепланирование = %+v", got)
	}
	if want := retryNow.Add(5 * time.Second); !got.nextAt.Equal(want) {
		t.Errorf("next_attempt_at = %v, ожидалось %v", got.nextAt, want)
	}
}

func TestProcessDueExhaustedAlertsAndDeletes(t *testing.T) {
	st := &fakeStore{due: []store.RetryItem{item(3, 7)}}
	snd := &fakeSender{err: errors.New("нет связи")}
	w, buf := newWorker(st, snd, Config{MaxAttempts: 8, InitialBackoff: 5 * time.Second, MaxBackoff: time.Hour})

	if err := w.ProcessDue(context.Background()); err != nil {
		t.Fatalf("ProcessDue: %v", err)
	}
	if len(st.deleted) != 1 || st.deleted[0] != 3 {
		t.Errorf("удалено = %v, ожидалось [3]", st.deleted)
	}
	if len(st.reschedules) != 0 {
		t.Errorf("после исчерпания не должно быть перепланирований: %v", st.reschedules)
	}
	if !strings.Contains(buf.String(), "ALERT") {
		t.Errorf("в журнале нет ALERT: %s", buf.String())
	}
}

func TestProcessDueAlreadyExhausted(t *testing.T) {
	st := &fakeStore{due: []store.RetryItem{item(9, 8)}}
	snd := &fakeSender{}
	w, buf := newWorker(st, snd, Config{MaxAttempts: 8, InitialBackoff: 5 * time.Second, MaxBackoff: time.Hour})

	if err := w.ProcessDue(context.Background()); err != nil {
		t.Fatalf("ProcessDue: %v", err)
	}
	if snd.sent() != 0 {
		t.Errorf("повторная отправка не выполнялась, а отправок = %d", snd.sent())
	}
	if len(st.deleted) != 1 || st.deleted[0] != 9 {
		t.Errorf("удалено = %v, ожидалось [9]", st.deleted)
	}
	if !strings.Contains(buf.String(), "ALERT") {
		t.Errorf("в журнале нет ALERT: %s", buf.String())
	}
}

func TestProcessDueStoreError(t *testing.T) {
	st := &fakeStore{dueErr: errors.New("бд")}
	w, _ := newWorker(st, &fakeSender{}, Config{})
	if err := w.ProcessDue(context.Background()); err == nil {
		t.Fatal("ожидалась ошибка чтения очереди")
	}
}

func TestBackoffCapped(t *testing.T) {
	w, _ := newWorker(&fakeStore{}, &fakeSender{}, Config{
		InitialBackoff: 5 * time.Second,
		MaxBackoff:     time.Hour,
	})
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{100, time.Hour},
		{0, 5 * time.Second},
	}
	for _, c := range cases {
		if got := w.backoff(c.attempt); got != c.want {
			t.Errorf("backoff(%d) = %v, ожидалось %v", c.attempt, got, c.want)
		}
	}
}

func TestRunLiftsQueueOnStart(t *testing.T) {
	st := &fakeStore{due: []store.RetryItem{item(1, 0)}}
	snd := &fakeSender{}
	w, _ := newWorker(st, snd, Config{PollInterval: 20 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		st.mu.Lock()
		n := len(st.deleted)
		st.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("очередь не поднята при старте")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run вернул ошибку: %v", err)
	}
	if snd.sent() != 1 {
		t.Errorf("отправок = %d, ожидалась 1", snd.sent())
	}
}
