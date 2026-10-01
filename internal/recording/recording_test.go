package recording

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("не удалось загрузить часовой пояс %q: %v", name, err)
	}
	return loc
}

func TestFileName(t *testing.T) {
	loc := mustLoad(t, "Asia/Yekaterinburg")
	l := New("https://calls.example.ru:8443/records", loc)

	start := time.Date(2026, 9, 30, 9, 16, 42, 0, loc)
	got := l.FileName("1700000000.42", "+79991234567", start)
	want := "1700000000.42-2026-09-30-09_16-+79991234567-s.mp3"
	if got != want {
		t.Errorf("FileName = %q, ожидалось %q", got, want)
	}
}

func TestFileNameConvertsToAsteriskTimezone(t *testing.T) {
	loc := mustLoad(t, "Asia/Yekaterinburg") // UTC+5
	l := New("https://calls.example.ru:8443/records/", loc)

	// 04:16 UTC == 09:16 в Екатеринбурге.
	start := time.Date(2026, 9, 30, 4, 16, 0, 0, time.UTC)
	name := l.FileName("u1", "7999", start)
	if !strings.Contains(name, "-2026-09-30-09_16-") {
		t.Errorf("имя файла %q не содержит минуту в часовом поясе Asterisk", name)
	}
}

func TestFileNameMinuteBoundaries(t *testing.T) {
	loc := mustLoad(t, "Asia/Yekaterinburg")
	l := New("https://example.test/", loc)

	cases := []struct {
		name string
		t    time.Time
		want string
	}{
		{"конец минуты", time.Date(2026, 9, 30, 9, 16, 59, 0, loc), "u-2026-09-30-09_16-c-s.mp3"},
		{"начало следующей минуты", time.Date(2026, 9, 30, 9, 17, 0, 0, loc), "u-2026-09-30-09_17-c-s.mp3"},
		{"конец часа", time.Date(2026, 9, 30, 9, 59, 59, 0, loc), "u-2026-09-30-09_59-c-s.mp3"},
		{"начало следующего часа", time.Date(2026, 9, 30, 10, 0, 0, 0, loc), "u-2026-09-30-10_00-c-s.mp3"},
		{"конец суток", time.Date(2026, 9, 30, 23, 59, 59, 0, loc), "u-2026-09-30-23_59-c-s.mp3"},
		{"начало следующих суток", time.Date(2026, 10, 1, 0, 0, 0, 0, loc), "u-2026-10-01-00_00-c-s.mp3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := l.FileName("u", "c", c.t); got != c.want {
				t.Errorf("FileName = %q, ожидалось %q", got, c.want)
			}
		})
	}
}

func TestURLNormalizesTrailingSlash(t *testing.T) {
	loc := time.UTC

	with := New("https://calls.example.ru:8443/records", loc)
	without := New("https://calls.example.ru:8443/records/", loc)

	const file = "a-s.mp3"
	if got := with.URL(file); got != "https://calls.example.ru:8443/records/a-s.mp3" {
		t.Errorf("URL = %q", got)
	}
	if with.URL(file) != without.URL(file) {
		t.Errorf("результат зависит от завершающего «/»: %q != %q", with.URL(file), without.URL(file))
	}
}

func TestURLFor(t *testing.T) {
	loc := mustLoad(t, "Asia/Yekaterinburg")
	l := New("https://calls.example.ru:8443/records/", loc)
	start := time.Date(2026, 9, 30, 9, 16, 0, 0, loc)

	got := l.URLFor("1700000000.42", "+79991234567", start)
	want := "https://calls.example.ru:8443/records/1700000000.42-2026-09-30-09_16-+79991234567-s.mp3"
	if got != want {
		t.Errorf("URLFor = %q, ожидалось %q", got, want)
	}
}

func TestStartFromUniqueid(t *testing.T) {
	loc := mustLoad(t, "Asia/Yekaterinburg")
	l := New("https://example.test/", loc)

	want := time.Date(2026, 9, 30, 9, 16, 42, 0, loc)
	got, ok := l.StartFromUniqueid(fmt.Sprintf("%d.42", want.Unix()))
	if !ok {
		t.Fatal("StartFromUniqueid не распознал метку времени")
	}
	if !got.Equal(want) {
		t.Errorf("StartFromUniqueid = %v, ожидалось %v", got, want)
	}
	if _, off := got.Zone(); off != 5*3600 {
		t.Errorf("смещение зоны = %d, ожидалось 18000", off)
	}

	for _, bad := range []string{"", "u1", "abc.42", "0.0", "-5.42", ".42"} {
		if _, ok := l.StartFromUniqueid(bad); ok {
			t.Errorf("StartFromUniqueid(%q) не должен распознаваться", bad)
		}
	}
}

// TestURLForRealCall воспроизводит реальный звонок: минута имени файла берётся
// из Uniqueid, а не из отдельного события Cdr.
func TestURLForRealCall(t *testing.T) {
	loc := mustLoad(t, "Asia/Yekaterinburg") // UTC+5
	l := New("https://call.prosche.su:8443/records/", loc)

	const uid = "1790870838.5482" // 2026-10-01 21:07:18 +05
	start, ok := l.StartFromUniqueid(uid)
	if !ok {
		t.Fatal("StartFromUniqueid не распознал Uniqueid")
	}
	got := l.URLFor(uid, "+79655599888", start)
	want := "https://call.prosche.su:8443/records/1790870838.5482-2026-10-01-21_07-+79655599888-s.mp3"
	if got != want {
		t.Errorf("URLFor = %q, ожидалось %q", got, want)
	}
}

func TestNewDefaultsToUTC(t *testing.T) {
	l := New("https://example.test/", nil)
	got := l.FileName("u", "c", time.Date(2026, 9, 30, 9, 16, 0, 0, time.UTC))
	if got != "u-2026-09-30-09_16-c-s.mp3" {
		t.Errorf("FileName = %q", got)
	}
}
