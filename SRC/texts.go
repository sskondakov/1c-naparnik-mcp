package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

// Тексты для агента и 1С:Напарника: инструкция сервера, описания инструментов и параметров.
// По умолчанию — вшитый tools.json; внешний tools.json переопределяет отдельные тексты.
// Шаблоны: {имя} — значение из настроек (textVarNames) или фрагмент своей секции.
// Фрагмент с вариантами выбирает код (textEnv.choose).

const toolsFileName = "tools.json"

//go:embed tools.json
var embeddedTools []byte

// textVarNames — подстановки, которые код заполняет из настроек.
var textVarNames = []string{
	"read_dirs", "write_dirs", "default_language",
	"read_default_lines", "read_max_lines",
	"search_context", "search_max_context", "search_default_pattern",
	"bom_crlf_extensions",
}

type texts struct {
	Agent    textSection `json:"agent"`
	Naparnik textSection `json:"naparnik"`
}

type textSection struct {
	Instructions *text                `json:"instructions,omitempty"`
	Errors       map[string]*text     `json:"errors,omitempty"`
	Fragments    map[string]*fragment `json:"fragments,omitempty"`
	Tools        map[string]*toolText `json:"tools,omitempty"`
}

type toolText struct {
	Title       *text            `json:"title,omitempty"`
	Description *text            `json:"description,omitempty"`
	Params      map[string]*text `json:"params,omitempty"`
}

// text — строка или массив строк (склеивается через перевод строки).
type text string

func (t *text) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*t = text(s)
		return nil
	}
	var lines []string
	if err := json.Unmarshal(b, &lines); err != nil {
		return errors.New("текст задаётся строкой или массивом строк")
	}
	*t = text(strings.Join(lines, "\n"))
	return nil
}

func (t text) MarshalJSON() ([]byte, error) {
	if strings.Contains(string(t), "\n") {
		return json.Marshal(strings.Split(string(t), "\n"))
	}
	return json.Marshal(string(t))
}

// fragment — текст или набор вариантов {"вариант": текст}.
type fragment struct {
	text     *text
	variants map[string]*text
}

func (f *fragment) UnmarshalJSON(b []byte) error {
	if s := strings.TrimSpace(string(b)); strings.HasPrefix(s, "{") {
		return json.Unmarshal(b, &f.variants)
	}
	f.text = new(text)
	return f.text.UnmarshalJSON(b)
}

func (f fragment) MarshalJSON() ([]byte, error) {
	if f.variants != nil {
		return json.Marshal(f.variants)
	}
	if f.text == nil {
		return []byte(`""`), nil
	}
	return f.text.MarshalJSON()
}

func defaultTexts() *texts {
	var t texts
	if err := decodeStrict(embeddedTools, &t); err != nil {
		panic("вшитый tools.json: " + err.Error())
	}
	if err := t.validate(); err != nil {
		panic("вшитый tools.json: " + err.Error())
	}
	return &t
}

// loadTexts — вшитые тексты с наложенным внешним tools.json. path — использованный файл ("" — только вшитые).
func loadTexts(getenv func(string) string, dirs ...string) (*texts, string, error) {
	t := defaultTexts()
	path, err := findFile(getenv, "NAPARNIK_TOOLS", toolsFileName, dirs...)
	if err != nil || path == "" {
		return t, "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %v", path, err)
	}
	if err := t.apply(data); err != nil {
		return nil, "", fmt.Errorf("%s: %v", path, err)
	}
	return t, path, nil
}

// apply накладывает JSON с изменёнными текстами и проверяет результат.
func (t *texts) apply(data []byte) error {
	var o texts
	if err := decodeStrict(data, &o); err != nil {
		return err
	}
	if err := t.Agent.merge(&o.Agent, "agent"); err != nil {
		return err
	}
	if err := t.Naparnik.merge(&o.Naparnik, "naparnik"); err != nil {
		return err
	}
	return t.validate()
}

func (d *textSection) merge(o *textSection, path string) error {
	unknown := func(key string) error { return fmt.Errorf("%s: неизвестный ключ", key) }
	if o.Instructions != nil {
		if d.Instructions == nil {
			return unknown(path + ".instructions")
		}
		d.Instructions = o.Instructions
	}
	for _, k := range sortedKeys(o.Errors) {
		if _, ok := d.Errors[k]; !ok {
			return unknown(path + ".errors." + k)
		}
		d.Errors[k] = o.Errors[k]
	}
	for _, k := range sortedKeys(o.Fragments) {
		key := path + ".fragments." + k
		df, ok := d.Fragments[k]
		if !ok {
			return fmt.Errorf("%s: неизвестный фрагмент", key)
		}
		of := o.Fragments[k]
		switch {
		case of.variants != nil && df.variants == nil:
			return fmt.Errorf("%s: ожидается строка, а не варианты", key)
		case of.variants == nil && df.variants != nil:
			return fmt.Errorf("%s: ожидаются варианты (%s)", key, strings.Join(sortedKeys(df.variants), ", "))
		case of.variants != nil:
			for _, v := range sortedKeys(of.variants) {
				if _, ok := df.variants[v]; !ok {
					return fmt.Errorf("%s.%s: неизвестный вариант, допустимы: %s", key, v, strings.Join(sortedKeys(df.variants), ", "))
				}
				df.variants[v] = of.variants[v]
			}
		default:
			df.text = of.text
		}
	}
	for _, name := range sortedKeys(o.Tools) {
		key := path + ".tools." + name
		dt, ok := d.Tools[name]
		if !ok {
			return fmt.Errorf("%s: неизвестный инструмент, допустимы: %s", key, strings.Join(sortedKeys(d.Tools), ", "))
		}
		ot := o.Tools[name]
		if ot.Title != nil {
			if dt.Title == nil {
				return unknown(key + ".title")
			}
			dt.Title = ot.Title
		}
		if ot.Description != nil {
			dt.Description = ot.Description
		}
		for _, p := range sortedKeys(ot.Params) {
			if _, ok := dt.Params[p]; !ok {
				return fmt.Errorf("%s.params.%s: неизвестный параметр, допустимы: %s", key, p, strings.Join(sortedKeys(dt.Params), ", "))
			}
			dt.Params[p] = ot.Params[p]
		}
	}
	return nil
}

// validate проверяет шаблоны: подстановки известны, фрагменты без циклов, обязательные тексты не пусты.
func (t *texts) validate() error {
	for _, s := range []struct {
		sec  *textSection
		name string
	}{{&t.Agent, "agent"}, {&t.Naparnik, "naparnik"}} {
		if err := s.sec.validate(s.name); err != nil {
			return err
		}
	}
	return nil
}

func (s *textSection) validate(path string) error {
	type item struct {
		key      string
		tpl      *text
		required bool
	}
	var items []item
	if s.Instructions != nil {
		items = append(items, item{path + ".instructions", s.Instructions, true})
	}
	for _, k := range sortedKeys(s.Errors) {
		items = append(items, item{path + ".errors." + k, s.Errors[k], true})
	}
	for _, k := range sortedKeys(s.Fragments) {
		f := s.Fragments[k]
		if f.variants == nil {
			items = append(items, item{path + ".fragments." + k, f.text, false})
		}
		for _, v := range sortedKeys(f.variants) {
			items = append(items, item{path + ".fragments." + k + "." + v, f.variants[v], false})
		}
	}
	for _, name := range sortedKeys(s.Tools) {
		tt := s.Tools[name]
		key := path + ".tools." + name
		if tt.Title != nil {
			items = append(items, item{key + ".title", tt.Title, true})
		}
		items = append(items, item{key + ".description", tt.Description, true})
		for _, p := range sortedKeys(tt.Params) {
			items = append(items, item{key + ".params." + p, tt.Params[p], false})
		}
	}
	for _, it := range items {
		if it.tpl == nil || strings.TrimSpace(string(*it.tpl)) == "" {
			if it.required {
				return fmt.Errorf("%s: текст не должен быть пустым", it.key)
			}
			continue
		}
		for _, ph := range placeholders(string(*it.tpl)) {
			if !slices.Contains(textVarNames, ph) && s.Fragments[ph] == nil {
				return fmt.Errorf("%s: неизвестная подстановка {%s}; допустимы: %s и фрагменты %s", it.key, ph,
					strings.Join(textVarNames, ", "), strings.Join(sortedKeys(s.Fragments), ", "))
			}
		}
	}
	// Циклы во фрагментах.
	state := map[string]int{} // 1 — в обходе, 2 — проверен
	var visit func(name string, chain []string) error
	visit = func(name string, chain []string) error {
		switch state[name] {
		case 1:
			return fmt.Errorf("%s.fragments: циклическая подстановка %s", path, strings.Join(append(chain, name), " → "))
		case 2:
			return nil
		}
		state[name] = 1
		for _, tpl := range s.Fragments[name].all() {
			for _, ph := range placeholders(tpl) {
				if s.Fragments[ph] != nil {
					if err := visit(ph, append(chain, name)); err != nil {
						return err
					}
				}
			}
		}
		state[name] = 2
		return nil
	}
	for _, k := range sortedKeys(s.Fragments) {
		if err := visit(k, nil); err != nil {
			return err
		}
	}
	return nil
}

func (f *fragment) all() []string {
	if f.variants == nil {
		if f.text == nil {
			return nil
		}
		return []string{string(*f.text)}
	}
	var out []string
	for _, k := range sortedKeys(f.variants) {
		if v := f.variants[k]; v != nil {
			out = append(out, string(*v))
		}
	}
	return out
}

// --- подстановка ---

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// scanTemplate вызывает fn для каждой подстановки {имя}; остальной текст — lit.
func scanTemplate(s string, lit func(string), fn func(name string)) {
	for {
		i := strings.IndexByte(s, '{')
		if i < 0 {
			lit(s)
			return
		}
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			lit(s)
			return
		}
		name := s[i+1 : i+j]
		if !isIdent(name) {
			lit(s[:i+1])
			s = s[i+1:]
			continue
		}
		lit(s[:i])
		fn(name)
		s = s[i+j+1:]
	}
}

func placeholders(s string) []string {
	var out []string
	scanTemplate(s, func(string) {}, func(n string) { out = append(out, n) })
	return out
}

// textEnv — значения подстановок и выбранные варианты фрагментов.
type textEnv struct {
	vars   map[string]string
	choose map[string]string
}

func newTextEnv(cfg *config, read, write []string) *textEnv {
	f := &cfg.Files
	access := "none"
	if len(read) > 0 {
		access = "read"
		if len(write) > 0 {
			access = "read_write"
		}
	}
	paths := "many"
	if len(read) == 1 {
		paths = "one"
	}
	return &textEnv{
		vars: map[string]string{
			"read_dirs":              joinSlash(read),
			"write_dirs":             joinSlash(write),
			"default_language":       cfg.Ask.DefaultLanguage,
			"read_default_lines":     strconv.Itoa(f.ReadDefaultLines),
			"read_max_lines":         strconv.Itoa(f.ReadMaxLines),
			"search_context":         strconv.Itoa(f.SearchContext),
			"search_max_context":     strconv.Itoa(f.SearchMaxContext),
			"search_default_pattern": f.SearchDefaultPattern,
			"bom_crlf_extensions":    strings.Join(f.BomCrlfExtensions, ", "),
		},
		choose: map[string]string{"files_access": access, "paths_note": paths},
	}
}

func (s *textSection) render(tpl *text, env *textEnv) string {
	if tpl == nil {
		return ""
	}
	var b strings.Builder
	s.renderTo(&b, string(*tpl), env, 0)
	out := b.String()
	for strings.Contains(out, "  ") { // пустой фрагмент между пробелами
		out = strings.ReplaceAll(out, "  ", " ")
	}
	return strings.TrimSpace(out)
}

func (s *textSection) renderTo(b *strings.Builder, tpl string, env *textEnv, depth int) {
	scanTemplate(tpl, func(l string) { b.WriteString(l) }, func(name string) {
		if v, ok := env.vars[name]; ok {
			b.WriteString(v)
			return
		}
		f := s.Fragments[name]
		if f == nil || depth > 10 {
			b.WriteString("{" + name + "}")
			return
		}
		var t *text
		if f.variants != nil {
			t = f.variants[env.choose[name]]
		} else {
			t = f.text
		}
		if t != nil {
			s.renderTo(b, strings.TrimSpace(string(*t)), env, depth+1)
		}
	})
}

// toolDoc — готовые тексты одного инструмента.
type toolDoc struct {
	Title       string
	Description string
	Params      map[string]string
}

func (s *textSection) tool(name string, env *textEnv) toolDoc {
	tt := s.Tools[name]
	if tt == nil {
		return toolDoc{Params: map[string]string{}}
	}
	d := toolDoc{Title: s.render(tt.Title, env), Description: s.render(tt.Description, env), Params: map[string]string{}}
	for k, v := range tt.Params {
		d.Params[k] = s.render(v, env)
	}
	return d
}

func (t *texts) instructions(env *textEnv) string { return t.Agent.render(t.Agent.Instructions, env) }

func (t *texts) agentError(key string, env *textEnv) string {
	return t.Agent.render(t.Agent.Errors[key], env)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
