package okdesk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ovn25519/ast2okdesk/internal/callflow"
)

// Контракт: клиент Okdesk пригоден для использования в callflow как ScreenPopper.
var _ callflow.ScreenPopper = (*Client)(nil)

func silentLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func loadLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("не удалось загрузить часовой пояс %q: %v", name, err)
	}
	return loc
}

func testClient(t *testing.T, srv *httptest.Server, tz *time.Location) *Client {
	t.Helper()
	c, err := New(Config{
		BaseURL:            srv.URL,
		APIToken:           "secret-token",
		SearchNumbersCount: 10,
		Timezone:           tz,
		HTTPClient:         srv.Client(),
		Logger:             silentLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func TestNewValidation(t *testing.T) {
	loc := time.UTC
	cases := []struct {
		name string
		cfg  Config
	}{
		{"без base_url", Config{APIToken: "t", SearchNumbersCount: 1, Timezone: loc}},
		{"без api_token", Config{BaseURL: "https://x.test", SearchNumbersCount: 1, Timezone: loc}},
		{"без timezone", Config{BaseURL: "https://x.test", APIToken: "t", SearchNumbersCount: 1}},
		{"нулевой search_numbers_count", Config{BaseURL: "https://x.test", APIToken: "t", Timezone: loc}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := New(c.cfg); err == nil {
				t.Error("ожидалась ошибка, получено nil")
			}
		})
	}
}

func TestScreenPop(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotQuery  url.Values
		gotBody   screenPopRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.Query()
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, http.StatusOK, "{}")
	}))
	defer srv.Close()

	c := testClient(t, srv, time.UTC)
	if err := c.ScreenPop(context.Background(), "+79991234567", 327); err != nil {
		t.Fatalf("ScreenPop: %v", err)
	}

	if gotMethod != http.MethodPost || gotPath != "/api/v1/telephony/messages" {
		t.Errorf("запрос %s %s, ожидалось POST /api/v1/telephony/messages", gotMethod, gotPath)
	}
	if gotQuery.Get("api_token") != "secret-token" {
		t.Errorf("api_token = %q", gotQuery.Get("api_token"))
	}
	if len(gotBody.TelephonyNumbers) != 1 || gotBody.TelephonyNumbers[0] != 327 {
		t.Errorf("telephony_numbers = %v", gotBody.TelephonyNumbers)
	}
	if gotBody.SearchNumbersCount != 10 {
		t.Errorf("search_numbers_count = %d", gotBody.SearchNumbersCount)
	}
	if gotBody.Phone != "+79991234567" {
		t.Errorf("phone = %q", gotBody.Phone)
	}
}

func TestScreenPopErrorRedactsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusInternalServerError, `{"error":"bad secret-token"}`)
	}))
	defer srv.Close()

	c := testClient(t, srv, time.UTC)
	err := c.ScreenPop(context.Background(), "79991234567", 327)
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("ожидался *APIError, получено %T", err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d", apiErr.StatusCode)
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Errorf("ошибка содержит api_token: %v", err)
	}
}

func TestCreatePhoneCallConvertsTimezone(t *testing.T) {
	var payload struct {
		PhoneCall phoneCallRequest `json:"phone_call"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/phone_calls" {
			t.Errorf("путь = %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		writeJSON(w, http.StatusCreated, "{}")
	}))
	defer srv.Close()

	// Клиент работает в зоне Okdesk (Москва, UTC+3), а время приходит в зоне
	// Asterisk (Екатеринбург, UTC+5).
	moscow := loadLoc(t, "Europe/Moscow")
	yekat := loadLoc(t, "Asia/Yekaterinburg")
	c := testClient(t, srv, moscow)

	issueID := 7
	call := PhoneCall{
		CallID:        "1700000000.42",
		StartedAt:     time.Date(2026, 9, 30, 9, 16, 0, 0, yekat),
		FinishedAt:    time.Date(2026, 9, 30, 9, 17, 30, 0, yekat),
		Duration:      90,
		Direction:     DirectionIncoming,
		SourcePhone:   "+79991234567",
		ReceiverPhone: "+73512345678",
		FileURL:       "https://calls.example.ru:8443/records/a.mp3",
		IssueID:       &issueID,
	}
	if err := c.CreatePhoneCall(context.Background(), call); err != nil {
		t.Fatalf("CreatePhoneCall: %v", err)
	}

	pc := payload.PhoneCall
	if pc.StartedAt != "2026-09-30 07:16" {
		t.Errorf("started_at = %q, ожидалось 2026-09-30 07:16", pc.StartedAt)
	}
	if pc.FinishedAt != "2026-09-30 07:17" {
		t.Errorf("finished_at = %q, ожидалось 2026-09-30 07:17", pc.FinishedAt)
	}
	if pc.CallID != "1700000000.42" || pc.Duration != 90 || pc.Direction != 0 ||
		pc.SourcePhone != "+79991234567" || pc.ReceiverPhone != "+73512345678" ||
		pc.SearchNumbersCount != 10 {
		t.Errorf("phone_call = %+v", pc)
	}
	if pc.FileURL == "" || pc.IssueID == nil || *pc.IssueID != 7 {
		t.Errorf("file_url/issue_id = %q/%v", pc.FileURL, pc.IssueID)
	}
}

func TestCreatePhoneCallOmitsOptionalFields(t *testing.T) {
	raw := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		raw = string(b)
		writeJSON(w, http.StatusCreated, "{}")
	}))
	defer srv.Close()

	c := testClient(t, srv, time.UTC)
	err := c.CreatePhoneCall(context.Background(), PhoneCall{
		CallID: "u1", StartedAt: time.Now(), FinishedAt: time.Now(),
		Direction: DirectionIncoming,
	})
	if err != nil {
		t.Fatalf("CreatePhoneCall: %v", err)
	}
	if strings.Contains(raw, "file_url") || strings.Contains(raw, "issue_id") {
		t.Errorf("необязательные поля не должны отправляться: %s", raw)
	}
}

func TestCreatePhoneCallError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusUnprocessableEntity, `{"errors":{"call_id":["обязателен"]}}`)
	}))
	defer srv.Close()

	c := testClient(t, srv, time.UTC)
	err := c.CreatePhoneCall(context.Background(), PhoneCall{CallID: ""})
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("ожидался *APIError 422, получено %v", err)
	}
}

func TestFindIssueIDByContact(t *testing.T) {
	var issuesQuery url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/contacts/", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("phone"); got != "9991234567" {
			t.Errorf("phone = %q, ожидалось 9991234567", got)
		}
		writeJSON(w, http.StatusOK, `{"contacts":[{"id":12,"company":{"id":3}},{"id":9}]}`)
	})
	mux.HandleFunc("/api/v1/issues/list", func(w http.ResponseWriter, r *http.Request) {
		issuesQuery = r.URL.Query()
		writeJSON(w, http.StatusOK, `{"issues":[{"id":5,"deadline_at":"2026-09-30 12:00:00","author":{"id":9}}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := testClient(t, srv, time.UTC)
	id, ok, err := c.FindIssueID(context.Background(), "+7 (999) 123-45-67")
	if err != nil || !ok || id != 5 {
		t.Fatalf("FindIssueID = (%d, %v, %v), ожидалось (5, true, nil)", id, ok, err)
	}
	// Контакт с наименьшим id (9), заявки запрашиваются по нему.
	if got := issuesQuery.Get("contact_ids[]"); got != "9" {
		t.Errorf("contact_ids[] = %q, ожидалось 9", got)
	}
	if got := issuesQuery.Get("status_codes_not[]"); got != "completed" {
		t.Errorf("status_codes_not[] = %q", got)
	}
}

func TestFindIssueIDPrefersInvolvedIssue(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/contacts/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"contacts":[{"id":9}]}`)
	})
	mux.HandleFunc("/api/v1/issues/list", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"issues":[
			{"id":8,"deadline_at":"2026-09-30 10:00:00"},
			{"id":7,"deadline_at":"2026-09-30 18:00:00","observers":[{"id":9}]}
		]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := testClient(t, srv, time.UTC)
	id, ok, err := c.FindIssueID(context.Background(), "79991234567")
	if err != nil || !ok || id != 7 {
		t.Fatalf("FindIssueID = (%d, %v, %v), ожидалось (7, true, nil)", id, ok, err)
	}
}

func TestFindIssueIDNearestDeadline(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/contacts/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"contacts":[{"id":9}]}`)
	})
	mux.HandleFunc("/api/v1/issues/list", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"issues":[
			{"id":1,"deadline_at":"2026-09-30 18:00:00","author":{"id":9}},
			{"id":2,"deadline_at":"2026-09-30 10:00:00","author":{"id":9}},
			{"id":3,"author":{"id":9}}
		]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := testClient(t, srv, time.UTC)
	id, ok, err := c.FindIssueID(context.Background(), "79991234567")
	if err != nil || !ok || id != 2 {
		t.Fatalf("FindIssueID = (%d, %v, %v), ожидалось (2, true, nil)", id, ok, err)
	}
}

func TestFindIssueIDCompanyFallback(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/contacts/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"contacts":[]}`)
	})
	mux.HandleFunc("/api/v1/companies/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"companies":[{"id":3}]}`)
	})
	var companyQuery url.Values
	mux.HandleFunc("/api/v1/issues/list", func(w http.ResponseWriter, r *http.Request) {
		companyQuery = r.URL.Query()
		writeJSON(w, http.StatusOK, `{"issues":[{"id":4,"deadline_at":"2026-09-30 12:00:00"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := testClient(t, srv, time.UTC)
	id, ok, err := c.FindIssueID(context.Background(), "79991234567")
	if err != nil || !ok || id != 4 {
		t.Fatalf("FindIssueID = (%d, %v, %v), ожидалось (4, true, nil)", id, ok, err)
	}
	if got := companyQuery.Get("company_ids[]"); got != "3" {
		t.Errorf("company_ids[] = %q, ожидалось 3", got)
	}
}

func TestFindIssueIDNone(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/contacts/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"contacts":[]}`)
	})
	mux.HandleFunc("/api/v1/companies/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"companies":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := testClient(t, srv, time.UTC)
	id, ok, err := c.FindIssueID(context.Background(), "79991234567")
	if err != nil || ok || id != 0 {
		t.Fatalf("FindIssueID = (%d, %v, %v), ожидалось (0, false, nil)", id, ok, err)
	}
}

func TestFindIssueIDTopLevelArray(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/contacts/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `[{"id":9}]`)
	})
	mux.HandleFunc("/api/v1/issues/list", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `[{"id":11,"deadline_at":"2026-09-30T12:00:00"}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := testClient(t, srv, time.UTC)
	id, ok, err := c.FindIssueID(context.Background(), "79991234567")
	if err != nil || !ok || id != 11 {
		t.Fatalf("FindIssueID = (%d, %v, %v), ожидалось (11, true, nil)", id, ok, err)
	}
}

func TestSelectIssue(t *testing.T) {
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(6 * time.Hour)
	pt0, pt1 := &t0, &t1

	cases := []struct {
		name      string
		issues    []Issue
		contactID int
		wantID    int
		wantOK    bool
	}{
		{"пусто", nil, 9, 0, false},
		{"наблюдатель важнее срока", []Issue{
			{ID: 8, DeadlineAt: pt0},
			{ID: 7, DeadlineAt: pt1, ObserverIDs: []int{9}},
		}, 9, 7, true},
		{"инициатор важнее срока", []Issue{
			{ID: 8, DeadlineAt: pt0},
			{ID: 7, DeadlineAt: pt1, AuthorID: 9},
		}, 9, 7, true},
		{"ближайший deadline", []Issue{
			{ID: 1, DeadlineAt: pt1},
			{ID: 2, DeadlineAt: pt0},
		}, 0, 2, true},
		{"заявка без срока последняя", []Issue{
			{ID: 1},
			{ID: 2, DeadlineAt: pt1},
		}, 0, 2, true},
		{"равный срок — меньший id", []Issue{
			{ID: 7, DeadlineAt: pt0},
			{ID: 3, DeadlineAt: pt0},
		}, 0, 3, true},
		{"единственная без срока", []Issue{{ID: 4}}, 0, 4, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, ok := SelectIssue(c.issues, c.contactID)
			if ok != c.wantOK || id != c.wantID {
				t.Errorf("SelectIssue = (%d, %v), ожидалось (%d, %v)", id, ok, c.wantID, c.wantOK)
			}
		})
	}
}

func TestLastDigits(t *testing.T) {
	cases := []struct {
		phone string
		n     int
		want  string
	}{
		{"+79991234567", 10, "9991234567"},
		{"8 (999) 123-45-67", 10, "9991234567"},
		{"79991234567", 10, "9991234567"},
		{"12345", 10, "12345"},
		{"+7 999 123 45 67", 4, "4567"},
		{"без цифр", 5, ""},
	}
	for _, c := range cases {
		if got := lastDigits(c.phone, c.n); got != c.want {
			t.Errorf("lastDigits(%q, %d) = %q, ожидалось %q", c.phone, c.n, got, c.want)
		}
	}
}

func TestParseDeadline(t *testing.T) {
	valid := []string{
		"2026-09-30 12:00:00",
		"2026-09-30T12:00:00",
		"2026-09-30T12:00:00+05:00",
		"2026-09-30",
	}
	for _, raw := range valid {
		if _, ok := parseDeadline(raw); !ok {
			t.Errorf("parseDeadline(%q) = false, ожидалось true", raw)
		}
	}
	for _, raw := range []string{"", "не дата", "30.09.2026"} {
		if _, ok := parseDeadline(raw); ok {
			t.Errorf("parseDeadline(%q) = true, ожидалось false", raw)
		}
	}
}
