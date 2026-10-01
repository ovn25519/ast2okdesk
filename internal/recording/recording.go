// Package recording собирает имя файла записи разговора и публичную ссылку на
// неё. Имя реконструируется по фиксированному шаблону штатной записи Asterisk:
//
//	{Uniqueid}-{YYYY-MM-DD-HH_MM}-{CallerIDNum}-s.mp3
//
// Минута начала берётся из события Cdr.StartTime и трактуется в часовом поясе
// сервера Asterisk (asterisk.timezone).
package recording

import (
	"fmt"
	"strings"
	"time"
)

// cdrTimeLayout — формат времени в событии Cdr Asterisk.
const cdrTimeLayout = "2006-01-02 15:04:05"

// nameLayout — формат минуты начала в имени файла.
const nameLayout = "2006-01-02-15_04"

// Locator формирует имена файлов и ссылки на записи.
type Locator struct {
	baseURL string
	loc     *time.Location
}

// New создаёт Locator. baseURL — префикс ссылки (гарантированно дополняется
// завершающим «/»), loc — часовой пояс сервера Asterisk.
func New(baseURL string, loc *time.Location) *Locator {
	if loc == nil {
		loc = time.UTC
	}
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base != "" {
		base += "/"
	}
	return &Locator{baseURL: base, loc: loc}
}

// ParseStartTime разбирает время начала звонка из события Cdr в часовом поясе
// сервера Asterisk.
func (l *Locator) ParseStartTime(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	t, err := time.ParseInLocation(cdrTimeLayout, raw, l.loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("некорректное время начала звонка %q: %w", raw, err)
	}
	return t, nil
}

// FileName формирует имя файла записи для звонка.
func (l *Locator) FileName(uniqueid, callerID string, start time.Time) string {
	return fmt.Sprintf("%s-%s-%s-s.mp3",
		uniqueid, start.In(l.loc).Format(nameLayout), callerID)
}

// URL собирает публичную ссылку на запись по имени файла.
func (l *Locator) URL(fileName string) string { return l.baseURL + fileName }

// URLFor формирует имя файла и сразу возвращает ссылку на запись.
func (l *Locator) URLFor(uniqueid, callerID string, start time.Time) string {
	return l.URL(l.FileName(uniqueid, callerID, start))
}
