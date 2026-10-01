package ami

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// readAction читает одно действие AMI (до пустой строки).
func readAction(br *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return b.String(), err
		}
		b.WriteString(line)
		if strings.TrimRight(line, "\r\n") == "" {
			return b.String(), nil
		}
	}
}

func writeSuccessLogin(w io.Writer) error {
	_, err := io.WriteString(w, "Response: Success\r\nMessage: Authentication accepted\r\n\r\n")
	return err
}

func writeEvent(w io.Writer, name, uniqueid string) error {
	_, err := io.WriteString(w, "Event: "+name+"\r\nUniqueid: "+uniqueid+"\r\n\r\n")
	return err
}

func newTestClient(addr string) *Client {
	return New(Options{
		Address:      addr,
		Username:     "test",
		Secret:       "secret",
		DialTimeout:  2 * time.Second,
		ReconnectMin: 10 * time.Millisecond,
		ReconnectMax: 50 * time.Millisecond,
		Logger:       silentLogger(),
	})
}

func awaitEvent(t *testing.T, c *Client, want string) Frame {
	t.Helper()
	select {
	case f := <-c.Events():
		if f.EventName() != want {
			t.Fatalf("получено событие %q, ожидалось %q", f.EventName(), want)
		}
		return f
	case <-time.After(3 * time.Second):
		t.Fatalf("не дождались события %q", want)
		return Frame{}
	}
}

func TestClientLoginAndEvent(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("не удалось открыть слушатель: %v", err)
	}
	defer ln.Close()

	loginSeen := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		action, err := readAction(br)
		if err == nil {
			loginSeen <- action
		}
		if writeSuccessLogin(conn) != nil {
			return
		}
		if writeEvent(conn, "QueueCallerJoin", "1700000000.1") != nil {
			return
		}
		// Держим соединение открытым, пока клиент не завершится.
		time.Sleep(2 * time.Second)
	}()

	c := newTestClient(ln.Addr().String())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	f := awaitEvent(t, c, "QueueCallerJoin")
	if got := f.Get("Uniqueid"); got != "1700000000.1" {
		t.Errorf("Uniqueid = %q, ожидалось 1700000000.1", got)
	}

	if action := <-loginSeen; !strings.Contains(action, "Action: Login") ||
		!strings.Contains(action, "Username: test") ||
		!strings.Contains(action, "Events: on") {
		t.Errorf("действие Login некорректно:\n%s", action)
	}

	if st := c.Stats(); !st.Connected || st.FramesReceived == 0 || st.LastEventAt.IsZero() {
		t.Errorf("Stats = %+v, ожидалось подключённое состояние с полученным событием", st)
	}

	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("Run вернул ошибку после отмены: %v", err)
	}
}

func TestClientReconnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("не удалось открыть слушатель: %v", err)
	}
	defer ln.Close()

	var mu sync.Mutex
	connections := 0

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			connections++
			n := connections
			mu.Unlock()

			go func(n int) {
				defer conn.Close()
				br := bufio.NewReader(conn)
				if _, err := readAction(br); err != nil {
					return
				}
				if writeSuccessLogin(conn) != nil {
					return
				}
				if writeEvent(conn, "AgentCalled", "1700000000."+string(rune('0'+n))) != nil {
					return
				}
				// Первое соединение обрываем, чтобы вызвать переподключение.
				if n == 1 {
					return
				}
				time.Sleep(2 * time.Second)
			}(n)
		}
	}()

	c := newTestClient(ln.Addr().String())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	awaitEvent(t, c, "AgentCalled") // первое соединение
	awaitEvent(t, c, "AgentCalled") // после переподключения

	if st := c.Stats(); st.Reconnects < 1 {
		t.Errorf("Reconnects = %d, ожидалось >= 1", st.Reconnects)
	}
}

func TestClientLoginRejectedRetries(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("не удалось открыть слушатель: %v", err)
	}
	defer ln.Close()

	attempts := make(chan struct{}, 16)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				br := bufio.NewReader(conn)
				if _, err := readAction(br); err != nil {
					return
				}
				attempts <- struct{}{}
				_, _ = io.WriteString(conn, "Response: Error\r\nMessage: Authentication failed\r\n\r\n")
			}()
		}
	}()

	c := newTestClient(ln.Addr().String())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	for i := 0; i < 3; i++ {
		select {
		case <-attempts:
		case <-time.After(3 * time.Second):
			t.Fatalf("ожидалась попытка аутентификации №%d", i+1)
		}
	}

	if st := c.Stats(); st.Connected {
		t.Error("Connected = true, хотя Login отклонён")
	}
	select {
	case f := <-c.Events():
		t.Fatalf("получено событие %q при отклонённом Login", f.EventName())
	case <-time.After(100 * time.Millisecond):
	}
}

func TestClientContextCancellationStopsRun(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("не удалось открыть слушатель: %v", err)
	}
	defer ln.Close()

	release := make(chan struct{})
	defer close(release)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		_, _ = readAction(br)
		_ = writeSuccessLogin(conn)
		<-release
	}()

	c := newTestClient(ln.Addr().String())
	ctx, cancel := context.WithCancel(context.Background())

	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	awaitConnected(t, c)

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run вернул ошибку после отмены: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run не завершился после отмены контекста")
	}
}

func awaitConnected(t *testing.T, c *Client) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.Stats().Connected {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("клиент не подключился к AMI")
}
