// Package okdesk реализует клиент REST API Okdesk, необходимый интеграции:
// передачу информации о входящем звонке (screen-pop), создание записи о
// телефонном разговоре и автоматическую привязку звонка к открытой заявке.
//
// Клиент не логирует api_token: в URL попадает только путь, а тело ответа при
// ошибке очищается от значения токена.
package okdesk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHTTPTimeout = 15 * time.Second
	maxResponseBytes   = 1 << 20 // 1 МиБ
	maxErrorBodyChars  = 300
	apiTimeLayout      = "2006-01-02 15:04"
)

// DirectionIncoming — направление «входящий» в phone_calls.
const DirectionIncoming = 0

// Observer получает результат каждого обращения к Okdesk (для счётчиков
// мониторинга). err равен nil при успешном ответе.
type Observer interface {
	ObserveAPI(path string, err error)
}

// Config — параметры клиента Okdesk.
type Config struct {
	BaseURL            string
	APIToken           string
	SearchNumbersCount int
	// Timezone — часовой пояс аккаунта Okdesk (IANA). Применяется к started_at
	// и finished_at при создании записи о разговоре.
	Timezone   *time.Location
	HTTPClient *http.Client
	Logger     *slog.Logger
	// Observer, если задан, уведомляется о каждом HTTP-запросе к Okdesk.
	Observer Observer
}

// Client — клиент REST API Okdesk.
type Client struct {
	baseURL            string
	apiToken           string
	searchNumbersCount int
	tz                 *time.Location
	http               *http.Client
	log                *slog.Logger
	observer           Observer
}

// New создаёт клиент и проверяет обязательные параметры.
func New(cfg Config) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, errors.New("okdesk: не задан base_url")
	}
	if strings.TrimSpace(cfg.APIToken) == "" {
		return nil, errors.New("okdesk: не задан api_token")
	}
	if cfg.Timezone == nil {
		return nil, errors.New("okdesk: не задан timezone")
	}
	if cfg.SearchNumbersCount < 1 {
		return nil, errors.New("okdesk: search_numbers_count должен быть >= 1")
	}

	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: defaultHTTPTimeout}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Client{
		baseURL:            base,
		apiToken:           cfg.APIToken,
		searchNumbersCount: cfg.SearchNumbersCount,
		tz:                 cfg.Timezone,
		http:               hc,
		log:                logger,
		observer:           cfg.Observer,
	}, nil
}

// ScreenPop передаёт в Okdesk информацию о входящем звонке, чтобы в интерфейсе
// оператора открылась карточка абонента (POST /api/v1/telephony/messages).
func (c *Client) ScreenPop(ctx context.Context, phone string, telephonyNumber int) error {
	body := screenPopRequest{
		TelephonyNumbers:   []int{telephonyNumber},
		SearchNumbersCount: c.searchNumbersCount,
		Phone:              phone,
	}
	if _, err := c.request(ctx, http.MethodPost, "/api/v1/telephony/messages", nil, body); err != nil {
		return err
	}
	c.log.Debug("screen-pop отправлен", "telephony_number", telephonyNumber)
	return nil
}

// PhoneCall — данные для создания записи о телефонном разговоре.
type PhoneCall struct {
	CallID        string
	StartedAt     time.Time
	FinishedAt    time.Time
	Duration      int
	Direction     int
	SourcePhone   string
	ReceiverPhone string
	FileURL       string
	// IssueID — заявка для автопривязки; nil — без привязки.
	IssueID *int
}

// CreatePhoneCall создаёт запись о телефонном разговоре
// (POST /api/v1/phone_calls). Время конвертируется в часовой пояс Okdesk.
func (c *Client) CreatePhoneCall(ctx context.Context, call PhoneCall) error {
	payload, err := c.BuildPhoneCall(call)
	if err != nil {
		return err
	}
	if err := c.SendPhoneCall(ctx, payload); err != nil {
		return err
	}
	c.log.Info("запись о звонке создана в Okdesk", "call_id", call.CallID, "issue_id", call.IssueID)
	return nil
}

// BuildPhoneCall собирает тело запроса phone_call (объект без внешней обёртки)
// и сериализует его. Результат пригоден для отправки через SendPhoneCall и для
// хранения в очереди retry.
func (c *Client) BuildPhoneCall(call PhoneCall) ([]byte, error) {
	payload := phoneCallRequest{
		CallID:             call.CallID,
		StartedAt:          c.formatAPITime(call.StartedAt),
		FinishedAt:         c.formatAPITime(call.FinishedAt),
		Duration:           call.Duration,
		Direction:          call.Direction,
		SourcePhone:        call.SourcePhone,
		ReceiverPhone:      call.ReceiverPhone,
		SearchNumbersCount: c.searchNumbersCount,
		FileURL:            call.FileURL,
		IssueID:            call.IssueID,
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("okdesk: сериализация phone_call: %w", err)
	}
	return buf, nil
}

// SendPhoneCall отправляет заранее собранный объект phone_call
// (POST /api/v1/phone_calls). Используется как при первичной отправке, так и
// воркером повторной доставки.
func (c *Client) SendPhoneCall(ctx context.Context, payload []byte) error {
	_, err := c.request(ctx, http.MethodPost, "/api/v1/phone_calls", nil,
		map[string]any{"phone_call": json.RawMessage(payload)})
	return err
}

// FindIssueID подбирает открытую заявку клиента для автопривязки.
//
// Порядок: контакт по номеру → открытые заявки контакта; при отсутствии контакта
// или заявок — компания по номеру → заявки компании. Среди открытых заявок
// приоритет отдаётся заявке, где звонящий — инициатор/наблюдатель, затем —
// заявке с ближайшим deadline_at. Если ничего не найдено, ok == false.
func (c *Client) FindIssueID(ctx context.Context, phone string) (int, bool, error) {
	contactID, companyID, foundContact, err := c.findContact(ctx, phone)
	if err != nil {
		return 0, false, err
	}
	if foundContact {
		issues, err := c.listIssues(ctx, contactID, 0)
		if err != nil {
			return 0, false, err
		}
		if id, ok := SelectIssue(issues, contactID); ok {
			return id, true, nil
		}
	}

	if companyID == 0 {
		cid, found, err := c.findCompany(ctx, phone)
		if err != nil {
			return 0, false, err
		}
		if found {
			companyID = cid
		}
	}

	if companyID != 0 {
		issues, err := c.listIssues(ctx, 0, companyID)
		if err != nil {
			return 0, false, err
		}
		if id, ok := SelectIssue(issues, contactID); ok {
			return id, true, nil
		}
	}
	return 0, false, nil
}

// Issue — открытая заявка Okdesk в объёме, нужном для выбора.
type Issue struct {
	ID          int
	DeadlineAt  *time.Time
	AuthorID    int
	ObserverIDs []int
}

// involves сообщает, является ли контакт инициатором или наблюдателем заявки.
func (i Issue) involves(contactID int) bool {
	if contactID <= 0 {
		return false
	}
	if i.AuthorID == contactID {
		return true
	}
	for _, id := range i.ObserverIDs {
		if id == contactID {
			return true
		}
	}
	return false
}

// SelectIssue выбирает заявку для привязки: сначала среди заявок, где звонящий —
// инициатор или наблюдатель, затем по ближайшему deadline_at (заявки без
// deadline считаются последними, при равенстве выбирается меньший id).
func SelectIssue(issues []Issue, contactID int) (int, bool) {
	if len(issues) == 0 {
		return 0, false
	}

	preferred := make([]Issue, 0, len(issues))
	for _, issue := range issues {
		if issue.involves(contactID) {
			preferred = append(preferred, issue)
		}
	}
	candidates := issues
	if len(preferred) > 0 {
		candidates = preferred
	}

	best := candidates[0]
	for _, issue := range candidates[1:] {
		if closerDeadline(issue, best) {
			best = issue
		}
	}
	return best.ID, true
}

// closerDeadline сообщает, что заявка a приоритетнее b по сроку решения.
func closerDeadline(a, b Issue) bool {
	switch {
	case a.DeadlineAt == nil && b.DeadlineAt == nil:
		return a.ID < b.ID
	case a.DeadlineAt == nil:
		return false
	case b.DeadlineAt == nil:
		return true
	case a.DeadlineAt.Equal(*b.DeadlineAt):
		return a.ID < b.ID
	default:
		return a.DeadlineAt.Before(*b.DeadlineAt)
	}
}

// findContact ищет контакт по последним цифрам номера. При нескольких
// совпадениях выбирается контакт с наименьшим id.
func (c *Client) findContact(ctx context.Context, phone string) (contactID, companyID int, ok bool, err error) {
	q := url.Values{}
	q.Set("phone", lastDigits(phone, c.searchNumbersCount))

	data, err := c.request(ctx, http.MethodGet, "/api/v1/contacts/", q, nil)
	if err != nil {
		return 0, 0, false, err
	}
	raws, err := extractList(data, "contacts")
	if err != nil {
		return 0, 0, false, fmt.Errorf("okdesk: разбор списка контактов: %w", err)
	}

	var best *contactDTO
	for _, raw := range raws {
		var dto contactDTO
		if json.Unmarshal(raw, &dto) != nil || dto.ID <= 0 {
			continue
		}
		if best == nil || dto.ID < best.ID {
			d := dto
			best = &d
		}
	}
	if best == nil {
		return 0, 0, false, nil
	}
	if best.Company != nil {
		companyID = best.Company.ID
	}
	return best.ID, companyID, true, nil
}

// findCompany ищет компанию по последним цифрам номера.
func (c *Client) findCompany(ctx context.Context, phone string) (int, bool, error) {
	q := url.Values{}
	q.Set("phone", lastDigits(phone, c.searchNumbersCount))

	data, err := c.request(ctx, http.MethodGet, "/api/v1/companies/", q, nil)
	if err != nil {
		return 0, false, err
	}
	raws, err := extractList(data, "companies")
	if err != nil {
		return 0, false, fmt.Errorf("okdesk: разбор списка компаний: %w", err)
	}

	best := 0
	for _, raw := range raws {
		var dto idDTO
		if json.Unmarshal(raw, &dto) != nil || dto.ID <= 0 {
			continue
		}
		if best == 0 || dto.ID < best {
			best = dto.ID
		}
	}
	return best, best != 0, nil
}

// listIssues запрашивает открытые заявки контакта или компании.
func (c *Client) listIssues(ctx context.Context, contactID, companyID int) ([]Issue, error) {
	q := url.Values{}
	if contactID > 0 {
		q.Set("contact_ids[]", strconv.Itoa(contactID))
	}
	if companyID > 0 {
		q.Set("company_ids[]", strconv.Itoa(companyID))
	}
	q.Set("status_codes_not[]", "completed")

	data, err := c.request(ctx, http.MethodGet, "/api/v1/issues/list", q, nil)
	if err != nil {
		return nil, err
	}
	raws, err := extractList(data, "issues")
	if err != nil {
		return nil, fmt.Errorf("okdesk: разбор списка заявок: %w", err)
	}

	issues := make([]Issue, 0, len(raws))
	for _, raw := range raws {
		var dto issueDTO
		if json.Unmarshal(raw, &dto) != nil || dto.ID <= 0 {
			continue
		}
		issue := Issue{ID: dto.ID}
		if dto.Author != nil {
			issue.AuthorID = dto.Author.ID
		}
		for _, o := range dto.Observers {
			if o.ID > 0 {
				issue.ObserverIDs = append(issue.ObserverIDs, o.ID)
			}
		}
		if dto.DeadlineAt != nil {
			if t, found := parseDeadline(*dto.DeadlineAt); found {
				issue.DeadlineAt = &t
			}
		}
		issues = append(issues, issue)
	}
	return issues, nil
}

// request выполняет HTTP-запрос к Okdesk, добавляя api_token в query.
func (c *Client) request(ctx context.Context, method, path string, query url.Values, body any) (data []byte, err error) {
	if c.observer != nil {
		// observer уведомляется о результате любого обращения (в т.ч. ошибках
		// сети и разборе запроса).
		defer func() { c.observer.ObserveAPI(path, err) }()
	}

	if query == nil {
		query = url.Values{}
	}
	query.Set("api_token", c.apiToken)
	target := c.baseURL + path + "?" + query.Encode()

	var reader io.Reader
	if body != nil {
		buf, merr := json.Marshal(body)
		if merr != nil {
			return nil, fmt.Errorf("okdesk: сериализация запроса %s: %w", path, merr)
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, fmt.Errorf("okdesk: создание запроса %s: %w", path, err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	c.log.Debug("запрос к Okdesk", "method", method, "path", path)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("okdesk: запрос %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	data, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("okdesk: чтение ответа %s: %w", path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return data, &APIError{
			Method:     method,
			Path:       path,
			StatusCode: resp.StatusCode,
			Body:       c.redact(string(data)),
		}
	}
	return data, nil
}

// redact удаляет значение api_token из строки (страховка на случай, если сервер
// вернёт его в теле ответа).
func (c *Client) redact(s string) string {
	if c.apiToken == "" {
		return s
	}
	return strings.ReplaceAll(s, c.apiToken, "***")
}

// formatAPITime приводит время к часовому поясу Okdesk и формату API.
func (c *Client) formatAPITime(t time.Time) string {
	return t.In(c.tz).Format(apiTimeLayout)
}

// APIError описывает неуспешный ответ Okdesk.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > maxErrorBodyChars {
		body = body[:maxErrorBodyChars] + "…"
	}
	if body == "" {
		return fmt.Sprintf("okdesk: %s %s вернул %d", e.Method, e.Path, e.StatusCode)
	}
	return fmt.Sprintf("okdesk: %s %s вернул %d: %s", e.Method, e.Path, e.StatusCode, body)
}

// --- DTO и вспомогательные функции -----------------------------------------

type screenPopRequest struct {
	TelephonyNumbers   []int  `json:"telephony_numbers"`
	SearchNumbersCount int    `json:"search_numbers_count"`
	Phone              string `json:"phone"`
}

type phoneCallRequest struct {
	CallID             string `json:"call_id"`
	StartedAt          string `json:"started_at"`
	FinishedAt         string `json:"finished_at"`
	Duration           int    `json:"duration"`
	Direction          int    `json:"direction"`
	SourcePhone        string `json:"source_phone"`
	ReceiverPhone      string `json:"receiver_phone"`
	SearchNumbersCount int    `json:"search_numbers_count"`
	FileURL            string `json:"file_url,omitempty"`
	IssueID            *int   `json:"issue_id,omitempty"`
}

type idDTO struct {
	ID int `json:"id"`
}

type contactDTO struct {
	ID      int    `json:"id"`
	Company *idDTO `json:"company"`
}

type issueDTO struct {
	ID         int     `json:"id"`
	DeadlineAt *string `json:"deadline_at"`
	Author     *idDTO  `json:"author"`
	Observers  []idDTO `json:"observers"`
}

// lastDigits оставляет последние n цифр номера (нецифровые символы, включая «+»,
// отбрасываются).
func lastDigits(phone string, n int) string {
	digits := make([]byte, 0, len(phone))
	for i := 0; i < len(phone); i++ {
		if phone[i] >= '0' && phone[i] <= '9' {
			digits = append(digits, phone[i])
		}
	}
	if n > 0 && len(digits) > n {
		digits = digits[len(digits)-n:]
	}
	return string(digits)
}

var deadlineLayouts = []string{
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	time.RFC3339,
	"2006-01-02",
}

// parseDeadline разбирает deadline_at в одном из поддерживаемых форматов.
func parseDeadline(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range deadlineLayouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// extractList извлекает массив объектов из ответа: как из обёртки вида
// {"contacts": [...]}, так и из массива в корне документа.
func extractList(data []byte, keys ...string) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, err
		}
		return arr, nil
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, err
	}
	for _, key := range keys {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, err
		}
		return arr, nil
	}
	return nil, nil
}
