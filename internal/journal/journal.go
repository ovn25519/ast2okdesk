// Package journal завершает обработку звонка: по событию Hangup дожидается
// события Cdr (с таймаутом), собирает запись о разговоре, подбирает открытую
// заявку для автопривязки и отправляет данные в Okdesk. При сбое отправки
// запись кладётся в очередь retry SQLite.
package journal

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ovn25519/ast2okdesk/internal/ami"
	"github.com/ovn25519/ast2okdesk/internal/okdesk"
	"github.com/ovn25519/ast2okdesk/internal/store"
)

// Значения по умолчанию.
const (
	defaultCdrTimeout      = 10 * time.Second
	defaultFinalizeTimeout = 30 * time.Second
)

// Store — подмножество хранилища, необходимое финализатору.
type Store interface {
	GetCall(ctx context.Context, uniqueid string) (store.Call, error)
	MarkFinished(ctx context.Context, uniqueid string, at time.Time) error
	DeleteCall(ctx context.Context, uniqueid string) error
	DeleteDedupByCall(ctx context.Context, uniqueid string) error
	EnqueueRetry(ctx context.Context, item store.NewRetryItem) (bool, error)
}

// CallClient — операции Okdesk, необходимые финализатору.
type CallClient interface {
	BuildPhoneCall(call okdesk.PhoneCall) ([]byte, error)
	SendPhoneCall(ctx context.Context, payload []byte) error
	FindIssueID(ctx context.Context, phone string) (int, bool, error)
}

// Recordings — реконструкция имени файла записи и разбор времени Cdr.
type Recordings interface {
	ParseStartTime(raw string) (time.Time, error)
	URLFor(uniqueid, callerID string, start time.Time) string
}

// Config — параметры финализатора.
type Config struct {
	// IncomingPhoneNumber — входящий номер, попадающий в receiver_phone.
	IncomingPhoneNumber string
	// CdrTimeout — сколько ждать событие Cdr после Hangup.
	CdrTimeout time.Duration
	// RetryInitialBackoff — задержка первой повторной отправки при сбое.
	RetryInitialBackoff time.Duration
	// FinalizeTimeout — предел на обработку одного звонка.
	FinalizeTimeout time.Duration
	// AutoLinkIssue — подбирать заявку для автопривязки. Если выключено,
	// issue_id не отправляется, а связь с заявками устанавливает сам Okdesk.
	AutoLinkIssue bool
}

// Finalizer — конечный автомат завершения звонка по Uniqueid.
type Finalizer struct {
	store  Store
	client CallClient
	rec    Recordings
	cfg    Config
	log    *slog.Logger
	now    func() time.Time

	mu      sync.Mutex
	pending map[string]*pending

	wg sync.WaitGroup

	cdrReceived atomic.Int64
	callsLogged atomic.Int64
}

// pending — звонок, ожидающий событие Cdr.
type pending struct {
	hangupAt time.Time
	timer    *time.Timer
}

// New создаёт финализатор. client и rec обязательны.
func New(st Store, client CallClient, rec Recordings, cfg Config, logger *slog.Logger) *Finalizer {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.CdrTimeout <= 0 {
		cfg.CdrTimeout = defaultCdrTimeout
	}
	if cfg.FinalizeTimeout <= 0 {
		cfg.FinalizeTimeout = defaultFinalizeTimeout
	}
	f := &Finalizer{
		store:   st,
		client:  client,
		rec:     rec,
		cfg:     cfg,
		log:     logger,
		now:     time.Now,
		pending: make(map[string]*pending),
	}
	if cfg.AutoLinkIssue {
		f.log.Info("автопривязка заявок включена")
	} else {
		f.log.Info("автопривязка заявок выключена: связь с заявками устанавливает сам Okdesk")
	}
	return f
}

// CDRCount возвращает число принятых событий Cdr (для мониторинга).
func (j *Finalizer) CDRCount() int64 { return j.cdrReceived.Load() }

// CallsLogged возвращает число успешно зажурналированных звонков.
func (j *Finalizer) CallsLogged() int64 { return j.callsLogged.Load() }

// OnHangup запускает ожидание Cdr для известного звонка. Звонки, отсутствующие
// в корреляции (например, канал оператора), игнорируются.
func (j *Finalizer) OnHangup(ctx context.Context, f ami.Frame) error {
	uniqueid := f.Get("Uniqueid")
	if uniqueid == "" {
		return nil
	}
	call, err := j.store.GetCall(ctx, uniqueid)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	j.mu.Lock()
	if _, ok := j.pending[uniqueid]; ok {
		j.mu.Unlock()
		return nil
	}
	p := &pending{hangupAt: j.now()}
	p.timer = time.AfterFunc(j.cfg.CdrTimeout, func() { j.onTimeout(uniqueid) })
	j.pending[uniqueid] = p
	j.mu.Unlock()

	// Предварительно отмечаем завершение; время уточним из Cdr.EndTime.
	if err := j.store.MarkFinished(ctx, uniqueid, p.hangupAt); err != nil && !errors.Is(err, store.ErrNotFound) {
		j.log.Warn("Hangup: отметка завершения", "uniqueid", uniqueid, "error", err)
	}
	j.log.Info("Hangup: ожидание Cdr",
		"uniqueid", uniqueid, "caller", call.CallerIDNum, "timeout", j.cfg.CdrTimeout)
	return nil
}

// OnCdr принимает событие Cdr и завершает ожидающий звонок.
func (j *Finalizer) OnCdr(_ context.Context, f ami.Frame) {
	uniqueid := f.Get("UniqueID")
	if uniqueid == "" {
		uniqueid = f.Get("Uniqueid")
	}
	if uniqueid == "" {
		return
	}
	j.cdrReceived.Add(1)

	j.mu.Lock()
	p, ok := j.pending[uniqueid]
	if ok {
		delete(j.pending, uniqueid)
	}
	j.mu.Unlock()
	if !ok {
		j.log.Debug("Cdr: нет ожидающего звонка", "uniqueid", uniqueid)
		return
	}
	if p.timer != nil {
		p.timer.Stop()
	}
	j.startFinalize(uniqueid, f)
}

// onTimeout срабатывает, если Cdr не пришёл за CdrTimeout: звонок журналируется
// по данным корреляции.
func (j *Finalizer) onTimeout(uniqueid string) {
	j.mu.Lock()
	_, ok := j.pending[uniqueid]
	if ok {
		delete(j.pending, uniqueid)
	}
	j.mu.Unlock()
	if !ok {
		return
	}
	j.log.Warn("Cdr не получен за отведённое время, журналируем по данным корреляции",
		"uniqueid", uniqueid, "timeout", j.cfg.CdrTimeout)
	j.startFinalize(uniqueid, ami.NewFrame(nil))
}

// startFinalize запускает обработку завершённого звонка в отдельной горутине,
// чтобы не задерживать поток событий AMI сетевыми вызовами.
func (j *Finalizer) startFinalize(uniqueid string, cdr ami.Frame) {
	j.wg.Add(1)
	go func() {
		defer j.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), j.cfg.FinalizeTimeout)
		defer cancel()
		j.finalize(ctx, uniqueid, cdr)
	}()
}

// finalize собирает и отправляет запись о звонке.
func (j *Finalizer) finalize(ctx context.Context, uniqueid string, cdr ami.Frame) {
	call, err := j.store.GetCall(ctx, uniqueid)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		j.log.Error("журналирование: чтение звонка", "uniqueid", uniqueid, "error", err)
		return
	}

	startedAt, finishedAt, duration, fileStart := j.timing(call, cdr)

	fileURL := ""
	if j.rec != nil {
		fileURL = j.rec.URLFor(uniqueid, call.CallerIDNum, fileStart)
	}

	var issueID *int
	if j.cfg.AutoLinkIssue {
		issueID = j.findIssue(ctx, call.CallerIDNum)
	}

	payload, err := j.client.BuildPhoneCall(okdesk.PhoneCall{
		CallID:        uniqueid,
		StartedAt:     startedAt,
		FinishedAt:    finishedAt,
		Duration:      duration,
		Direction:     okdesk.DirectionIncoming,
		SourcePhone:   call.CallerIDNum,
		ReceiverPhone: j.cfg.IncomingPhoneNumber,
		FileURL:       fileURL,
		IssueID:       issueID,
	})
	if err != nil {
		j.log.Error("журналирование: сборка запроса", "uniqueid", uniqueid, "error", err)
		return
	}

	if err := j.client.SendPhoneCall(ctx, payload); err != nil {
		j.enqueueRetry(ctx, uniqueid, payload, err)
	} else {
		j.callsLogged.Add(1)
		j.log.Info("звонок зажурналирован",
			"uniqueid", uniqueid, "duration", duration, "issue_id", issueID, "file_url", fileURL)
	}

	// Звонок завершён: корреляция и дедупликация больше не нужны.
	j.cleanupCall(ctx, uniqueid)
}

// enqueueRetry кладёт недоставленную запись в очередь повторной отправки.
func (j *Finalizer) enqueueRetry(ctx context.Context, uniqueid string, payload []byte, sendErr error) {
	j.log.Warn("журналирование: сбой отправки, запись в retry", "uniqueid", uniqueid, "error", sendErr)
	_, err := j.store.EnqueueRetry(ctx, store.NewRetryItem{
		CallID:        uniqueid,
		Payload:       string(payload),
		NextAttemptAt: j.now().Add(j.cfg.RetryInitialBackoff),
	})
	if err != nil {
		j.log.Error("журналирование: постановка в retry", "uniqueid", uniqueid, "error", err)
	}
}

// findIssue подбирает заявку для автопривязки. Ошибки поиска не фатальны.
func (j *Finalizer) findIssue(ctx context.Context, phone string) *int {
	if j.client == nil || strings.TrimSpace(phone) == "" {
		return nil
	}
	id, ok, err := j.client.FindIssueID(ctx, phone)
	if err != nil {
		j.log.Warn("журналирование: поиск заявки", "phone", phone, "error", err)
		return nil
	}
	if !ok {
		return nil
	}
	return &id
}

// cleanupCall удаляет корреляцию и дедупликацию завершённого звонка.
func (j *Finalizer) cleanupCall(ctx context.Context, uniqueid string) {
	if err := j.store.DeleteDedupByCall(ctx, uniqueid); err != nil {
		j.log.Warn("удаление дедупликации", "uniqueid", uniqueid, "error", err)
	}
	if err := j.store.DeleteCall(ctx, uniqueid); err != nil {
		j.log.Warn("удаление корреляции", "uniqueid", uniqueid, "error", err)
	}
}

// timing вычисляет времена и длительность разговора. При отсутствии Cdr
// используются данные корреляции.
func (j *Finalizer) timing(call store.Call, cdr ami.Frame) (startedAt, finishedAt time.Time, duration int, fileStart time.Time) {
	var cdrStart, cdrAnswer, cdrEnd time.Time
	if j.rec != nil {
		if t, err := j.rec.ParseStartTime(cdr.Get("StartTime")); err == nil {
			cdrStart = t
		}
		if t, err := j.rec.ParseStartTime(cdr.Get("AnswerTime")); err == nil {
			cdrAnswer = t
		}
		if t, err := j.rec.ParseStartTime(cdr.Get("EndTime")); err == nil {
			cdrEnd = t
		}
	}
	duration = parseSeconds(cdr.Get("BillableSeconds"))
	if duration <= 0 {
		duration = parseSeconds(cdr.Get("Duration"))
	}

	startedAt = cdrAnswer
	if startedAt.IsZero() {
		startedAt = cdrStart
	}
	if startedAt.IsZero() {
		if call.AnsweredAt != nil {
			startedAt = *call.AnsweredAt
		} else {
			startedAt = call.CreatedAt
		}
	}

	fileStart = cdrStart
	if fileStart.IsZero() {
		fileStart = call.CreatedAt
	}

	finishedAt = cdrEnd
	if finishedAt.IsZero() {
		if call.FinishedAt != nil {
			finishedAt = *call.FinishedAt
		} else {
			finishedAt = j.now()
		}
	}
	if finishedAt.Before(startedAt) {
		finishedAt = startedAt
	}
	if duration <= 0 {
		if d := int(finishedAt.Sub(startedAt).Seconds()); d > 0 {
			duration = d
		}
	}
	return startedAt, finishedAt, duration, fileStart
}

// parseSeconds разбирает неотрицательное целое из строки Cdr.
func parseSeconds(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// Wait ожидает завершения обработки всех начатых звонков.
func (j *Finalizer) Wait() { j.wg.Wait() }

// Close останавливает ожидание Cdr и дожидается обработки начатых звонков.
func (j *Finalizer) Close() {
	j.mu.Lock()
	for id, p := range j.pending {
		if p.timer != nil {
			p.timer.Stop()
		}
		delete(j.pending, id)
	}
	j.mu.Unlock()
	j.wg.Wait()
}
