// Package callflow реализует диспетчер событий AMI и корреляцию звонка по
// Uniqueid: постановку в очередь, screen-pop оператора, ответ и отказ абонента.
//
// События Hangup и Cdr делегируются финализатору (пакет journal), который
// дожидается Cdr и журналирует разговор в Okdesk. Здесь обрабатываются
// QueueCallerJoin, AgentCalled, AgentConnect и QueueCallerAbandon.
package callflow

import (
	"context"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ovn25519/ast2okdesk/internal/ami"
	"github.com/ovn25519/ast2okdesk/internal/store"
)

// Store — подмножество хранилища, необходимое диспетчеру.
type Store interface {
	SaveCallQueued(ctx context.Context, call store.Call) error
	ClaimDedup(ctx context.Context, uniqueid, peer string) (bool, error)
	MarkAnswered(ctx context.Context, uniqueid, peer string, at time.Time) error
	MarkAbandoned(ctx context.Context, uniqueid string, at time.Time) error
	GetCall(ctx context.Context, uniqueid string) (store.Call, error)
}

// ScreenPopper — источник screen-pop в Okdesk (POST /api/v1/telephony/messages).
type ScreenPopper interface {
	ScreenPop(ctx context.Context, phone string, telephonyNumber int) error
}

// Finalizer обрабатывает завершение звонка: ожидание Cdr и журналирование.
// Реализуется пакетом journal.
type Finalizer interface {
	// OnHangup запускает ожидание Cdr для известного звонка.
	OnHangup(ctx context.Context, f ami.Frame) error
	// OnCdr принимает событие Cdr.
	OnCdr(ctx context.Context, f ami.Frame)
}

// Config — параметры диспетчера.
type Config struct {
	// Queue — имя мониторируемой очереди.
	Queue string
	// TelephonyNumber — запасной внутренний номер Okdesk, если имя peer
	// нечисловое и отсутствует в Employees. Может быть 0.
	TelephonyNumber int
	// Employees — необязательные переопределения: peer → внутренний номер
	// Okdesk. Имеют приоритет над номером, извлечённым из имени peer.
	Employees map[string]int
}

// Dispatcher обрабатывает события AMI.
type Dispatcher struct {
	store     Store
	pop       ScreenPopper
	finalizer Finalizer
	cfg       Config
	log       *slog.Logger
	now       func() time.Time

	wg sync.WaitGroup
}

// New создаёт диспетчер. Если logger равен nil, используется slog.Default.
func New(st Store, pop ScreenPopper, cfg Config, logger *slog.Logger) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{
		store: st,
		pop:   pop,
		cfg:   cfg,
		log:   logger,
		now:   time.Now,
	}
}

// SetFinalizer назначает обработчик завершения звонка (Hangup/Cdr).
func (d *Dispatcher) SetFinalizer(f Finalizer) { d.finalizer = f }

// Wait ожидает завершения фоновых операций screen-pop.
func (d *Dispatcher) Wait() { d.wg.Wait() }

// Run читает события ami.Events и обрабатывает их до отмены контекста или
// закрытия канала. Ошибки отдельных событий логируются и не прерывают цикл.
func (d *Dispatcher) Run(ctx context.Context, events <-chan ami.Frame) error {
	defer d.wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return nil
		case f, ok := <-events:
			if !ok {
				return nil
			}
			if err := d.Handle(ctx, f); err != nil {
				d.log.Error("обработка события AMI",
					"event", f.EventName(), "uniqueid", f.Get("Uniqueid"), "error", err)
			}
		}
	}
}

// Handle обрабатывает одно событие AMI. Неизвестные события игнорируются.
func (d *Dispatcher) Handle(ctx context.Context, f ami.Frame) error {
	switch f.EventName() {
	case "QueueCallerJoin":
		return d.handleQueueCallerJoin(ctx, f)
	case "AgentCalled":
		return d.handleAgentCalled(ctx, f)
	case "AgentConnect":
		return d.handleAgentConnect(ctx, f)
	case "QueueCallerAbandon":
		return d.handleQueueCallerAbandon(ctx, f)
	case "Hangup":
		if d.finalizer == nil {
			return nil
		}
		return d.finalizer.OnHangup(ctx, f)
	case "Cdr":
		if d.finalizer == nil {
			return nil
		}
		d.finalizer.OnCdr(ctx, f)
		return nil
	default:
		return nil
	}
}

// handleQueueCallerJoin фиксирует постановку абонента в очередь.
func (d *Dispatcher) handleQueueCallerJoin(ctx context.Context, f ami.Frame) error {
	if !d.ourQueue(f) {
		return nil
	}
	uniqueid := f.Get("Uniqueid")
	if uniqueid == "" {
		return nil
	}

	linkedid := f.Get("Linkedid")
	if linkedid == "" {
		linkedid = uniqueid
	}

	now := d.now()
	call := store.Call{
		Uniqueid:    uniqueid,
		Linkedid:    linkedid,
		CallerIDNum: f.Get("CallerIDNum"),
		Queue:       f.Get("Queue"),
		Status:      store.StatusQueued,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := d.store.SaveCallQueued(ctx, call); err != nil {
		return err
	}
	d.log.Info("звонок поступил в очередь",
		"uniqueid", uniqueid, "linkedid", linkedid,
		"caller", call.CallerIDNum, "queue", call.Queue)
	return nil
}

// handleAgentCalled определяет оператора и запускает screen-pop.
func (d *Dispatcher) handleAgentCalled(ctx context.Context, f ami.Frame) error {
	if !d.ourQueue(f) {
		return nil
	}
	uniqueid := f.Get("Uniqueid")
	if uniqueid == "" {
		return nil
	}

	peer, ok := ParsePeer(f.Get("DestChannel"))
	if !ok {
		d.log.Warn("AgentCalled: не удалось определить оператора",
			"uniqueid", uniqueid, "dest_channel", f.Get("DestChannel"))
		return nil
	}

	number, source, ok := d.telephonyNumber(peer)
	if !ok {
		d.log.Warn("AgentCalled: не удалось определить внутренний номер Okdesk, screen-pop пропущен "+
			"(проверьте, что внутренний номер оператора в Okdesk совпадает с именем peer)",
			"uniqueid", uniqueid, "peer", peer)
		return nil
	}

	// Дедупликация: при ringall каждый оператор получает своё событие, но
	// повторный AgentCalled для той же пары (звонок, оператор) пропускается.
	first, err := d.store.ClaimDedup(ctx, uniqueid, peer)
	if err != nil {
		return err
	}
	if !first {
		d.log.Debug("AgentCalled: screen-pop уже отправлен",
			"uniqueid", uniqueid, "peer", peer)
		return nil
	}

	phone := f.Get("CallerIDNum")
	d.log.Info("screen-pop оператора",
		"uniqueid", uniqueid, "peer", peer, "phone", phone,
		"telephony_number", number, "number_source", source)

	// Screen-pop выполняется асинхронно, чтобы не блокировать обработку
	// остальных событий AMI при недоступности Okdesk. Повторов нет — только лог.
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		if err := d.pop.ScreenPop(ctx, phone, number); err != nil {
			d.log.Warn("screen-pop не удался",
				"uniqueid", uniqueid, "peer", peer, "phone", phone, "error", err)
		}
	}()
	return nil
}

// handleAgentConnect отмечает ответ оператора.
func (d *Dispatcher) handleAgentConnect(ctx context.Context, f ami.Frame) error {
	if !d.ourQueue(f) {
		return nil
	}
	uniqueid := f.Get("Uniqueid")
	if uniqueid == "" {
		return nil
	}

	peer, _ := ParsePeer(f.Get("DestChannel"))
	if err := d.store.MarkAnswered(ctx, uniqueid, peer, d.now()); err != nil {
		if err == store.ErrNotFound {
			d.log.Debug("AgentConnect: звонок не найден", "uniqueid", uniqueid)
			return nil
		}
		return err
	}
	d.log.Info("оператор ответил", "uniqueid", uniqueid, "peer", peer)
	return nil
}

// handleQueueCallerAbandon отмечает отказ абонента до ответа.
func (d *Dispatcher) handleQueueCallerAbandon(ctx context.Context, f ami.Frame) error {
	if !d.ourQueue(f) {
		return nil
	}
	uniqueid := f.Get("Uniqueid")
	if uniqueid == "" {
		return nil
	}
	if err := d.store.MarkAbandoned(ctx, uniqueid, d.now()); err != nil {
		if err == store.ErrNotFound {
			d.log.Debug("QueueCallerAbandon: звонок не найден", "uniqueid", uniqueid)
			return nil
		}
		return err
	}
	d.log.Info("абонент прервал ожидание", "uniqueid", uniqueid)
	return nil
}

// ourQueue сообщает, относится ли событие к мониторируемой очереди.
func (d *Dispatcher) ourQueue(f ami.Frame) bool {
	return f.Get("Queue") == d.cfg.Queue
}

// telephonyNumber возвращает внутренний номер Okdesk для оператора peer, а также
// источник, откуда он взялся: «employees», «peer» или «fallback».
//
// Порядок: явное переопределение в [[employees]] → цифровое имя peer (основной
// путь: оператор сам указывает свой внутренний номер в профиле Okdesk) →
// okdesk.telephony_number (запасной вариант для нечисловых имён peer).
func (d *Dispatcher) telephonyNumber(peer string) (number int, source string, ok bool) {
	if n, found := d.cfg.Employees[peer]; found && n > 0 {
		return n, "employees", true
	}
	if n, err := strconv.Atoi(peer); err == nil && n > 0 {
		return n, "peer", true
	}
	if d.cfg.TelephonyNumber > 0 {
		return d.cfg.TelephonyNumber, "fallback", true
	}
	return 0, "", false
}

// destChannelRe извлекает имя SIP/PJSIP-peer из канала, например «SIP/101-0000000a»
// или «PJSIP/101-0000000a». Жадный класс символов с откатом делит имя peer и
// уникальный суффикс канала по последнему дефису, поэтому имена peer могут
// содержать «.», «_» и «-».
var destChannelRe = regexp.MustCompile(`^(?:SIP|PJSIP)/([A-Za-z0-9_.-]+)-`)

// ParsePeer извлекает оператора из значения DestChannel (каналы SIP и PJSIP).
func ParsePeer(destChannel string) (string, bool) {
	m := destChannelRe.FindStringSubmatch(strings.TrimSpace(destChannel))
	if m == nil {
		return "", false
	}
	return m[1], true
}
