package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ovn25519/ast2okdesk/internal/ami"
	"github.com/ovn25519/ast2okdesk/internal/okdesk"
	"github.com/ovn25519/ast2okdesk/internal/recording"
	"github.com/ovn25519/ast2okdesk/internal/store"
)

func silent() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// --- Поддельное хранилище ------------------------------------------------------

type fakeStore struct {
	mu           sync.Mutex
	calls        map[string]store.Call
	finished     map[string]time.Time
	deletedCalls []string
	deletedDedup []string
	retries      []store.NewRetryItem
	enqueueErr   error
}

func newFakeStore() *fakeStore {
	return &fakeStore{calls: map[string]store.Call{}, finished: map[string]time.Time{}}
}

func (s *fakeStore) put(c store.Call) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[c.Uniqueid] = c
}

func (s *fakeStore) GetCall(_ context.Context, uid string) (store.Call, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.calls[uid]
	if !ok {
		return store.Call{}, store.ErrNotFound
	}
	return c, nil
}

func (s *fakeStore) MarkFinished(_ context.Context, uid string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.calls[uid]
	if !ok {
		return store.ErrNotFound
	}
	c.FinishedAt = &at
	s.calls[uid] = c
	s.finished[uid] = at
	return nil
}

func (s *fakeStore) DeleteCall(_ context.Context, uid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.calls, uid)
	s.deletedCalls = append(s.deletedCalls, uid)
	return nil
}

func (s *fakeStore) DeleteDedupByCall(_ context.Context, uid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletedDedup = append(s.deletedDedup, uid)
	return nil
}

func (s *fakeStore) EnqueueRetry(_ context.Context, it store.NewRetryItem) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enqueueErr != nil {
		return false, s.enqueueErr
	}
	s.retries = append(s.retries, it)
	return true, nil
}

func (s *fakeStore) containsDeleted(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.deletedCalls {
		if d == uid {
			return true
		}
	}
	return false
}

// --- Поддельный клиент Okdesk --------------------------------------------------

type fakeClient struct {
	mu         sync.Mutex
	built      []okdesk.PhoneCall
	sent       int
	sendErr    error
	issueID    int
	issueOK    bool
	issueErr   error
	findPhones []string
}

func (c *fakeClient) BuildPhoneCall(pc okdesk.PhoneCall) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.built = append(c.built, pc)
	return json.Marshal(pc)
}

func (c *fakeClient) SendPhoneCall(_ context.Context, _ []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sendErr != nil {
		return c.sendErr
	}
	c.sent++
	return nil
}

func (c *fakeClient) FindIssueID(_ context.Context, phone string) (int, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.findPhones = append(c.findPhones, phone)
	return c.issueID, c.issueOK, c.issueErr
}

func (c *fakeClient) lastBuilt() (okdesk.PhoneCall, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.built) == 0 {
		return okdesk.PhoneCall{}, false
	}
	return c.built[len(c.built)-1], true
}

func (c *fakeClient) sendCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sent
}

func (c *fakeClient) findCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.findPhones)
}

// --- Вспомогательные -----------------------------------------------------------

func newFinalizer(st Store, cl CallClient, cfg Config) *Finalizer {
	// В большинстве тестов проверяется включённая автопривязка; отключение
	// проверяется отдельным тестом через New с AutoLinkIssue=false.
	cfg.AutoLinkIssue = true
	return New(st, cl, recording.New("https://rec/", time.UTC), cfg, silent())
}

// fixedNow возвращает функцию времени с фиксированным отбоем.
func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

func hangupFrame(uid string) ami.Frame {
	return ami.NewFrame(map[string]string{"Event": "Hangup", "Uniqueid": uid})
}

var (
	t0       = time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	tAnswer  = time.Date(2026, 9, 30, 9, 16, 0, 0, time.UTC)
	tStart   = time.Date(2026, 9, 30, 9, 15, 0, 0, time.UTC)
	tEnd     = time.Date(2026, 9, 30, 9, 17, 0, 0, time.UTC)
	baseCall = store.Call{
		Uniqueid:    "u1",
		CallerIDNum: "79990001122",
		Status:      store.StatusAnswered,
		AgentPeer:   "327",
		CreatedAt:   t0,
		AnsweredAt:  &tAnswer,
	}
)

func TestHangupJournalsCall(t *testing.T) {
	st := newFakeStore()
	st.put(baseCall)
	cl := &fakeClient{issueID: 42, issueOK: true}
	fin := newFinalizer(st, cl, Config{
		TelephonyNumber:     327,
		RetryInitialBackoff: time.Second,
	})
	fin.now = fixedNow(tEnd)

	if err := fin.OnHangup(context.Background(), hangupFrame("u1")); err != nil {
		t.Fatalf("OnHangup: %v", err)
	}
	fin.Wait()

	pc, ok := cl.lastBuilt()
	if !ok {
		t.Fatal("phone_call не собран")
	}
	if pc.CallID != "u1" || pc.SourcePhone != "79990001122" || pc.ReceiverPhone != "327" {
		t.Errorf("phone_call = %+v", pc)
	}
	if !pc.StartedAt.Equal(tAnswer) || !pc.FinishedAt.Equal(tEnd) {
		t.Errorf("времена = %v..%v, ожидались %v..%v", pc.StartedAt, pc.FinishedAt, tAnswer, tEnd)
	}
	if pc.Duration != 60 {
		t.Errorf("duration = %d, ожидалось 60 (ответ→отбой)", pc.Duration)
	}
	if pc.Direction != okdesk.DirectionIncoming {
		t.Errorf("direction = %d, ожидался 0", pc.Direction)
	}
	// Uniqueid «u1» не содержит метки времени — минута берётся из CreatedAt (09:00).
	wantURL := "https://rec/u1-2026-09-30-09_00-79990001122-s.mp3"
	if pc.FileURL != wantURL {
		t.Errorf("file_url = %q, ожидался %q", pc.FileURL, wantURL)
	}
	if pc.IssueID == nil || *pc.IssueID != 42 {
		t.Errorf("issue_id = %v, ожидался 42", pc.IssueID)
	}
	if cl.sendCount() != 1 {
		t.Errorf("отправок = %d, ожидалась 1", cl.sendCount())
	}
	if !st.containsDeleted("u1") {
		t.Error("корреляция звонка не удалена после журналирования")
	}
	if len(st.deletedDedup) != 1 || st.deletedDedup[0] != "u1" {
		t.Errorf("дедупликация не удалена: %v", st.deletedDedup)
	}
	if fin.CallsLogged() != 1 {
		t.Errorf("CallsLogged = %d, ожидалось 1", fin.CallsLogged())
	}
}

// TestTimingRoundsToSecond проверяет, что началом разговора берётся время ответа
// оператора (AMI AgentConnect) и длительность округляется до секунды.
func TestTimingRoundsToSecond(t *testing.T) {
	st := newFakeStore()
	answered := tAnswer.Add(400 * time.Millisecond) // 09:16:00.4
	st.put(store.Call{
		Uniqueid:    "u4",
		CallerIDNum: "79990001122",
		Status:      store.StatusAnswered,
		AgentPeer:   "327",
		CreatedAt:   t0,
		AnsweredAt:  &answered,
	})
	cl := &fakeClient{}
	fin := newFinalizer(st, cl, Config{TelephonyNumber: 327})
	fin.now = fixedNow(tEnd)

	if err := fin.OnHangup(context.Background(), hangupFrame("u4")); err != nil {
		t.Fatalf("OnHangup: %v", err)
	}
	fin.Wait()

	pc, ok := cl.lastBuilt()
	if !ok {
		t.Fatal("phone_call не собран")
	}
	if !pc.StartedAt.Equal(answered) {
		t.Errorf("started_at = %v, ожидалось время AgentConnect %v", pc.StartedAt, answered)
	}
	// 09:17:00.0 - 09:16:00.4 = 59.6 с → округление до 60.
	if pc.Duration != 60 {
		t.Errorf("duration = %d, ожидалось 60", pc.Duration)
	}
}

// TestFileNameFromUniqueid проверяет, что минута имени файла берётся из метки
// времени в Uniqueid (время создания канала).
func TestFileNameFromUniqueid(t *testing.T) {
	st := newFakeStore()
	uid := fmt.Sprintf("%d.5482", tStart.Unix())
	st.put(store.Call{
		Uniqueid:    uid,
		CallerIDNum: "79990001122",
		Status:      store.StatusAnswered,
		AgentPeer:   "327",
		CreatedAt:   t0,
		AnsweredAt:  &tAnswer,
	})
	cl := &fakeClient{}
	fin := newFinalizer(st, cl, Config{TelephonyNumber: 327})
	fin.now = fixedNow(tEnd)

	if err := fin.OnHangup(context.Background(), hangupFrame(uid)); err != nil {
		t.Fatalf("OnHangup: %v", err)
	}
	fin.Wait()

	pc, ok := cl.lastBuilt()
	if !ok {
		t.Fatal("phone_call не собран")
	}
	wantURL := fmt.Sprintf("https://rec/%s-2026-09-30-09_15-79990001122-s.mp3", uid)
	if pc.FileURL != wantURL {
		t.Errorf("file_url = %q, ожидался %q", pc.FileURL, wantURL)
	}
}

// TestFinishedBeforeStartedClamped проверяет устойчивость к рассинхрону времени:
// если отбой раньше ответа, длительность всё равно не меньше 1 (Okdesk отвергает 0).
func TestFinishedBeforeStartedClamped(t *testing.T) {
	st := newFakeStore()
	st.put(baseCall)
	cl := &fakeClient{}
	fin := newFinalizer(st, cl, Config{TelephonyNumber: 327})
	fin.now = fixedNow(tStart) // раньше времени ответа

	if err := fin.OnHangup(context.Background(), hangupFrame("u1")); err != nil {
		t.Fatalf("OnHangup: %v", err)
	}
	fin.Wait()

	pc, ok := cl.lastBuilt()
	if !ok {
		t.Fatal("phone_call не собран")
	}
	if !pc.FinishedAt.Equal(tAnswer) {
		t.Errorf("finished_at = %v, ожидалось выравнивание по started_at %v", pc.FinishedAt, tAnswer)
	}
	if pc.Duration != 1 {
		t.Errorf("duration = %d, ожидалось 1", pc.Duration)
	}
}

func TestSendFailureEnqueuesRetry(t *testing.T) {
	st := newFakeStore()
	st.put(baseCall)
	cl := &fakeClient{sendErr: errors.New("сеть недоступна")}
	fin := newFinalizer(st, cl, Config{
		TelephonyNumber:     327,
		RetryInitialBackoff: 5 * time.Second,
	})

	if err := fin.OnHangup(context.Background(), hangupFrame("u1")); err != nil {
		t.Fatalf("OnHangup: %v", err)
	}
	fin.Wait()

	if len(st.retries) != 1 {
		t.Fatalf("записей retry = %d, ожидалась 1", len(st.retries))
	}
	item := st.retries[0]
	if item.CallID != "u1" || item.Payload == "" {
		t.Errorf("retry-запись = %+v", item)
	}
	if !item.NextAttemptAt.After(time.Now().Add(4 * time.Second)) {
		t.Errorf("next_attempt_at = %v, ожидалось ~через 5с", item.NextAttemptAt)
	}
	if !st.containsDeleted("u1") {
		t.Error("корреляция должна удаляться даже при сбое отправки")
	}
}

func TestUnknownHangupIgnored(t *testing.T) {
	st := newFakeStore()
	cl := &fakeClient{}
	fin := newFinalizer(st, cl, Config{})

	if err := fin.OnHangup(context.Background(), hangupFrame("нет")); err != nil {
		t.Fatalf("OnHangup: %v", err)
	}
	fin.Close()

	if _, ok := cl.lastBuilt(); ok {
		t.Error("неизвестный звонок не должен журналироваться")
	}
}

func TestIssueLookupErrorIgnored(t *testing.T) {
	st := newFakeStore()
	st.put(baseCall)
	cl := &fakeClient{issueErr: errors.New("Okdesk недоступен")}
	fin := newFinalizer(st, cl, Config{TelephonyNumber: 327})
	fin.now = fixedNow(tEnd)

	fin.OnHangup(context.Background(), hangupFrame("u1"))
	fin.Wait()

	pc, ok := cl.lastBuilt()
	if !ok {
		t.Fatal("запись должна быть собрана несмотря на сбой поиска заявки")
	}
	if pc.IssueID != nil {
		t.Errorf("issue_id = %v, ожидался nil", pc.IssueID)
	}
	if cl.sendCount() != 1 {
		t.Errorf("отправок = %d, ожидалась 1", cl.sendCount())
	}
}

// TestAbandonedCallNotJournaled проверяет, что брошенные звонки (оператор не
// ответил) в Okdesk не отправляются, но корреляция и дедупликация удаляются.
func TestAbandonedCallNotJournaled(t *testing.T) {
	st := newFakeStore()
	st.put(store.Call{
		Uniqueid:    "u2",
		CallerIDNum: "79990001122",
		Status:      store.StatusAbandoned,
		CreatedAt:   t0,
	})
	cl := &fakeClient{}
	fin := newFinalizer(st, cl, Config{TelephonyNumber: 327})

	if err := fin.OnHangup(context.Background(), hangupFrame("u2")); err != nil {
		t.Fatalf("OnHangup: %v", err)
	}
	fin.Wait()

	if _, ok := cl.lastBuilt(); ok {
		t.Error("брошенный звонок не должен журналироваться")
	}
	if cl.sendCount() != 0 {
		t.Errorf("отправок = %d, ожидалось 0", cl.sendCount())
	}
	if !st.containsDeleted("u2") {
		t.Error("корреляция брошенного звонка должна удаляться")
	}
}

// TestAnsweredWithoutNumberSkipped проверяет, что если внутренний номер
// ответившего оператора определить не удалось, запись не отправляется.
func TestAnsweredWithoutNumberSkipped(t *testing.T) {
	st := newFakeStore()
	st.put(store.Call{
		Uniqueid:    "u3",
		CallerIDNum: "79990001122",
		Status:      store.StatusAnswered,
		AgentPeer:   "ivan",
		CreatedAt:   t0,
		AnsweredAt:  &tAnswer,
	})
	cl := &fakeClient{}
	fin := newFinalizer(st, cl, Config{})

	if err := fin.OnHangup(context.Background(), hangupFrame("u3")); err != nil {
		t.Fatalf("OnHangup: %v", err)
	}
	fin.Wait()

	if _, ok := cl.lastBuilt(); ok {
		t.Error("без внутреннего номера запись отправлять нельзя")
	}
	if !st.containsDeleted("u3") {
		t.Error("корреляция должна удаляться")
	}
}

// TestAutoLinkDisabled проверяет, что при выключенной автопривязке поиск заявки
// не выполняется вовсе, а запись уходит в Okdesk без issue_id.
func TestAutoLinkDisabled(t *testing.T) {
	st := newFakeStore()
	st.put(baseCall)
	cl := &fakeClient{issueID: 42, issueOK: true}
	fin := New(st, cl, recording.New("https://rec/", time.UTC), Config{
		TelephonyNumber:     327,
		RetryInitialBackoff: time.Second,
		AutoLinkIssue:       false,
	}, silent())
	fin.now = fixedNow(tEnd)

	if err := fin.OnHangup(context.Background(), hangupFrame("u1")); err != nil {
		t.Fatalf("OnHangup: %v", err)
	}
	fin.Wait()

	if n := cl.findCount(); n != 0 {
		t.Errorf("FindIssueID вызван %d раз, ожидалось 0", n)
	}
	pc, ok := cl.lastBuilt()
	if !ok {
		t.Fatal("phone_call не собран")
	}
	if pc.IssueID != nil {
		t.Errorf("issue_id = %v, ожидался nil при отключённой автопривязке", *pc.IssueID)
	}
	if cl.sendCount() != 1 {
		t.Errorf("отправок = %d, ожидалась 1", cl.sendCount())
	}
}
