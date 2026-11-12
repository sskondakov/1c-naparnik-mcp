package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"1c-naparnik-mcp/naparnik"
)

func readLog(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLogLevels(t *testing.T) {
	if _, err := parseLogLevel("trace"); err == nil {
		t.Error("неизвестный уровень должен давать ошибку")
	}
	if l, err := parseLogLevel(""); err != nil || l != levelInfo {
		t.Errorf("по умолчанию info: %v %v", l, err)
	}
	for _, c := range []struct {
		level string
		want  []string
		not   []string
	}{
		{"error", []string{"error сбой"}, []string{"ход", "детали"}},
		{"info", []string{"error сбой", "info  ход"}, []string{"детали"}},
		{"DEBUG", []string{"error сбой", "info  ход", "debug детали"}, nil},
	} {
		p := filepath.Join(t.TempDir(), "n.log")
		l, _ := parseLogLevel(c.level)
		j, warn := openJournal(p, l)
		if warn != "" {
			t.Fatal(warn)
		}
		j.errorf("сбой\nвторая строка")
		j.infof("ход")
		j.debugf("детали")
		j.close()
		got := readLog(t, p)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: нет %q в\n%s", c.level, w, got)
			}
		}
		for _, w := range c.not {
			if strings.Contains(got, w) {
				t.Errorf("%s: лишнее %q в\n%s", c.level, w, got)
			}
		}
		if !strings.Contains(got, `сбой\nвторая строка`) || !strings.Contains(got, fmt.Sprintf("[%d]", os.Getpid())) {
			t.Errorf("одна строка на запись и PID:\n%s", got)
		}
	}
}

func TestJournalOff(t *testing.T) {
	var j *journal // журнал выключен: методы ничего не делают
	j.errorf("x")
	j.apiRequest("GET", "/x", 200, 0, nil)
	j.close()

	dir := t.TempDir()
	if j, warn := openJournal("", levelInfo); j != nil || warn != "" {
		t.Error("пустой путь — журнала нет")
	}
	if j, warn := openJournal(filepath.Join(dir, "нет", "n.log"), levelInfo); j != nil || !strings.Contains(warn, "работаю без журнала") {
		t.Errorf("ошибка открытия: %v %q", j, warn)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Error("файлы не должны создаваться")
	}
}

func TestSafeArgs(t *testing.T) {
	got := safeArgs(`{"path":"C:/a/<b>.bsl","content":"секретный код","old_text":"абв","new_text":"где"}`)
	if strings.Contains(got, "секретный") || strings.Contains(got, "абв") || !strings.Contains(got, `"content":"<13 символов>"`) ||
		!strings.Contains(got, `"path":"C:/a/<b>.bsl"`) {
		t.Errorf("safeArgs: %s", got)
	}
}

// Ошибка 500 от API попадает в журнал с методом, путём, кодом и текстом; ключ, вопрос и содержимое файлов — нет.
func TestJournalAsk(t *testing.T) {
	root := testProject(t)
	mod := sl(root, "Documents/Продажа/Ext/ObjectModule.bsl")
	round := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/public/v1/conversations":
			fmt.Fprint(w, `{"uuid":"c1"}`)
		case r.URL.Path == "/public/v1/conversations/c1/messages":
			round++
			if round == 1 {
				args, _ := json.Marshal(map[string]any{"path": mod})
				call, _ := json.Marshal(map[string]any{"id": "k1", "function": map[string]string{"name": "local_read_file", "arguments": string(args)}})
				fmt.Fprint(w, "data: {\"uuid\":\"a1\",\"role\":\"assistant\",\"content\":{\"tool_calls\":["+string(call)+"]},\"finished\":true}\n\n")
				return
			}
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":"Внутренняя ошибка генерации","error_type":"internal_error"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer api.Close()

	p := filepath.Join(t.TempDir(), "n.log")
	j, _ := openJournal(p, levelDebug)
	client := naparnik.New(api.URL, "секретный-ключ")
	client.OnRequest = j.apiRequest
	acc, _ := newFileAccess([]string{root}, nil)
	s := newServer(client, true, acc, defaultConfig(), defaultTexts(), j)
	res := s.call(context.Background(), s.tool("naparnik_ask"), json.RawMessage(`{"question":"Тайный вопрос про проведение"}`))
	j.close()
	if !res.IsError {
		t.Fatalf("ожидалась ошибка: %v", res)
	}
	got := readLog(t, p)
	for _, w := range []string{
		"info  вопрос: диалог новый, 28 символов, use_files=true, allow_write=false",
		"info  api POST /conversations 200 ",
		"info  раунд 1: local_read_file",
		`debug local_read_file {"path":"` + mod + `"}: ok`,
		"error api POST /conversations/c1/messages 500 ",
		"internal_error: Внутренняя ошибка генерации",
		"error инструмент naparnik_ask:",
		"info  итог: диалог c1, ошибка, ",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("нет %q в журнале:\n%s", w, got)
		}
	}
	for _, w := range []string{"секретный-ключ", "Тайный вопрос", "Движения.Остатки"} {
		if strings.Contains(got, w) {
			t.Errorf("в журнал попало %q:\n%s", w, got)
		}
	}
}
