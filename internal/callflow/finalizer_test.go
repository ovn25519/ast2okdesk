package callflow

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/ovn25519/ast2okdesk/internal/ami"
)

// fakeFinalizer фиксирует делегированные события завершения звонка.
type fakeFinalizer struct {
	mu   sync.Mutex
	hang []string
	err  error
}

func (f *fakeFinalizer) OnHangup(_ context.Context, fr ami.Frame) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hang = append(f.hang, fr.Get("Uniqueid"))
	return f.err
}

func (f *fakeFinalizer) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hang...)
}

func TestFinalizerHangupDelegation(t *testing.T) {
	d := newDispatcher(newFakeStore(), &fakePop{}, testConfig(t, nil, 327))
	ff := &fakeFinalizer{}
	d.SetFinalizer(ff)

	if err := d.Handle(context.Background(), ev("Hangup", "Uniqueid", "u1")); err != nil {
		t.Fatalf("Handle(Hangup): %v", err)
	}
	// Событие Cdr больше не требуется и должно просто игнорироваться.
	if err := d.Handle(context.Background(), ev("Cdr", "UniqueID", "u1")); err != nil {
		t.Fatalf("Handle(Cdr): %v", err)
	}

	hang := ff.snapshot()
	if len(hang) != 1 || hang[0] != "u1" {
		t.Errorf("OnHangup вызван с %v, ожидалось [u1]", hang)
	}
}

func TestFinalizerNilSkipped(t *testing.T) {
	d := newDispatcher(newFakeStore(), &fakePop{}, testConfig(t, nil, 327))
	if err := d.Handle(context.Background(), ev("Hangup", "Uniqueid", "u1")); err != nil {
		t.Fatalf("Handle(Hangup) без финализатора: %v", err)
	}
	if err := d.Handle(context.Background(), ev("Cdr", "UniqueID", "u1")); err != nil {
		t.Fatalf("Handle(Cdr) без финализатора: %v", err)
	}
}

func TestFinalizerHangupError(t *testing.T) {
	d := newDispatcher(newFakeStore(), &fakePop{}, testConfig(t, nil, 327))
	d.SetFinalizer(&fakeFinalizer{err: errors.New("сбой")})
	if err := d.Handle(context.Background(), ev("Hangup", "Uniqueid", "u1")); err == nil {
		t.Fatal("ожидалась ошибка от финализатора")
	}
}
