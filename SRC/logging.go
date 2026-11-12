package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Журнал в файл (NAPARNIK_LOG). Нужен, чтобы разобрать сбой задним числом.
// Никогда не пишет ключ API, тексты вопросов и ответов, содержимое файлов.
// Все методы безопасны для nil: без NAPARNIK_LOG журнал — nil и ничего не делает.

type logLevel int

const (
	levelError logLevel = iota
	levelInfo
	levelDebug
)

var levelNames = map[string]logLevel{"error": levelError, "info": levelInfo, "debug": levelDebug}

type journal struct {
	mu    sync.Mutex
	w     *os.File
	level logLevel
	pid   int
}

// parseLogLevel разбирает NAPARNIK_LOG_LEVEL; пусто — info.
func parseLogLevel(s string) (logLevel, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return levelInfo, nil
	}
	l, ok := levelNames[s]
	if !ok {
		return 0, fmt.Errorf("NAPARNIK_LOG_LEVEL=%s: допустимы error, info, debug", s)
	}
	return l, nil
}

// openJournal открывает журнал на дозапись. Пустой path — журнала нет (nil).
// Ошибка открытия не мешает работе: возвращается nil и текст предупреждения.
func openJournal(path string, level logLevel) (*journal, string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, ""
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Sprintf("журнал %s не открыт (%v) — работаю без журнала", path, err)
	}
	return &journal{w: f, level: level, pid: os.Getpid()}, ""
}

func (j *journal) enabled(l logLevel) bool { return j != nil && l <= j.level }

func (j *journal) write(l logLevel, name, format string, args ...any) {
	if !j.enabled(l) {
		return
	}
	msg := fmt.Sprintf(format, args...)
	msg = strings.ReplaceAll(strings.ReplaceAll(msg, "\r", ""), "\n", `\n`)
	line := fmt.Sprintf("%s [%d] %-5s %s\n", time.Now().Format("2006-01-02 15:04:05.000"), j.pid, name, msg)
	j.mu.Lock()
	defer j.mu.Unlock()
	_, _ = j.w.WriteString(line)
}

func (j *journal) errorf(format string, args ...any) { j.write(levelError, "error", format, args...) }
func (j *journal) infof(format string, args ...any)  { j.write(levelInfo, "info", format, args...) }
func (j *journal) debugf(format string, args ...any) { j.write(levelDebug, "debug", format, args...) }

func (j *journal) close() {
	if j != nil {
		_ = j.w.Close()
	}
}

// apiRequest — хук naparnik.Client.OnRequest: каждый запрос к API (info), ошибки (error).
func (j *journal) apiRequest(method, path string, status int, dur time.Duration, err error) {
	if j == nil {
		return
	}
	code := "-"
	if status > 0 {
		code = fmt.Sprint(status)
	}
	switch {
	case err != nil && errors.Is(err, context.Canceled):
		j.infof("api %s %s %s %s: прервано клиентом", method, path, code, secs(dur))
	case err != nil:
		j.errorf("api %s %s %s %s: %v", method, path, code, secs(dur), err)
	default:
		j.infof("api %s %s %s %s", method, path, code, secs(dur))
	}
}

func secs(d time.Duration) string { return fmt.Sprintf("%.1fs", d.Seconds()) }

// safeArgs — аргументы вызова инструмента для журнала: содержимое файлов заменяется длиной.
func safeArgs(argsJSON string) string {
	var m map[string]any
	if json.Unmarshal([]byte(argsJSON), &m) != nil {
		return fmt.Sprintf("<%d символов, не JSON>", len([]rune(argsJSON)))
	}
	for _, k := range []string{"content", "old_text", "new_text"} {
		if s, ok := m[k].(string); ok {
			m[k] = fmt.Sprintf("<%d символов>", len([]rune(s)))
		}
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(m)
	return strings.TrimSpace(b.String())
}
