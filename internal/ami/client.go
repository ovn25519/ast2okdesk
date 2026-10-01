package ami

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

const (
	defaultDialTimeout  = 5 * time.Second
	defaultReconnectMin = 1 * time.Second
	defaultReconnectMax = 30 * time.Second
	defaultEventsBuffer = 1024
	loginActionID       = "ast2okdesk-login"
	readBufferSize      = 64 * 1024
	stableSessionTime   = 30 * time.Second
)

// DialFunc — функция установки TCP-соединения (подменяется в тестах).
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Options — параметры подключения к AMI.
type Options struct {
	Address  string // host:port
	Username string
	Secret   string

	DialTimeout  time.Duration // таймаут установления соединения и ответа на Login
	ReconnectMin time.Duration // начальная пауза перед переподключением
	ReconnectMax time.Duration // максимальная пауза перед переподключением
	EventsBuffer int           // размер буфера канала событий

	Logger *slog.Logger
	Dialer DialFunc
}

func (o Options) withDefaults() Options {
	if o.DialTimeout <= 0 {
		o.DialTimeout = defaultDialTimeout
	}
	if o.ReconnectMin <= 0 {
		o.ReconnectMin = defaultReconnectMin
	}
	if o.ReconnectMax < o.ReconnectMin {
		o.ReconnectMax = o.ReconnectMin
	}
	if o.EventsBuffer <= 0 {
		o.EventsBuffer = defaultEventsBuffer
	}
	if o.Dialer == nil {
		d := &net.Dialer{Timeout: o.DialTimeout}
		o.Dialer = d.DialContext
	}
	return o
}

// Stats — снимок состояния клиента для мониторинга.
type Stats struct {
	Connected      bool
	Reconnects     int64
	FramesReceived int64
	LastEventAt    time.Time
}

// Client — клиент AMI. Одно соединение обслуживает одна горутина Run,
// события публикуются в канал Events.
type Client struct {
	opts   Options
	events chan Frame

	connected  atomic.Bool
	reconnects atomic.Int64
	frames     atomic.Int64
	lastEvent  atomic.Int64 // UnixNano
}

// New создаёт клиент AMI, применяя значения по умолчанию.
func New(opts Options) *Client {
	opts = opts.withDefaults()
	return &Client{
		opts:   opts,
		events: make(chan Frame, opts.EventsBuffer),
	}
}

// Events возвращает канал событий AMI. Канал закрывается не клиентом,
// поэтому потребитель должен завершать работу при отмене контекста Run.
func (c *Client) Events() <-chan Frame { return c.events }

// Stats возвращает снимок состояния клиента.
func (c *Client) Stats() Stats {
	s := Stats{
		Connected:      c.connected.Load(),
		Reconnects:     c.reconnects.Load(),
		FramesReceived: c.frames.Load(),
	}
	if ns := c.lastEvent.Load(); ns > 0 {
		s.LastEventAt = time.Unix(0, ns)
	}
	return s
}

// Run подключается к AMI и читает события, переподключаясь при разрывах,
// до отмены контекста. Возвращает nil при штатном завершении по контексту.
func (c *Client) Run(ctx context.Context) error {
	backoff := c.opts.ReconnectMin
	for {
		if ctx.Err() != nil {
			return nil
		}

		start := time.Now()
		err := c.session(ctx)
		if ctx.Err() != nil {
			return nil
		}

		c.reconnects.Add(1)
		if time.Since(start) >= stableSessionTime {
			backoff = c.opts.ReconnectMin
		}
		c.log().Warn("AMI: сессия прервана, повторное подключение",
			"error", err, "backoff", backoff)

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}

		if backoff *= 2; backoff > c.opts.ReconnectMax {
			backoff = c.opts.ReconnectMax
		}
	}
}

// session обслуживает одно соединение: подключение, Login, чтение событий.
func (c *Client) session(ctx context.Context) error {
	conn, err := c.opts.Dialer(ctx, "tcp", c.opts.Address)
	if err != nil {
		return fmt.Errorf("подключение к AMI %s: %w", c.opts.Address, err)
	}
	defer conn.Close()

	// Закрываем соединение при отмене контекста, разблокируя чтение.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	br := bufio.NewReaderSize(conn, readBufferSize)
	if err := c.login(conn, br); err != nil {
		return err
	}

	c.connected.Store(true)
	defer c.connected.Store(false)
	c.log().Info("AMI: аутентификация успешна")
	return c.readLoop(ctx, conn, br)
}

// login отправляет действие Login и ожидает ответ сервера.
func (c *Client) login(conn net.Conn, br *bufio.Reader) error {
	var req strings.Builder
	req.WriteString("Action: Login\r\n")
	req.WriteString("ActionID: " + loginActionID + "\r\n")
	req.WriteString("Username: " + c.opts.Username + "\r\n")
	req.WriteString("Secret: " + c.opts.Secret + "\r\n")
	req.WriteString("Events: on\r\n\r\n")

	_ = conn.SetWriteDeadline(time.Now().Add(c.opts.DialTimeout))
	if _, err := io.WriteString(conn, req.String()); err != nil {
		return fmt.Errorf("отправка Login: %w", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(c.opts.DialTimeout))
	defer conn.SetReadDeadline(time.Time{})

	for {
		frame, err := readFrame(br)
		if err != nil {
			return fmt.Errorf("чтение ответа Login: %w", err)
		}
		if frame.Response() == "" {
			continue // событие, пришедшее до ответа — пропускаем
		}
		if id := frame.Get("ActionID"); id != "" && id != loginActionID {
			continue
		}
		if !strings.EqualFold(frame.Response(), "Success") {
			return fmt.Errorf("AMI отклонил Login: %s", frame.Get("Message"))
		}
		return nil
	}
}

// readLoop читает события и публикует их в канал Events.
func (c *Client) readLoop(ctx context.Context, conn net.Conn, br *bufio.Reader) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		frame, err := readFrame(br)
		if err != nil {
			return fmt.Errorf("чтение AMI: %w", err)
		}
		if !frame.IsEvent() {
			continue // ответы на действия нам не нужны
		}

		c.frames.Add(1)
		c.lastEvent.Store(time.Now().UnixNano())

		select {
		case c.events <- frame:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c *Client) log() *slog.Logger {
	if c.opts.Logger != nil {
		return c.opts.Logger
	}
	return slog.Default()
}
