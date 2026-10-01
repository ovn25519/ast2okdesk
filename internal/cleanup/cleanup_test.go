package cleanup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type fakeStore struct {
	mu     sync.Mutex
	before []time.Time
	calls  int64
	dedup  int64
	err    error
	vacuum int
}

func (s *fakeStore) DeleteStaleCalls(_ context.Context, before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.before = append(s.before, before)
	if s.err != nil {
		return 0, s.err
	}
	return s.calls, nil
}

func (s *fakeStore) DeleteStaleDedup(_ context.Context, before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dedup, nil
}

func (s *fakeStore) IncrementalVacuum(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vacuum++
	return nil
}

func silent() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestOnceDeletesAndVacuums(t *testing.T) {
	st := &fakeStore{calls: 2, dedup: 3}
	c := New(st, Config{TTL: 24 * time.Hour}, silent())
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	res, err := c.Once(context.Background())
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if res.Calls != 2 || res.Dedup != 3 {
		t.Errorf("Result = %+v, ожидалось {2 3}", res)
	}
	if len(st.before) != 1 {
		t.Fatalf("вызовов DeleteStaleCalls = %d, ожидался 1", len(st.before))
	}
	if want := now.Add(-24 * time.Hour); !st.before[0].Equal(want) {
		t.Errorf("before = %v, ожидалось %v", st.before[0], want)
	}
	if st.vacuum != 1 {
		t.Errorf("IncrementalVacuum вызван %d раз, ожидался 1", st.vacuum)
	}
}

func TestOnceZeroTTLIsNoop(t *testing.T) {
	st := &fakeStore{calls: 5, dedup: 5}
	c := New(st, Config{TTL: 0}, silent())
	res, err := c.Once(context.Background())
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if res.Calls != 0 || res.Dedup != 0 {
		t.Errorf("Result = %+v, ожидался нулевой", res)
	}
	if len(st.before) != 0 || st.vacuum != 0 {
		t.Errorf("хранилище не должно трогаться: before=%v vacuum=%d", st.before, st.vacuum)
	}
}

func TestOnceStoreError(t *testing.T) {
	st := &fakeStore{err: errors.New("бд")}
	c := New(st, Config{TTL: time.Hour}, silent())
	if _, err := c.Once(context.Background()); err == nil {
		t.Fatal("ожидалась ошибка хранилища")
	}
}

func TestRunDisabledWithoutInterval(t *testing.T) {
	c := New(&fakeStore{}, Config{TTL: time.Hour, Interval: 0}, silent())
	if err := c.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRunPeriodic(t *testing.T) {
	st := &fakeStore{calls: 1}
	c := New(st, Config{TTL: time.Hour, Interval: 20 * time.Millisecond}, silent())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		st.mu.Lock()
		v := st.vacuum
		st.mu.Unlock()
		if v > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("уборка не запускалась по таймеру")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}
