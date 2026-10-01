// Package recording собирает имя файла записи разговора и публичную ссылку на
// неё. Имя реконструируется по шаблону штатной записи Asterisk:
//
//	{Uniqueid}-{YYYY-MM-DD-HH_MM}-{CallerIDNum}-s.mp3
//
// Минута начала берётся из метки времени в самом Uniqueid: Asterisk формирует
// его как «{epoch}.{микросекунды}» в момент создания канала (до постановки в
// очередь), поэтому отдельное событие Cdr не требуется. Метка трактуется в
// часовом поясе сервера Asterisk (asterisk.timezone).
package recording

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

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

// StartFromUniqueid извлекает время создания канала из Uniqueid Asterisk
// (формат «{epoch}.{микросекунды}») и переводит его в часовой пояс сервера.
// Возвращает false, если Uniqueid не содержит корректной числовой метки.
func (l *Locator) StartFromUniqueid(uniqueid string) (time.Time, bool) {
	raw := strings.TrimSpace(uniqueid)
	if i := strings.IndexByte(raw, '.'); i >= 0 {
		raw = raw[:i]
	}
	sec, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || sec <= 0 {
		return time.Time{}, false
	}
	return time.Unix(sec, 0).In(l.loc), true
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
