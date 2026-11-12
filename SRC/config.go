package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"1c-naparnik-mcp/naparnik"
)

// Настройки сервера: значения по умолчанию ← cfg.json ← переменные окружения.
// В cfg.json указываются только нужные ключи; любая ошибка в файле — сервер не запускается.

const configFileName = "cfg.json"

type config struct {
	API       apiConfig      `json:"api"`
	FilesRoot []string       `json:"files_root"`
	WriteRoot []string       `json:"write_root"`
	Ask       askConfig      `json:"ask"`
	Timeouts  timeoutsConfig `json:"timeouts"`
	History   historyConfig  `json:"history"`
	Files     filesConfig    `json:"files"`
	MCP       mcpConfig      `json:"mcp"`
}

type apiConfig struct {
	BaseURL    string   `json:"base_url"`
	APIPrefix  string   `json:"api_prefix"`
	SSEMaxLine byteSize `json:"sse_max_line"`
}

type askConfig struct {
	Timeout         duration `json:"timeout"`
	MaxToolRounds   int      `json:"max_tool_rounds"` // ответов на вызовы инструментов за один вопрос
	FileLogShow     int      `json:"file_log_show"`   // сколько обращений к файлам показывать агенту
	DefaultLanguage string   `json:"default_language"`
}

type timeoutsConfig struct {
	Default duration `json:"default"`
	Compact duration `json:"compact"`
	Extra   duration `json:"extra"` // название, контекст, бюджет — вспомогательные запросы
}

type historyConfig struct {
	ToolResultChars int `json:"tool_result_chars"` // символов результата инструмента в истории диалога
}

type filesConfig struct {
	ListPage             int      `json:"list_page"`          // элементов каталога за один вызов
	ReadDefaultLines     int      `json:"read_default_lines"` // строк файла по умолчанию
	ReadMaxLines         int      `json:"read_max_lines"`     // максимум строк за вызов
	ReadMaxBytes         byteSize `json:"read_max_bytes"`     // максимум байт текста за вызов
	MaxFileSize          byteSize `json:"max_file_size"`      // файлы больше не читаются
	FindMaxHits          int      `json:"find_max_hits"`      // найденных файлов
	WalkTimeout          duration `json:"walk_timeout"`       // время на обход каталогов
	WalkMaxDepth         int      `json:"walk_max_depth"`
	Workers              int      `json:"workers"`            // параллельных чтений с диска
	SearchMaxHits        int      `json:"search_max_hits"`    // совпадений поиска
	SearchLineCut        int      `json:"search_line_cut"`    // символов строки в результатах поиска
	SearchFileSize       byteSize `json:"search_file_size"`   // файлы больше не просматриваются
	SearchMaxBytes       byteSize `json:"search_max_bytes"`   // объём результата поиска
	SearchContext        int      `json:"search_context"`     // строк контекста по умолчанию
	SearchMaxContext     int      `json:"search_max_context"` // максимум строк контекста
	SearchDefaultPattern string   `json:"search_default_pattern"`
	WriteMaxBytes        byteSize `json:"write_max_bytes"`     // максимум содержимого за один вызов записи
	EditContextLines     int      `json:"edit_context_lines"`  // строк вокруг правки в ответе
	BomCrlfExtensions    []string `json:"bom_crlf_extensions"` // новые файлы с BOM и CRLF
}

type mcpConfig struct {
	MaxMessageBytes byteSize `json:"max_message_bytes"`
}

func defaultConfig() *config {
	return &config{
		FilesRoot: []string{},
		WriteRoot: []string{},
		API: apiConfig{
			BaseURL:    naparnik.DefaultBaseURL,
			APIPrefix:  naparnik.DefaultAPIPrefix,
			SSEMaxLine: naparnik.DefaultMaxSSELine,
		},
		Ask: askConfig{
			Timeout:         duration(15 * time.Minute),
			MaxToolRounds:   30,
			FileLogShow:     30,
			DefaultLanguage: "1c",
		},
		Timeouts: timeoutsConfig{
			Default: duration(2 * time.Minute),
			Compact: duration(10 * time.Minute),
			Extra:   duration(20 * time.Second),
		},
		History: historyConfig{ToolResultChars: 300},
		Files: filesConfig{
			ListPage:             200,
			ReadDefaultLines:     400,
			ReadMaxLines:         2000,
			ReadMaxBytes:         80 << 10,
			MaxFileSize:          32 << 20,
			FindMaxHits:          100,
			WalkTimeout:          duration(40 * time.Second),
			WalkMaxDepth:         40,
			Workers:              16,
			SearchMaxHits:        80,
			SearchLineCut:        200,
			SearchFileSize:       8 << 20,
			SearchMaxBytes:       64 << 10,
			SearchContext:        2,
			SearchMaxContext:     10,
			SearchDefaultPattern: "*.bsl",
			WriteMaxBytes:        1 << 20,
			EditContextLines:     3,
			BomCrlfExtensions:    []string{".bsl", ".xml", ".os"},
		},
		MCP: mcpConfig{MaxMessageBytes: 64 << 20},
	}
}

// exeDir — каталог исполняемого файла ("" если не удалось определить).
func exeDir() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.Dir(p)
}

// findFile возвращает путь файла настроек: из переменной окружения (файл обязан существовать)
// или первый существующий name в каталогах dirs. "" — файла нет.
func findFile(getenv func(string) string, envName, name string, dirs ...string) (string, error) {
	if p := strings.TrimSpace(getenv(envName)); p != "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", fmt.Errorf("%s=%s: %v", envName, p, err)
		}
		if st, err := os.Stat(abs); err != nil || st.IsDir() {
			return "", fmt.Errorf("%s=%s: файл не найден", envName, abs)
		}
		return abs, nil
	}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", nil
}

// loadConfig собирает настройки. path — использованный cfg.json ("" — только умолчания).
func loadConfig(getenv func(string) string, exe string) (cfg *config, path string, err error) {
	cfg = defaultConfig()
	path, err = findFile(getenv, "NAPARNIK_CONFIG", configFileName, exe)
	if err != nil {
		return nil, "", err
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, "", fmt.Errorf("%s: %v", path, err)
		}
		if err := cfg.parse(data); err != nil {
			return nil, "", fmt.Errorf("%s: %v", path, err)
		}
		dir := filepath.Dir(path)
		for _, list := range []*[]string{&cfg.FilesRoot, &cfg.WriteRoot} {
			for i, d := range *list {
				if d = strings.TrimSpace(d); d != "" && !filepath.IsAbs(filepath.FromSlash(d)) {
					d = filepath.Join(dir, filepath.FromSlash(d))
				}
				(*list)[i] = d
			}
		}
	}
	// Переменные окружения важнее файла.
	if v := strings.TrimSpace(getenv("NAPARNIK_BASE_URL")); v != "" {
		cfg.API.BaseURL = v
	}
	if v := strings.TrimSpace(getenv("NAPARNIK_FILES_ROOT")); v != "" {
		cfg.FilesRoot = filepath.SplitList(v)
	}
	if v := strings.TrimSpace(getenv("NAPARNIK_WRITE_ROOT")); v != "" {
		cfg.WriteRoot = filepath.SplitList(v)
	}
	cfg.API.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.API.BaseURL), "/")
	if err := cfg.validate(); err != nil {
		where := "настройки"
		if path != "" {
			where = path
		}
		return nil, "", fmt.Errorf("%s: %v", where, err)
	}
	return cfg, path, nil
}

// parse накладывает JSON поверх текущих значений.
func (c *config) parse(data []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return fmt.Errorf("некорректный JSON: %v", err)
	}
	if _, ok := top["api_key"]; ok {
		return errors.New("api_key: ключ задаётся только в переменной окружения NAPARNIK_API_KEY")
	}
	return decodeStrict(data, c)
}

// decodeStrict разбирает JSON в v: неизвестные ключи (с путём) и данные после объекта — ошибка.
func decodeStrict(data []byte, v any) error {
	if err := checkKeys(data, reflect.TypeOf(v), ""); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("некорректный JSON: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("некорректный JSON: лишние данные после объекта")
	}
	return nil
}

var unmarshalerType = reflect.TypeFor[json.Unmarshaler]()

// checkKeys ищет ключи JSON, которых нет в структуре t, и сообщает путь к ним.
func checkKeys(data []byte, t reflect.Type, path string) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if reflect.PointerTo(t).Implements(unmarshalerType) {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		var m map[string]json.RawMessage
		if json.Unmarshal(data, &m) != nil {
			return nil // ошибку типа сообщит основной разбор
		}
		fields := map[string]reflect.Type{}
		for i := range t.NumField() {
			f := t.Field(i)
			if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name != "" && name != "-" {
				fields[name] = f.Type
			}
		}
		for k, raw := range m {
			ft, ok := fields[k]
			if !ok {
				return fmt.Errorf("%s: неизвестный ключ", joinKey(path, k))
			}
			if err := checkKeys(raw, ft, joinKey(path, k)); err != nil {
				return err
			}
		}
	case reflect.Map:
		var m map[string]json.RawMessage
		if json.Unmarshal(data, &m) != nil {
			return nil
		}
		for k, raw := range m {
			if err := checkKeys(raw, t.Elem(), joinKey(path, k)); err != nil {
				return err
			}
		}
	}
	return nil
}

func joinKey(path, k string) string {
	if path == "" {
		return k
	}
	return path + "." + k
}

func (c *config) validate() error {
	var errs []string
	check := func(ok bool, key, msg string) {
		if !ok {
			errs = append(errs, key+": "+msg)
		}
	}
	rng := func(v, lo, hi int, key string) {
		check(v >= lo && v <= hi, key, fmt.Sprintf("должно быть от %d до %d", lo, hi))
	}
	atLeast := func(v, lo int, key string) {
		check(v >= lo, key, fmt.Sprintf("должно быть не меньше %d", lo))
	}
	size := func(v byteSize, lo byteSize, key string) {
		check(v >= lo, key, "должно быть не меньше "+lo.String())
	}
	dur := func(v duration, key string) {
		check(time.Duration(v) >= time.Second, key, "должно быть не меньше 1s")
	}

	u, err := url.Parse(c.API.BaseURL)
	check(err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "", "api.base_url", "нужен адрес вида https://host")
	check(c.API.APIPrefix == "" || strings.HasPrefix(c.API.APIPrefix, "/"), "api.api_prefix", "должен начинаться с /")
	size(c.API.SSEMaxLine, 64<<10, "api.sse_max_line")

	dur(c.Ask.Timeout, "ask.timeout")
	rng(c.Ask.MaxToolRounds, 1, 1000, "ask.max_tool_rounds")
	atLeast(c.Ask.FileLogShow, 0, "ask.file_log_show")
	check(strings.TrimSpace(c.Ask.DefaultLanguage) != "", "ask.default_language", "не должно быть пустым")

	dur(c.Timeouts.Default, "timeouts.default")
	dur(c.Timeouts.Compact, "timeouts.compact")
	dur(c.Timeouts.Extra, "timeouts.extra")
	atLeast(c.History.ToolResultChars, 1, "history.tool_result_chars")

	f := &c.Files
	atLeast(f.ListPage, 1, "files.list_page")
	atLeast(f.ReadMaxLines, 1, "files.read_max_lines")
	rng(f.ReadDefaultLines, 1, max(f.ReadMaxLines, 1), "files.read_default_lines")
	size(f.ReadMaxBytes, 1<<10, "files.read_max_bytes")
	size(f.MaxFileSize, 1<<10, "files.max_file_size")
	atLeast(f.FindMaxHits, 1, "files.find_max_hits")
	dur(f.WalkTimeout, "files.walk_timeout")
	atLeast(f.WalkMaxDepth, 1, "files.walk_max_depth")
	rng(f.Workers, 1, 256, "files.workers")
	atLeast(f.SearchMaxHits, 1, "files.search_max_hits")
	atLeast(f.SearchLineCut, 10, "files.search_line_cut")
	size(f.SearchFileSize, 1<<10, "files.search_file_size")
	size(f.SearchMaxBytes, 1<<10, "files.search_max_bytes")
	rng(f.SearchMaxContext, 0, 100, "files.search_max_context")
	rng(f.SearchContext, 0, max(f.SearchMaxContext, 0), "files.search_context")
	_, perr := nameMatcher(f.SearchDefaultPattern)
	check(strings.TrimSpace(f.SearchDefaultPattern) != "" && perr == nil, "files.search_default_pattern", "нужен шаблон имени файла, например *.bsl")
	size(f.WriteMaxBytes, 1<<10, "files.write_max_bytes")
	rng(f.EditContextLines, 0, 50, "files.edit_context_lines")
	for _, e := range f.BomCrlfExtensions {
		check(len(e) >= 2 && strings.HasPrefix(e, ".") && !strings.ContainsAny(e, `/\ `), "files.bom_crlf_extensions", fmt.Sprintf("%q — нужно расширение вида .bsl", e))
	}
	size(c.MCP.MaxMessageBytes, 1<<20, "mcp.max_message_bytes")

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// --- типы значений ---

// duration — длительность в формате Go: "15m", "40s", "1h30m".
type duration time.Duration

func (d *duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("длительность задаётся строкой, например \"40s\" или \"15m\"")
	}
	v, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("некорректная длительность %q, пример: \"40s\", \"15m\"", s)
	}
	*d = duration(v)
	return nil
}

func (d duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d duration) String() string {
	v := time.Duration(d)
	switch {
	case v != 0 && v%time.Hour == 0:
		return strconv.FormatInt(int64(v/time.Hour), 10) + "h"
	case v != 0 && v%time.Minute == 0:
		return strconv.FormatInt(int64(v/time.Minute), 10) + "m"
	case v%time.Second == 0:
		return strconv.FormatInt(int64(v/time.Second), 10) + "s"
	}
	return v.String()
}

// byteSize — размер: число байт или строка с единицей ("80KB", "32MB", "1GB", "512B").
type byteSize int64

var sizeUnits = []struct {
	name string
	mul  int64
}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}}

func (s *byteSize) UnmarshalJSON(b []byte) error {
	var n int64
	if json.Unmarshal(b, &n) == nil {
		*s = byteSize(n)
		return nil
	}
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return errors.New("размер задаётся числом байт или строкой, например \"80KB\", \"32MB\"")
	}
	t := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(str), " ", ""))
	for _, u := range sizeUnits {
		if num, ok := strings.CutSuffix(t, u.name); ok {
			v, err := strconv.ParseInt(num, 10, 64)
			if err != nil || v < 0 {
				break
			}
			*s = byteSize(v * u.mul)
			return nil
		}
	}
	return fmt.Errorf("некорректный размер %q, пример: \"80KB\", \"32MB\"", str)
}

func (s byteSize) MarshalJSON() ([]byte, error) {
	for _, u := range sizeUnits[:3] {
		if s != 0 && int64(s)%u.mul == 0 {
			return json.Marshal(s.String())
		}
	}
	return json.Marshal(int64(s))
}

func (s byteSize) String() string {
	for _, u := range sizeUnits {
		if s != 0 && int64(s)%u.mul == 0 {
			return strconv.FormatInt(int64(s)/u.mul, 10) + u.name
		}
	}
	return "0B"
}

// marshalPretty — JSON с отступами и без HTML-экранирования.
func marshalPretty(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
