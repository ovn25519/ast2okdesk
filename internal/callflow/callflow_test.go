package callflow

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ovn25519/ast2okdesk/internal/ami"
	"github.com/ovn25519/ast2okdesk/internal/store"
)

func silentLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeStore — in-memory реализация Store для сценарных тестов.
type fakeStore struct {
	mu      sync.Mutex
	calls   map[string]store.Call
	dedup   map[string]bool
	saveErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{calls: make(map[string]store.Call), dedup: make(map[string]bool)}
}

func dedupKey(uniqueid, peer string) string { return uniqueid + "\x00" + peer }

func (s *fakeStore) SaveCallQueued(_ context.Context, call store.Call) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	s.calls[call.Uniqueid] = call
	return nil
}

func (s *fakeStore) ClaimDedup(_ context.Context, uniqueid, peer string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := dedupKey(uniqueid, peer)
	if s.dedup[k] {
		return false, nil
	}
	s.dedup[k] = true
	return true, nil
}

func (s *fakeStore) MarkAnswered(_ context.Context, uniqueid, peer string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.calls[uniqueid]
	if !ok {
		return store.ErrNotFound
	}
	c.Status = store.StatusAnswered
	c.AgentPeer = peer
	c.AnsweredAt = &at
	c.UpdatedAt = at
	s.calls[uniqueid] = c
	return nil
}

func (s *fakeStore) MarkAbandoned(_ context.Context, uniqueid string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.calls[uniqueid]
	if !ok {
		return store.ErrNotFound
	}
	c.Status = store.StatusAbandoned
	c.UpdatedAt = at
	s.calls[uniqueid] = c
	return nil
}

func (s *fakeStore) GetCall(_ context.Context, uniqueid string) (store.Call, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.calls[uniqueid]
	if !ok {
		return store.Call{}, store.ErrNotFound
	}
	return c, nil
}

// fakePop — мок screen-pop.
type fakePop struct {
	mu    sync.Mutex
	calls []popCall
	err   error
}

type popCall struct {
	phone  string
	number int
}

func (p *fakePop) ScreenPop(_ context.Context, phone string, telephonyNumber int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, popCall{phone: phone, number: telephonyNumber})
	return p.err
}

func (p *fakePop) snapshot() []popCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]popCall(nil), p.calls...)
}

func ev(name string, kv ...string) ami.Frame {
	m := map[string]string{"Event": name}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return ami.NewFrame(m)
}

var fixedNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func newDispatcher(st Store, pop ScreenPopper, cfg Config) *Dispatcher {
	d := New(st, pop, cfg, silentLogger())
	d.now = func() time.Time { return fixedNow }
	return d
}

func testConfig(t *testing.T, employees map[string]int, fallback int) Config {
	t.Helper()
	return Config{Queue: "support", TelephonyNumber: fallback, Employees: employees}
}

func TestQueueCallerJoinSavesCall(t *testing.T) {
	st := newFakeStore()
	d := newDispatcher(st, &fakePop{}, testConfig(t, nil, 327))

	err := d.Handle(context.Background(), ev("QueueCallerJoin",
		"Queue", "support", "Uniqueid", "u1", "Linkedid", "l1",
		"CallerIDNum", "79990001122"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	call, err := st.GetCall(context.Background(), "u1")
	if err != nil {
		t.Fatalf("звонок не сохранён: %v", err)
	}
	if call.Status != store.StatusQueued || call.Linkedid != "l1" ||
		call.CallerIDNum != "79990001122" || call.Queue != "support" {
		t.Errorf("сохранённый звонок = %+v", call)
	}
	if !call.CreatedAt.Equal(fixedNow) {
		t.Errorf("CreatedAt = %v, ожидалось %v", call.CreatedAt, fixedNow)
	}
}

func TestQueueCallerJoinLinkedidFallback(t *testing.T) {
	st := newFakeStore()
	d := newDispatcher(st, &fakePop{}, testConfig(t, nil, 327))

	if err := d.Handle(context.Background(), ev("QueueCallerJoin",
		"Queue", "support", "Uniqueid", "u1")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	call, _ := st.GetCall(context.Background(), "u1")
	if call.Linkedid != "u1" {
		t.Errorf("Linkedid = %q, ожидалось u1", call.Linkedid)
	}
}

func TestForeignQueueIgnored(t *testing.T) {
	st := newFakeStore()
	pop := &fakePop{}
	d := newDispatcher(st, pop, testConfig(t, map[string]int{"101": 327}, 0))

	for _, name := range []string{"QueueCallerJoin", "AgentCalled", "AgentConnect", "QueueCallerAbandon"} {
		if err := d.Handle(context.Background(), ev(name,
			"Queue", "other", "Uniqueid", "u1", "DestChannel", "SIP/101-0000000a")); err != nil {
			t.Fatalf("Handle(%s): %v", name, err)
		}
	}
	d.Wait()

	if len(st.calls) != 0 {
		t.Errorf("сохранены звонки чужой очереди: %v", st.calls)
	}
	if got := pop.snapshot(); len(got) != 0 {
		t.Errorf("screen-pop для чужой очереди: %v", got)
	}
}

func TestAgentCalledScreenPop(t *testing.T) {
	st := newFakeStore()
	pop := &fakePop{}
	d := newDispatcher(st, pop, testConfig(t, map[string]int{"101": 327}, 0))

	err := d.Handle(context.Background(), ev("AgentCalled",
		"Queue", "support", "Uniqueid", "u1",
		"DestChannel", "SIP/101-0000000a", "CallerIDNum", "79990001122"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	d.Wait()

	got := pop.snapshot()
	if len(got) != 1 {
		t.Fatalf("screen-pop вызван %d раз, ожидался 1: %v", len(got), got)
	}
	if got[0].phone != "79990001122" || got[0].number != 327 {
		t.Errorf("screen-pop = %+v, ожидалось {79990001122 327}", got[0])
	}
}

func TestAgentCalledDedup(t *testing.T) {
	st := newFakeStore()
	pop := &fakePop{}
	d := newDispatcher(st, pop, testConfig(t, map[string]int{"101": 327}, 0))

	f := ev("AgentCalled", "Queue", "support", "Uniqueid", "u1",
		"DestChannel", "SIP/101-0000000a", "CallerIDNum", "79990001122")
	for i := 0; i < 2; i++ {
		if err := d.Handle(context.Background(), f); err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}
	d.Wait()

	if got := pop.snapshot(); len(got) != 1 {
		t.Errorf("screen-pop вызван %d раз, ожидался 1: %v", len(got), got)
	}
}

func TestAgentCalledRingall(t *testing.T) {
	st := newFakeStore()
	pop := &fakePop{}
	d := newDispatcher(st, pop, testConfig(t, map[string]int{"101": 327, "102": 328}, 0))

	base := []string{"Queue", "support", "Uniqueid", "u1", "CallerIDNum", "79990001122"}
	if err := d.Handle(context.Background(), ev("AgentCalled",
		append(append([]string{}, base...), "DestChannel", "SIP/101-0000000a")...)); err != nil {
		t.Fatalf("Handle(101): %v", err)
	}
	if err := d.Handle(context.Background(), ev("AgentCalled",
		append(append([]string{}, base...), "DestChannel", "SIP/102-0000000b")...)); err != nil {
		t.Fatalf("Handle(102): %v", err)
	}
	d.Wait()

	got := pop.snapshot()
	if len(got) != 2 {
		t.Fatalf("screen-pop вызван %d раз, ожидалось 2: %v", len(got), got)
	}
	// Порядок асинхронных screen-pop не гарантирован: проверяем множество номеров.
	numbers := map[int]bool{}
	for _, c := range got {
		numbers[c.number] = true
	}
	if !numbers[327] || !numbers[328] {
		t.Errorf("номера screen-pop = %+v, ожидались 327 и 328", got)
	}
}

func TestAgentCalledFallbackNumber(t *testing.T) {
	st := newFakeStore()
	pop := &fakePop{}
	d := newDispatcher(st, pop, testConfig(t, nil, 327))

	if err := d.Handle(context.Background(), ev("AgentCalled",
		"Queue", "support", "Uniqueid", "u1",
		"DestChannel", "SIP/ivan-0000000a", "CallerIDNum", "79990001122")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	d.Wait()

	got := pop.snapshot()
	if len(got) != 1 || got[0].number != 327 {
		t.Errorf("screen-pop = %+v, ожидался fallback-номер 327", got)
	}
}

func TestAgentCalledNoNumberSkips(t *testing.T) {
	st := newFakeStore()
	pop := &fakePop{}
	d := newDispatcher(st, pop, testConfig(t, nil, 0))

	if err := d.Handle(context.Background(), ev("AgentCalled",
		"Queue", "support", "Uniqueid", "u1",
		"DestChannel", "SIP/ivan-0000000a", "CallerIDNum", "79990001122")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	d.Wait()

	if got := pop.snapshot(); len(got) != 0 {
		t.Errorf("screen-pop не должен вызываться: %v", got)
	}
}

func TestAgentCalledNonSIPChannelSkips(t *testing.T) {
	st := newFakeStore()
	pop := &fakePop{}
	d := newDispatcher(st, pop, testConfig(t, map[string]int{"101": 327}, 0))

	for _, ch := range []string{"Local/101@from-queue-0000;1", "", "SIP/101"} {
		if err := d.Handle(context.Background(), ev("AgentCalled",
			"Queue", "support", "Uniqueid", "u1", "DestChannel", ch, "CallerIDNum", "79990001122")); err != nil {
			t.Fatalf("Handle(%q): %v", ch, err)
		}
	}
	d.Wait()

	if got := pop.snapshot(); len(got) != 0 {
		t.Errorf("screen-pop не должен вызываться для %v", got)
	}
}

func TestAgentCalledPeerNumber(t *testing.T) {
	st := newFakeStore()
	pop := &fakePop{}
	// Ни таблицы [[employees]], ни запасного номера: номер берётся из имени peer.
	d := newDispatcher(st, pop, testConfig(t, nil, 0))

	if err := d.Handle(context.Background(), ev("AgentCalled",
		"Queue", "support", "Uniqueid", "u1",
		"DestChannel", "SIP/327-0000000a", "CallerIDNum", "79990001122")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	d.Wait()

	got := pop.snapshot()
	if len(got) != 1 || got[0].number != 327 {
		t.Errorf("screen-pop = %+v, ожидался номер 327 из имени peer", got)
	}
}

func TestAgentCalledPJSIP(t *testing.T) {
	st := newFakeStore()
	pop := &fakePop{}
	d := newDispatcher(st, pop, testConfig(t, nil, 0))

	if err := d.Handle(context.Background(), ev("AgentCalled",
		"Queue", "support", "Uniqueid", "u1",
		"DestChannel", "PJSIP/327-0000000a", "CallerIDNum", "79990001122")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	d.Wait()

	got := pop.snapshot()
	if len(got) != 1 || got[0].number != 327 {
		t.Errorf("screen-pop = %+v, ожидался номер 327 для канала PJSIP", got)
	}
}

func TestAgentCalledEmployeesOverridePeer(t *testing.T) {
	st := newFakeStore()
	pop := &fakePop{}
	// Явное переопределение имеет приоритет над цифровым именем peer.
	d := newDispatcher(st, pop, testConfig(t, map[string]int{"327": 456}, 0))

	if err := d.Handle(context.Background(), ev("AgentCalled",
		"Queue", "support", "Uniqueid", "u1",
		"DestChannel", "SIP/327-0000000a", "CallerIDNum", "79990001122")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	d.Wait()

	got := pop.snapshot()
	if len(got) != 1 || got[0].number != 456 {
		t.Errorf("screen-pop = %+v, ожидался номер 456 из [[employees]]", got)
	}
}

func TestScreenPopErrorDoesNotPropagate(t *testing.T) {
	st := newFakeStore()
	pop := &fakePop{err: errors.New("Okdesk недоступен")}
	d := newDispatcher(st, pop, testConfig(t, map[string]int{"101": 327}, 0))

	f := ev("AgentCalled", "Queue", "support", "Uniqueid", "u1",
		"DestChannel", "SIP/101-0000000a", "CallerIDNum", "79990001122")
	if err := d.Handle(context.Background(), f); err != nil {
		t.Fatalf("Handle вернул ошибку: %v", err)
	}
	if err := d.Handle(context.Background(), f); err != nil {
		t.Fatalf("повторный Handle вернул ошибку: %v", err)
	}
	d.Wait()

	if got := pop.snapshot(); len(got) != 1 {
		t.Errorf("screen-pop вызван %d раз, ожидался 1 (без повторов): %v", len(got), got)
	}
}

func TestAgentConnectMarksAnswered(t *testing.T) {
	st := newFakeStore()
	d := newDispatcher(st, &fakePop{}, testConfig(t, map[string]int{"101": 327}, 0))

	if err := d.Handle(context.Background(), ev("QueueCallerJoin",
		"Queue", "support", "Uniqueid", "u1", "CallerIDNum", "7999")); err != nil {
		t.Fatalf("Join: %v", err)
	}
	if err := d.Handle(context.Background(), ev("AgentConnect",
		"Queue", "support", "Uniqueid", "u1", "DestChannel", "SIP/101-0000000a")); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	call, _ := st.GetCall(context.Background(), "u1")
	if call.Status != store.StatusAnswered || call.AgentPeer != "101" || call.AnsweredAt == nil {
		t.Errorf("звонок после ответа = %+v", call)
	}
}

func TestAgentConnectUnknownCallNoError(t *testing.T) {
	d := newDispatcher(newFakeStore(), &fakePop{}, testConfig(t, map[string]int{"101": 327}, 0))
	if err := d.Handle(context.Background(), ev("AgentConnect",
		"Queue", "support", "Uniqueid", "нет", "DestChannel", "SIP/101-0000000a")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

func TestQueueCallerAbandonMarksAbandoned(t *testing.T) {
	st := newFakeStore()
	d := newDispatcher(st, &fakePop{}, testConfig(t, nil, 327))

	if err := d.Handle(context.Background(), ev("QueueCallerJoin",
		"Queue", "support", "Uniqueid", "u1")); err != nil {
		t.Fatalf("Join: %v", err)
	}
	if err := d.Handle(context.Background(), ev("QueueCallerAbandon",
		"Queue", "support", "Uniqueid", "u1")); err != nil {
		t.Fatalf("Abandon: %v", err)
	}

	call, _ := st.GetCall(context.Background(), "u1")
	if call.Status != store.StatusAbandoned {
		t.Errorf("статус = %q, ожидался abandoned", call.Status)
	}
}

func TestHandleUnknownEventIgnored(t *testing.T) {
	d := newDispatcher(newFakeStore(), &fakePop{}, testConfig(t, nil, 327))
	if err := d.Handle(context.Background(), ev("Newchannel", "Uniqueid", "u1")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

func TestParsePeer(t *testing.T) {
	cases := []struct {
		in    string
		peer  string
		valid bool
	}{
		{"SIP/101-0000000a", "101", true},
		{"SIP/ivan_1-0000000a", "ivan_1", true},
		{"PJSIP/101-0000000a", "101", true},
		{"PJSIP/ivan.petrov-0000000a", "ivan.petrov", true},
		{"SIP/sip-327-0000001a", "sip-327", true},
		{"SIP/101", "", false},
		{"Local/101@from-queue-0000;1", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		peer, ok := ParsePeer(c.in)
		if ok != c.valid || peer != c.peer {
			t.Errorf("ParsePeer(%q) = (%q, %v), ожидалось (%q, %v)", c.in, peer, ok, c.peer, c.valid)
		}
	}
}

func TestRunLoopProcessesEvents(t *testing.T) {
	st := newFakeStore()
	d := newDispatcher(st, &fakePop{}, testConfig(t, nil, 327))

	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan ami.Frame, 4)
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, events) }()

	events <- ev("QueueCallerJoin", "Queue", "support", "Uniqueid", "u1")

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := st.GetCall(context.Background(), "u1"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("событие не обработано циклом Run")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run вернул ошибку: %v", err)
	}
}

func TestRunLoopClosesOnChannelClose(t *testing.T) {
	d := newDispatcher(newFakeStore(), &fakePop{}, testConfig(t, nil, 327))
	events := make(chan ami.Frame)
	done := make(chan error, 1)
	go func() { done <- d.Run(context.Background(), events) }()

	close(events)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run вернул ошибку: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run не завершился после закрытия канала")
	}
}
