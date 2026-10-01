package ami

import (
	"bufio"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func frameFrom(t *testing.T, raw string) (Frame, error) {
	t.Helper()
	return readFrame(bufio.NewReaderSize(strings.NewReader(raw), 4096))
}

func TestReadFrameBasic(t *testing.T) {
	f, err := frameFrom(t, "Event: QueueCallerJoin\r\nQueue: support\r\nUniqueid: 1700000000.42\r\n\r\n")
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if got := f.EventName(); got != "QueueCallerJoin" {
		t.Errorf("EventName = %q, ожидалось QueueCallerJoin", got)
	}
	if got := f.Get("Queue"); got != "support" {
		t.Errorf("Queue = %q, ожидалось support", got)
	}
	if !f.IsEvent() {
		t.Error("IsEvent = false, ожидалось true")
	}
}

func TestReadFrameCaseInsensitive(t *testing.T) {
	// Cdr использует «UniqueID», события очереди — «Uniqueid».
	f, err := frameFrom(t, "Event: Cdr\r\nUniqueID: 1700000000.42\r\n\r\n")
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if got := f.Get("Uniqueid"); got != "1700000000.42" {
		t.Errorf("Get(\"Uniqueid\") = %q, ожидалось 1700000000.42", got)
	}
	if got := f.Get("UNIQUEID"); got != "1700000000.42" {
		t.Errorf("Get(\"UNIQUEID\") = %q, ожидалось 1700000000.42", got)
	}
}

func TestReadFrameSkipsBlankAndGarbageLines(t *testing.T) {
	raw := "\r\n\r\nмусор без разделителя\r\nEvent: AgentCalled\r\n\r\n"
	f, err := frameFrom(t, raw)
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if got := f.EventName(); got != "AgentCalled" {
		t.Errorf("EventName = %q, ожидалось AgentCalled", got)
	}
}

func TestReadFramePreservesValueWithColon(t *testing.T) {
	f, err := frameFrom(t, "Event: AgentCalled\r\nDestChannel: SIP/101-0000000a\r\nVariable: a:b:c\r\n\r\n")
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if got := f.Get("Variable"); got != "a:b:c" {
		t.Errorf("Variable = %q, ожидалось a:b:c", got)
	}
}

func TestReadFrameSequence(t *testing.T) {
	raw := "Event: QueueCallerJoin\r\nUniqueid: 1\r\n\r\nEvent: Hangup\r\nUniqueid: 1\r\n\r\n"
	br := bufio.NewReaderSize(strings.NewReader(raw), 4096)

	first, err := readFrame(br)
	if err != nil {
		t.Fatalf("первый кадр: %v", err)
	}
	second, err := readFrame(br)
	if err != nil {
		t.Fatalf("второй кадр: %v", err)
	}
	if first.EventName() != "QueueCallerJoin" || second.EventName() != "Hangup" {
		t.Fatalf("получены события %q и %q", first.EventName(), second.EventName())
	}

	if _, err := readFrame(br); !errors.Is(err, io.EOF) {
		t.Fatalf("после конца потока ожидался io.EOF, получено %v", err)
	}
}

func TestReadFrameEOFWithoutData(t *testing.T) {
	if _, err := frameFrom(t, ""); !errors.Is(err, io.EOF) {
		t.Fatalf("ожидался io.EOF, получено %v", err)
	}
}

func TestFrameResponseAndHeaders(t *testing.T) {
	f, err := frameFrom(t, "Response: Success\r\nMessage: Authentication accepted\r\n\r\n")
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if f.IsEvent() {
		t.Error("IsEvent = true для ответа на действие")
	}
	if got := f.Response(); got != "Success" {
		t.Errorf("Response = %q, ожидалось Success", got)
	}

	headers := f.Headers()
	want := map[string]string{"Response": "Success", "Message": "Authentication accepted"}
	if !reflect.DeepEqual(headers, want) {
		t.Errorf("Headers = %v, ожидалось %v", headers, want)
	}

	// Копия не должна влиять на исходный кадр.
	headers["Response"] = "Error"
	if got := f.Response(); got != "Success" {
		t.Errorf("изменение копии затронуло кадр: Response = %q", got)
	}
}
