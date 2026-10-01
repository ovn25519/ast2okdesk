// Package journal завершает обработку звонка: по событию Hangup собирает запись
// о разговоре, при необходимости подбирает открытую заявку для автопривязки и
// отправляет данные в Okdesk. При сбое отправки запись кладётся в очередь retry
// SQLite.
//
// Времена берутся из корреляции по Uniqueid, без события Cdr: начало разговора
// — момент ответа оператора (AMI AgentConnect), окончание — отбой (Hangup).
// Минута в имени файла записи восстанавливается из метки времени в Uniqueid.
package journal

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ovn25519/ast2okdesk/internal/ami"
	"github.com/ovn25519/ast2okdesk/internal/callflow"
	"github.com/ovn25519/ast2okdesk/internal/okdesk"
	"github.com/ovn25519/ast2okdesk/internal/store"
)

// defaultFinalizeTimeout — предел на обработку одного звонка.
const defaultFinalizeTimeout = 30 * time.Second

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

// Recordings — восстановление имени файла записи и ссылки на неё.
type Recordings interface {
	// StartFromUniqueid — время начала записи, восстановленное из Uniqueid.
	StartFromUniqueid(uniqueid string) (time.Time, bool)
	// URLFor — публичная ссылка на запись.
	URLFor(uniqueid, callerID string, start time.Time) string
}

// Config — параметры финализатора.
type Config struct {
	// Employees — точечные переопределения: peer → внутренний номер Okdesk.
	Employees map[string]int
	// TelephonyNumber — общий внутренний номер Okdesk (override). Вместе с
	// Employees используется для определения receiver_phone ответившего оператора.
	TelephonyNumber int
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

	wg sync.WaitGroup

	callsLogged atomic.Int64
}

// New создаёт финализатор. client и rec обязательны.
func New(st Store, client CallClient, rec Recordings, cfg Config, logger *slog.Logger) *Finalizer {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.FinalizeTimeout <= 0 {
		cfg.FinalizeTimeout = defaultFinalizeTimeout
	}
	f := &Finalizer{
		store:  st,
		client: client,
		rec:    rec,
		cfg:    cfg,
		log:    logger,
		now:    time.Now,
	}
	if cfg.AutoLinkIssue {
		f.log.Info("автопривязка заявок включена")
	} else {
		f.log.Info("автопривязка заявок выключена: связь с заявками устанавливает сам Okdesk")
	}
	return f
}

// CallsLogged возвращает число успешно зажурналированных звонков.
func (j *Finalizer) CallsLogged() int64 { return j.callsLogged.Load() }

// OnHangup завершает известный звонок: фиксирует время отбоя и запускает
// журналирование. Звонки, отсутствующие в корреляции (например, канал
// оператора с другим Uniqueid), игнорируются.
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

	finishedAt := j.now()
	if err := j.store.MarkFinished(ctx, uniqueid, finishedAt); err != nil && !errors.Is(err, store.ErrNotFound) {
		j.log.Warn("Hangup: отметка завершения", "uniqueid", uniqueid, "error", err)
	}
	j.log.Info("Hangup: журналирование звонка",
		"uniqueid", uniqueid, "caller", call.CallerIDNum)
	j.startFinalize(uniqueid)
	return nil
}

// startFinalize запускает обработку завершённого звонка в отдельной горутине,
// чтобы не задерживать поток событий AMI сетевыми вызовами.
func (j *Finalizer) startFinalize(uniqueid string) {
	j.wg.Add(1)
	go func() {
		defer j.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), j.cfg.FinalizeTimeout)
		defer cancel()
		j.finalize(ctx, uniqueid)
	}()
}

// finalize собирает и отправляет запись о звонке.
func (j *Finalizer) finalize(ctx context.Context, uniqueid string) {
	call, err := j.store.GetCall(ctx, uniqueid)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		j.log.Error("журналирование: чтение звонка", "uniqueid", uniqueid, "error", err)
		return
	}

	// Звонки, не принятые оператором (брошенные), в Okdesk не отправляются:
	// в записи о звонке нечем заполнить receiver_phone (обязательное поле).
	if call.Status != store.StatusAnswered {
		j.log.Info("журналирование: звонок не принят оператором, запись пропущена",
			"uniqueid", uniqueid, "status", call.Status)
		j.cleanupCall(ctx, uniqueid)
		return
	}

	// receiver_phone = внутренний номер ответившего оператора: по нему Okdesk
	// определяет сотрудника в списке звонков. Порядок тот же, что и для
	// screen-pop (employees → override → числовое имя peer).
	number, _, ok := callflow.ResolveTelephonyNumber(call.AgentPeer, j.cfg.Employees, j.cfg.TelephonyNumber)
	if !ok {
		j.log.Warn("журналирование: не удалось определить внутренний номер ответившего оператора, запись пропущена",
			"uniqueid", uniqueid, "peer", call.AgentPeer)
		j.cleanupCall(ctx, uniqueid)
		return
	}
	receiver := strconv.Itoa(number)

	startedAt, finishedAt, duration := j.timing(call)

	fileURL := ""
	if j.rec != nil {
		// Минута в имени файла — время создания канала (epoch в Uniqueid);
		// запасной вариант — время постановки звонка в очередь.
		fileStart := call.CreatedAt
		if t, ok := j.rec.StartFromUniqueid(uniqueid); ok {
			fileStart = t
		}
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
		ReceiverPhone: receiver,
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
			"uniqueid", uniqueid, "started_at", startedAt, "finished_at", finishedAt,
			"duration", duration, "issue_id", issueID, "file_url", fileURL)
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

// timing вычисляет времена и длительность разговора по данным корреляции:
// начало — ответ оператора (AgentConnect), окончание — отбой (Hangup).
func (j *Finalizer) timing(call store.Call) (startedAt, finishedAt time.Time, duration int) {
	startedAt = call.CreatedAt
	if call.AnsweredAt != nil {
		startedAt = *call.AnsweredAt
	}

	finishedAt = j.now()
	if call.FinishedAt != nil {
		finishedAt = *call.FinishedAt
	}
	if finishedAt.Before(startedAt) {
		finishedAt = startedAt
	}

	// Длительность = разговор от ответа оператора до отбоя: совпадает с длиной
	// записи. Округляем до секунды (AnsweredAt хранится с долями секунды) и
	// держим минимум 1 с: Okdesk отвергает duration = 0.
	duration = int(math.Round(finishedAt.Sub(startedAt).Seconds()))
	if duration < 1 {
		duration = 1
	}
	return startedAt, finishedAt, duration
}

// Wait ожидает завершения обработки всех начатых звонков.
func (j *Finalizer) Wait() { j.wg.Wait() }

// Close дожидается обработки начатых звонков.
func (j *Finalizer) Close() { j.wg.Wait() }
