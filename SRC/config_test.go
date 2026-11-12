package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"1c-naparnik-mcp/naparnik"
)

// testFiles — файловые инструменты с настройками и текстами по умолчанию.
func testFiles(roots, write []string) *fileTools {
	return newFileTools(roots, write, &defaultConfig().Files, defaultTexts())
}

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func writeCfg(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Значения по умолчанию совпадают с прежними константами.
func TestDefaultConfig(t *testing.T) {
	c := defaultConfig()
	f := c.Files
	got := []any{
		c.API.BaseURL, c.API.APIPrefix, int64(c.API.SSEMaxLine),
		time.Duration(c.Ask.Timeout), c.Ask.MaxToolRounds, c.Ask.FileLogShow, c.Ask.DefaultLanguage,
		time.Duration(c.Timeouts.Default), time.Duration(c.Timeouts.Compact), time.Duration(c.Timeouts.Extra),
		c.History.ToolResultChars,
		f.ListPage, f.ReadDefaultLines, f.ReadMaxLines, int64(f.ReadMaxBytes), int64(f.MaxFileSize), f.FindMaxHits,
		time.Duration(f.WalkTimeout), f.WalkMaxDepth, f.Workers, f.SearchMaxHits, f.SearchLineCut,
		int64(f.SearchFileSize), int64(f.SearchMaxBytes), f.SearchContext, f.SearchMaxContext, f.SearchDefaultPattern,
		int64(f.WriteMaxBytes), f.EditContextLines, strings.Join(f.BomCrlfExtensions, ","), int64(c.MCP.MaxMessageBytes),
	}
	want := []any{
		"https://code.1c.ai", "/public/v1", int64(16 << 20),
		15 * time.Minute, 30, 30, "1c",
		2 * time.Minute, 10 * time.Minute, 20 * time.Second,
		300,
		200, 400, 2000, int64(80 << 10), int64(32 << 20), 100,
		40 * time.Second, 40, 16, 80, 200,
		int64(8 << 20), int64(64 << 10), 2, 10, "*.bsl",
		int64(1 << 20), 3, ".bsl,.xml,.os", int64(64 << 20),
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("значение %d: %v, ожидалось %v", i, got[i], want[i])
		}
	}
	if err := c.validate(); err != nil {
		t.Error(err)
	}
}

func TestLoadConfig(t *testing.T) {
	// Нет файла — умолчания.
	cfg, path, err := loadConfig(envOf(nil), t.TempDir())
	if err != nil || path != "" || !reflect.DeepEqual(cfg, defaultConfig()) {
		t.Fatalf("без файла: %v %q", err, path)
	}

	dir := t.TempDir()
	writeCfg(t, dir, "cfg.json", `{
		"ask": {"timeout": "5m"},
		"files": {"workers": 4, "read_max_bytes": "8KB", "search_file_size": 81920, "bom_crlf_extensions": [".bsl"]},
		"api": {"base_url": "https://example.test/"}
	}`)
	cfg, path, err = loadConfig(envOf(nil), dir)
	if err != nil || path != filepath.Join(dir, "cfg.json") {
		t.Fatalf("%v %q", err, path)
	}
	if time.Duration(cfg.Ask.Timeout) != 5*time.Minute || cfg.Files.Workers != 4 || cfg.Files.ReadMaxBytes != 8192 ||
		cfg.Files.SearchFileSize != 81920 || !slices.Equal(cfg.Files.BomCrlfExtensions, []string{".bsl"}) ||
		cfg.API.BaseURL != "https://example.test" {
		t.Errorf("значения из файла: %+v", cfg)
	}
	if cfg.Files.SearchMaxHits != 80 || cfg.Ask.MaxToolRounds != 30 {
		t.Error("неуказанные ключи должны остаться по умолчанию")
	}

	// NAPARNIK_CONFIG указывает на файл явно; NAPARNIK_BASE_URL больше не влияет — адрес только в cfg.json.
	cfg, _, err = loadConfig(envOf(map[string]string{
		"NAPARNIK_CONFIG":   filepath.Join(dir, "cfg.json"),
		"NAPARNIK_BASE_URL": "http://localhost:8080",
	}), t.TempDir())
	if err != nil || cfg.API.BaseURL != "https://example.test" {
		t.Errorf("env: %v %v", err, cfg.API.BaseURL)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	cases := map[string]string{
		`{"files": {`:                                 "некорректный JSON",
		`{"files": {"worker": 4}}`:                    "files.worker: неизвестный ключ",
		`{"api_key": "x"}`:                            "NAPARNIK_API_KEY",
		`{"ask": {"timeout": "15 минут"}}`:            "некорректная длительность",
		`{"ask": {"timeout": 15}}`:                    "длительность задаётся строкой",
		`{"files": {"workers": 0}}`:                   "files.workers: должно быть от 1 до 256",
		`{"files": {"read_max_bytes": "80 ГБ"}}`:      "некорректный размер",
		`{"files": {"read_default_lines": 3000}}`:     "files.read_default_lines",
		`{"files": {"bom_crlf_extensions": ["bsl"]}}`: "files.bom_crlf_extensions",
		`{"api": {"base_url": "code.1c.ai"}}`:         "api.base_url",
		`{"timeouts": {"extra": "10ms"}}`:             "timeouts.extra: должно быть не меньше 1s",
		`{} {}`:                                       "некорректный JSON",
		`{"files_root": ["C:/x"]}`:                    "files_root: неизвестный ключ",
		`{"write_root": []}`:                          "write_root: неизвестный ключ",
	}
	for content, want := range cases {
		dir := t.TempDir()
		writeCfg(t, dir, "cfg.json", content)
		_, _, err := loadConfig(envOf(nil), dir)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: ошибка %v, ожидалось %q", content, err, want)
		}
	}
	_, _, err := loadConfig(envOf(map[string]string{"NAPARNIK_CONFIG": filepath.Join(t.TempDir(), "нет.json")}), "")
	if err == nil || !strings.Contains(err.Error(), "файл не найден") {
		t.Errorf("NAPARNIK_CONFIG на несуществующий файл: %v", err)
	}
}

// Вывод -dump-config читается обратно в те же настройки.
func TestDumpConfigRoundTrip(t *testing.T) {
	c := defaultConfig()
	c.Files.WalkTimeout = duration(90 * time.Second)
	c.Files.ReadMaxBytes = 1500
	b, err := marshalPretty(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"walk_timeout": "90s"`) || !strings.Contains(string(b), `"max_file_size": "32MB"`) ||
		!strings.Contains(string(b), `"read_max_bytes": 1500`) {
		t.Errorf("формат:\n%s", b)
	}
	back := defaultConfig()
	if err := back.parse(b); err != nil || !reflect.DeepEqual(back, c) {
		t.Errorf("обратное чтение: %v\n%+v\n%+v", err, back, c)
	}
}

// Вшитые тексты покрывают ровно те инструменты и параметры, что объявлены в схемах кода.
func TestTextsCoverSchemas(t *testing.T) {
	tx := defaultTexts()
	s := newServer(naparnik.New("", "k"), true, fileAccess{}, defaultConfig(), tx, nil)
	if len(s.tools) != len(tx.Agent.Tools) {
		t.Errorf("инструментов агента: в коде %d, в tools.json %d", len(s.tools), len(tx.Agent.Tools))
	}
	for _, td := range s.tools {
		tt := tx.Agent.Tools[td.Name]
		if tt == nil {
			t.Errorf("%s: нет текстов", td.Name)
			continue
		}
		props := td.InputSchema["properties"].(map[string]any)
		if !slices.Equal(sortedKeys(props), sortedKeys(tt.Params)) {
			t.Errorf("%s: параметры схемы %v, тексты %v", td.Name, sortedKeys(props), sortedKeys(tt.Params))
		}
		if td.Title == "" || td.Description == "" {
			t.Errorf("%s: пустой title или description", td.Name)
		}
	}
	root := t.TempDir()
	f := testFiles([]string{root}, []string{root})
	ct := f.clientTools()
	if len(ct) != len(tx.Naparnik.Tools) {
		t.Errorf("инструментов 1С:Напарника: в коде %d, в tools.json %d", len(ct), len(tx.Naparnik.Tools))
	}
	for _, c := range ct {
		var sch struct {
			Properties map[string]map[string]string `json:"properties"`
		}
		if err := json.Unmarshal(c.Parameters, &sch); err != nil {
			t.Fatalf("%s: схема %s: %v", c.Name, c.Parameters, err)
		}
		tt := tx.Naparnik.Tools[c.Name]
		if tt == nil || !slices.Equal(sortedKeys(sch.Properties), sortedKeys(tt.Params)) {
			t.Errorf("%s: параметры схемы и тексты расходятся", c.Name)
		}
		for name, p := range sch.Properties {
			if p["description"] == "" {
				t.Errorf("%s.%s: нет описания", c.Name, name)
			}
		}
		if len(placeholders(c.Description)) > 0 {
			t.Errorf("%s: неподставленные шаблоны: %s", c.Name, c.Description)
		}
	}
}

func TestTextsRender(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	cfg := defaultConfig()
	cfg.Files.ReadMaxLines = 500
	cfg.Files.SearchContext, cfg.Files.SearchMaxContext = 3, 15
	tx := defaultTexts()

	desc := func(f *fileTools, name string) (string, string) {
		for _, c := range f.clientTools() {
			if c.Name == name {
				return c.Description, string(c.Parameters)
			}
		}
		t.Fatalf("нет %s", name)
		return "", ""
	}
	one := newFileTools([]string{a}, nil, &cfg.Files, tx)
	d, params := desc(one, "local_read_file")
	if !strings.Contains(params, "максимум 500") || !strings.Contains(d, sl(a)+". Относительный путь") {
		t.Errorf("local_read_file:\n%s\n%s", d, params)
	}
	_, params = desc(one, "local_search_text")
	if !strings.Contains(params, "0–15, по умолчанию 3") {
		t.Errorf("context_lines: %s", params)
	}
	two := newFileTools([]string{a, b}, []string{b}, &cfg.Files, tx)
	d, _ = desc(two, "local_list_dir")
	if strings.Contains(d, "Относительный путь") || !strings.Contains(d, sl(a)+", "+sl(b)) || !strings.Contains(d, "XML-выгрузка конфигурации 1С") {
		t.Errorf("paths_note many:\n%s", d)
	}
	d, _ = desc(two, "local_write_file")
	if !strings.Contains(d, "только в каталогах: "+sl(b)+".") || !strings.Contains(d, "новые .bsl, .xml, .os создаются") {
		t.Errorf("write_note:\n%s", d)
	}

	ask := func(acc fileAccess) string {
		return newServer(naparnik.New("", "k"), true, acc, cfg, tx, nil).tool("naparnik_ask").Description
	}
	if d := ask(fileAccess{}); !strings.Contains(d, "не настроен (NAPARNIK_FILES_ROOT не задан)") {
		t.Errorf("files_access none:\n%s", d)
	}
	if d := ask(fileAccess{read: []string{a}}); !strings.Contains(d, "читает файлы в каталогах "+sl(a)) || !strings.Contains(d, "Запись файлов не настроена. Возвращает") {
		t.Errorf("files_access read:\n%s", d)
	}
	if d := ask(fileAccess{read: []string{a}, write: []string{a}}); !strings.Contains(d, "править файлы в "+sl(a)+" (только там). Возвращает") {
		t.Errorf("files_access read_write:\n%s", d)
	}
	s := newServer(naparnik.New("", ""), false, fileAccess{}, cfg, tx, nil)
	if !strings.HasPrefix(s.instructions(), "Сервер даёт доступ к 1С:Напарнику") {
		t.Errorf("instructions: %s", s.instructions())
	}
	if res := s.call(context.Background(), s.tool("naparnik_budget"), nil); !strings.Contains(res.Content[0].Text, "NAPARNIK_API_KEY (Ключ доступа)") {
		t.Errorf("no key: %v", res)
	}
}

func TestTextsOverlay(t *testing.T) {
	tx := defaultTexts()
	err := tx.apply([]byte(`{
		"agent": {"tools": {"naparnik_ask": {"title": "Свой заголовок"}}},
		"naparnik": {"fragments": {"dump_layout": "", "paths_note": {"one": "Каталог: {read_dirs}."}}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(naparnik.New("", "k"), true, fileAccess{}, defaultConfig(), tx, nil)
	ask := s.tool("naparnik_ask")
	if ask.Title != "Свой заголовок" || !strings.HasPrefix(ask.Description, "Задать вопрос 1С:Напарнику") {
		t.Errorf("частичная замена: %q / %q", ask.Title, ask.Description)
	}
	root := t.TempDir()
	d := newFileTools([]string{root}, nil, &defaultConfig().Files, tx).clientTools()[0].Description
	if !strings.Contains(d, "Каталог: "+sl(root)+". В больших каталогах") || strings.Contains(d, "  ") {
		t.Errorf("пустой фрагмент и вариант:\n%s", d)
	}

	cases := map[string]string{
		`{"agent": {"tools": {"naparnik_asc": {"title": "x"}}}}`:                             "agent.tools.naparnik_asc: неизвестный инструмент",
		`{"agent": {"tools": {"naparnik_ask": {"params": {"questio": "x"}}}}}`:               "agent.tools.naparnik_ask.params.questio: неизвестный параметр",
		`{"agent": {"tools": {"naparnik_ask": {"descripton": "x"}}}}`:                        "agent.tools.naparnik_ask.descripton: неизвестный ключ",
		`{"agent": {"fragments": {"нет": "x"}}}`:                                             "неизвестный фрагмент",
		`{"agent": {"fragments": {"files_access": {"write": "x"}}}}`:                         "files_access.write: неизвестный вариант",
		`{"agent": {"fragments": {"files_access": "x"}}}`:                                    "ожидаются варианты (none, read, read_write)",
		`{"naparnik": {"fragments": {"dump_layout": {"a": "x"}}}}`:                           "ожидается строка",
		`{"naparnik": {"tools": {"local_read_file": {"description": "Читать {read_max}"}}}}`: "local_read_file.description: неизвестная подстановка {read_max}",
		`{"agent": {"fragments": {"read_access": "{files_access}"}}}`:                        "циклическая подстановка",
		`{"agent": {"tools": {"naparnik_budget": {"description": " "}}}}`:                    "naparnik_budget.description: текст не должен быть пустым",
		`{"naparnik": {"tools": {"local_read_file": {"title": "x"}}}}`:                       "local_read_file.title: неизвестный ключ",
		`{"naparnik": {"instructions": "x"}}`:                                                "naparnik.instructions: неизвестный ключ",
		`{"agnet": {}}`:                                                                      "agnet: неизвестный ключ",
	}
	for content, want := range cases {
		err := defaultTexts().apply([]byte(content))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: ошибка %v, ожидалось %q", content, err, want)
		}
	}
}

// Вывод -dump-tools — полный tools.json, который читается обратно без изменений.
func TestDumpToolsRoundTrip(t *testing.T) {
	b, err := marshalPretty(defaultTexts())
	if err != nil {
		t.Fatal(err)
	}
	tx := defaultTexts()
	if err := tx.apply(b); err != nil {
		t.Fatal(err)
	}
	b2, _ := marshalPretty(tx)
	if string(b) != string(b2) {
		t.Error("тексты изменились при обратном чтении")
	}
	if !strings.Contains(string(b), `"instructions": [`) {
		t.Errorf("многострочный текст выводится массивом строк:\n%s", b[:300])
	}

	dir := t.TempDir()
	writeCfg(t, dir, "tools.json", `{"agent": {"tools": {"naparnik_budget": {"title": "Бюджет"}}}}`)
	tx, path, err := loadTexts(envOf(nil), "", dir)
	if err != nil || path != filepath.Join(dir, "tools.json") || *tx.Agent.Tools["naparnik_budget"].Title != "Бюджет" {
		t.Errorf("loadTexts: %v %q", err, path)
	}
	_, _, err = loadTexts(envOf(map[string]string{"NAPARNIK_TOOLS": filepath.Join(dir, "нет.json")}))
	if err == nil {
		t.Error("NAPARNIK_TOOLS на несуществующий файл должен давать ошибку")
	}
}

// Лимиты из настроек действительно применяются.
func TestConfigLimitsApply(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "m.bsl", "Цель 1\nЦель 2\nЦель 3\nЦель 4\nЦель 5\n")
	cfg := defaultConfig()
	cfg.Files.SearchMaxHits = 2
	cfg.Files.ReadMaxLines = 1
	f := newFileTools([]string{root}, nil, &cfg.Files, defaultTexts())
	out, _ := f.searchText(context.Background(), "цель", "", "", 0)
	if !strings.Contains(out, "совпадений 2 в 1 файлах") || !strings.Contains(out, "показаны не все") {
		t.Errorf("search_max_hits:\n%s", out)
	}
	out, _ = f.readFile(filepath.Join(root, "m.bsl"), 1, 100)
	if !strings.Contains(out, "строки 1–1 из 5") {
		t.Errorf("read_max_lines:\n%s", out)
	}
}
