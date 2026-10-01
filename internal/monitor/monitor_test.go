package monitor

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ovn25519/ast2okdesk/internal/ami"
)

// syncBuffer — потокобезопасный буфер журнала: тест читает его параллельно с
// записью из горутины монитора.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type fakeAMI struct{ stats ami.Stats }

func (f fakeAMI) Stats() ami.Stats { return f.stats }

type fakeRetry struct {
	depth int
	err   error
}

func (f fakeRetry) RetryDepth(context.Context) (int, error) { return f.depth, f.err }

type fakeCDR struct{ n int64 }

func (f fakeCDR) CDRCount() int64 { return f.n }

func newMonitor(cfg Config, counters *Counters, a AMISource, r RetrySource, c CDRSource) (*Monitor, *syncBuffer) {
	buf := &syncBuffer{}
	m := New(cfg, counters, a, r, c, slog.New(slog.NewTextHandler(buf, nil)))
	return m, buf
}

func TestCountersObserve(t *testing.T) {
	c := &Counters{}
	c.ObserveAPI("/a", nil)
	c.ObserveAPI("/b", errors.New("сбой"))
	s := c.Snapshot()
	if s.APITotal != 2 || s.APIFailed != 1 {
		t.Errorf("Snapshot = %+v, ожидалось {2 1}", s)
	}
}

func TestReportAllSources(t *testing.T) {
	dir := t.TempDir()
	counters := &Counters{}
	counters.ObserveAPI("/a", nil)
	counters.ObserveAPI("/b", errors.New("x"))

	m, buf := newMonitor(Config{FilesDir: dir}, counters,
		fakeAMI{stats: ami.Stats{Connected: true, Reconnects: 3, FramesReceived: 42, LastEventAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}},
		fakeRetry{depth: 5}, fakeCDR{n: 9})

	m.Report(context.Background())
	out := buf.String()
	for _, want := range []string{"мониторинг", "api_total=2", "api_failed=1", "cdr_received=9",
		"retry_depth=5", "ami_connected=true", "ami_reconnects=3", "ami_frames=42",
		"files_dir_ok=true"} {
		if !strings.Contains(out, want) {
			t.Errorf("в журнале нет %q: %s", want, out)
		}
	}
}

func TestReportRetryError(t *testing.T) {
	m, buf := newMonitor(Config{}, &Counters{}, nil, fakeRetry{err: errors.New("бд")}, nil)
	m.Report(context.Background())
	if !strings.Contains(buf.String(), "retry_depth_error=бд") {
		t.Errorf("ошибка глубины retry не залогирована: %s", buf.String())
	}
}

func TestReportMissingFilesDir(t *testing.T) {
	m, buf := newMonitor(Config{FilesDir: "/нет/такого/каталога"}, &Counters{}, nil, nil, nil)
	m.Report(context.Background())
	out := buf.String()
	if !strings.Contains(out, "файл-сервер записей недоступен") || !strings.Contains(out, "files_dir_ok=false") {
		t.Errorf("отсутствие каталога не залогировано: %s", out)
	}
}

func TestRunPeriodic(t *testing.T) {
	m, buf := newMonitor(Config{Interval: 20 * time.Millisecond}, &Counters{}, nil, nil, fakeCDR{n: 1})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if strings.Contains(buf.String(), "мониторинг") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("периодический дамп не выполнен")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}
