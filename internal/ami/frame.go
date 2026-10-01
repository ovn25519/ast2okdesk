// Package ami реализует минимальный клиент Asterisk Manager Interface (AMI):
// подключение по TCP, аутентификацию, реконнект с экспоненциальной задержкой
// и разбор событий. Клиент работает в режиме read-only и не отправляет
// управляющих действий (кроме Login), поэтому не может повлиять на работу АТС.
package ami

import (
	"bufio"
	"strings"
)

// Frame — одно сообщение AMI, состоящее из набора заголовков «Key: Value».
//
// AMI не гарантирует единый регистр имён заголовков: например, события очереди
// используют «Uniqueid», а событие Cdr — «UniqueID». Доступ через Get выполняется
// без учёта регистра, чтобы не зависеть от этой особенности.
type Frame struct {
	headers map[string]string // заголовки с исходными ключами
	lower   map[string]string // те же значения, ключи приведены к нижнему регистру
}

func newFrame() Frame {
	return Frame{
		headers: make(map[string]string, 8),
		lower:   make(map[string]string, 8),
	}
}

// NewFrame создаёт кадр из набора заголовков. Предназначен для построения
// синтетических событий, в том числе в тестах.
func NewFrame(headers map[string]string) Frame {
	f := newFrame()
	for k, v := range headers {
		f.headers[k] = v
		f.lower[strings.ToLower(k)] = v
	}
	return f
}

// Get возвращает значение заголовка name без учёта регистра.
func (f Frame) Get(name string) string {
	return f.lower[strings.ToLower(name)]
}

// EventName возвращает имя события (заголовок Event) или пустую строку,
// если кадр не является событием.
func (f Frame) EventName() string { return f.Get("Event") }

// IsEvent сообщает, является ли кадр событием (а не ответом на действие).
func (f Frame) IsEvent() bool { return f.EventName() != "" }

// Response возвращает заголовок Response (Success, Error или Follows).
func (f Frame) Response() string { return f.Get("Response") }

// Headers возвращает копию заголовков кадра с исходными ключами.
func (f Frame) Headers() map[string]string {
	out := make(map[string]string, len(f.headers))
	for k, v := range f.headers {
		out[k] = v
	}
	return out
}

// readFrame читает один кадр AMI из потока. Кадр завершается пустой строкой;
// строки между кадрами и строки без разделителя «:» игнорируются.
//
// При закрытии потока читаемые данные (если они есть) возвращаются вместе с
// ошибкой, чтобы вызывающая сторона могла отреагировать на разрыв соединения.
func readFrame(r *bufio.Reader) (Frame, error) {
	f := newFrame()
	for {
		line, err := r.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")

		if line == "" {
			if err != nil {
				if len(f.headers) > 0 {
					return f, err
				}
				return Frame{}, err
			}
			// Пустые строки между кадрами.
			if len(f.headers) == 0 {
				continue
			}
			return f, nil
		}

		if key, value, ok := strings.Cut(line, ":"); ok {
			if key = strings.TrimSpace(key); key != "" {
				value = strings.TrimSpace(value)
				f.headers[key] = value
				f.lower[strings.ToLower(key)] = value
			}
		}

		if err != nil {
			// Последняя строка без завершающего перевода строки.
			if len(f.headers) > 0 {
				return f, err
			}
			return Frame{}, err
		}
	}
}
