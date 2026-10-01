package okdesk

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeObserver собирает уведомления об обращениях к Okdesk.
type fakeObserver struct {
	mu    sync.Mutex
	paths []string
	errs  []error
}

func (o *fakeObserver) ObserveAPI(path string, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.paths = append(o.paths, path)
	o.errs = append(o.errs, err)
}

func (o *fakeObserver) snapshot() ([]string, []error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.paths...), append([]error(nil), o.errs...)
}

func TestBuildPhoneCallProducesUnwrappedObject(t *testing.T) {
	moscow := loadLoc(t, "Europe/Moscow")
	yekat := loadLoc(t, "Asia/Yekaterinburg")
	c, err := New(Config{
		BaseURL:            "https://okdesk.test",
		APIToken:           "t",
		SearchNumbersCount: 10,
		Timezone:           moscow,
		Logger:             silentLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	issueID := 5
	payload, err := c.BuildPhoneCall(PhoneCall{
		CallID:      "u1",
		StartedAt:   time.Date(2026, 9, 30, 9, 16, 0, 0, yekat),
		FinishedAt:  time.Date(2026, 9, 30, 9, 18, 0, 0, yekat),
		Duration:    120,
		Direction:   DirectionIncoming,
		SourcePhone: "79990001122",
		IssueID:     &issueID,
	})
	if err != nil {
		t.Fatalf("BuildPhoneCall: %v", err)
	}

	// Тело должно быть самим объектом phone_call, без внешней обёртки.
	var pc phoneCallRequest
	if err := json.Unmarshal(payload, &pc); err != nil {
		t.Fatalf("тело не является объектом phone_call: %v (%s)", err, payload)
	}
	if pc.CallID != "u1" || pc.SearchNumbersCount != 10 {
		t.Errorf("phone_call = %+v", pc)
	}
	if pc.StartedAt != "2026-09-30 07:16" || pc.FinishedAt != "2026-09-30 07:18" {
		t.Errorf("времена = %q..%q, ожидались 2026-09-30 07:16..07:18", pc.StartedAt, pc.FinishedAt)
	}
	if pc.IssueID == nil || *pc.IssueID != 5 {
		t.Errorf("issue_id = %v", pc.IssueID)
	}
}

func TestSendPhoneCallWrapsPayload(t *testing.T) {
	var raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/phone_calls" {
			t.Errorf("путь = %q", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		raw = string(b)
		writeJSON(w, http.StatusCreated, "{}")
	}))
	defer srv.Close()

	c := testClient(t, srv, time.UTC)
	if err := c.SendPhoneCall(context.Background(), []byte(`{"call_id":"u1","duration":10}`)); err != nil {
		t.Fatalf("SendPhoneCall: %v", err)
	}

	var body struct {
		PhoneCall map[string]any `json:"phone_call"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("тело не разобрано: %v (%s)", err, raw)
	}
	if body.PhoneCall == nil {
		t.Fatalf("нет обёртки phone_call: %s", raw)
	}
	if body.PhoneCall["call_id"] != "u1" {
		t.Errorf("call_id = %v, тело: %s", body.PhoneCall["call_id"], raw)
	}
}

func TestObserverNotified(t *testing.T) {
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail {
			writeJSON(w, http.StatusInternalServerError, `{"error":"сбой"}`)
			return
		}
		writeJSON(w, http.StatusOK, "{}")
	}))
	defer srv.Close()

	obs := &fakeObserver{}
	c, err := New(Config{
		BaseURL:            srv.URL,
		APIToken:           "t",
		SearchNumbersCount: 10,
		Timezone:           time.UTC,
		HTTPClient:         srv.Client(),
		Logger:             silentLogger(),
		Observer:           obs,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := c.ScreenPop(context.Background(), "79990001122", 327); err != nil {
		t.Fatalf("ScreenPop: %v", err)
	}
	fail = true
	if err := c.ScreenPop(context.Background(), "79990001122", 327); err == nil {
		t.Fatal("ожидалась ошибка")
	}

	paths, errs := obs.snapshot()
	if len(paths) != 2 || len(errs) != 2 {
		t.Fatalf("наблюдений = %d, ожидалось 2", len(paths))
	}
	if paths[0] != "/api/v1/telephony/messages" {
		t.Errorf("path = %q", paths[0])
	}
	if errs[0] != nil {
		t.Errorf("первое обращение должно быть успешным: %v", errs[0])
	}
	if errs[1] == nil {
		t.Error("второе обращение должно быть с ошибкой")
	}
}
