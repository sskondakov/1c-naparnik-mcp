package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"1c-naparnik-mcp/naparnik"
)

const bom = "\xef\xbb\xbf"

func writeTestFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func testProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeTestFile(t, root, "Documents/Продажа/Ext/ObjectModule.bsl", bom+"Процедура ОбработкаПроведения(Отказ, Режим)\r\n\tДвижения.Остатки.Записать();\r\nКонецПроцедуры\r\n")
	writeTestFile(t, root, "Documents/Продажа.xml", "<MetaDataObject/>")
	writeTestFile(t, root, "CommonModules/ОбщийМодуль/Ext/Module.bsl", "Функция Тест() Экспорт\n\tВозврат ДВИЖЕНИЯ;\nКонецФункции\n")
	writeTestFile(t, root, "bin.dat", "\x00\x01\x02")
	return root
}

// sl — абсолютный путь в виде, который видит Напарник.
func sl(parts ...string) string { return filepath.ToSlash(filepath.Join(parts...)) }

func TestFileAccessParse(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	nested := filepath.Join(a, "sub")
	_ = os.MkdirAll(nested, 0o755)
	outside := t.TempDir()
	missing := filepath.Join(a, "нет")

	acc, warns := newFileAccess([]string{a, nested, b, a, missing}, []string{nested, outside})
	if len(acc.read) != 2 || acc.read[0] != a || acc.read[1] != b {
		t.Errorf("read %v", acc.read)
	}
	if len(acc.write) != 1 || acc.write[0] != nested {
		t.Errorf("write %v", acc.write)
	}
	joined := strings.Join(warns, "\n")
	if !strings.Contains(joined, missing) || !strings.Contains(joined, outside+" не входит") {
		t.Errorf("warns:\n%s", joined)
	}
}

func TestResolve(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	one := testFiles([]string{a}, nil)
	two := testFiles([]string{a, b}, nil)

	ok := map[string]struct {
		f    *fileTools
		want string
	}{
		filepath.Join(a, "x", "y.bsl"):          {one, filepath.Join(a, "x", "y.bsl")},
		filepath.ToSlash(filepath.Join(b, "z")): {two, filepath.Join(b, "z")},
		"x/y.bsl":                               {one, filepath.Join(a, "x", "y.bsl")},
		`x\..\y`:                                {one, filepath.Join(a, "y")},
		strings.ToUpper(a[:1]) + a[1:] + `\q`:   {one, filepath.Join(strings.ToUpper(a[:1])+a[1:], "q")},
		strings.ToLower(filepath.Join(a, "Регистр.bsl")): {one, strings.ToLower(filepath.Join(a, "Регистр.bsl"))},
	}
	for in, c := range ok {
		got, err := c.f.resolve(in)
		if err != nil || !strings.EqualFold(got, c.want) {
			t.Errorf("resolve(%q) = %q, %v; want %q", in, got, err, c.want)
		}
	}
	for in, f := range map[string]*fileTools{
		"../x":                                  one,
		filepath.Join(a, "..", "x"):             one,
		filepath.Join(filepath.Dir(a), "other"): one,
		"x/y.bsl":                               two, // относительный путь при нескольких корнях
	} {
		if _, err := f.resolve(in); err == nil {
			t.Errorf("resolve(%q) must fail", in)
		}
	}
	if got, err := one.resolve("  "); err != nil || got != "" {
		t.Errorf("empty path: %q %v", got, err)
	}
}

func TestFilesOps(t *testing.T) {
	root := testProject(t)
	f := testFiles([]string{root}, nil)
	ctx := context.Background()

	out, err := f.listDir("", "", 0)
	if err != nil || !strings.Contains(out, sl(root)+"/") || strings.Contains(out, "запись") {
		t.Errorf("list roots: %v\n%s", err, out)
	}
	out, err = f.listDir(filepath.Join(root, "Documents"), "", 0)
	if err != nil || !strings.Contains(out, "Каталог "+sl(root, "Documents")+"/") || !strings.Contains(out, "Продажа/") || !strings.Contains(out, "Продажа.xml (17 Б)") {
		t.Errorf("list: %v\n%s", err, out)
	}
	out, _ = f.listDir(root, "common", 0)
	if !strings.Contains(out, "CommonModules/") || strings.Contains(out, "Documents") {
		t.Errorf("list filter:\n%s", out)
	}

	mod := sl(root, "Documents/Продажа/Ext/ObjectModule.bsl")
	out, err = f.readFile(mod, 2, 1)
	if err != nil || !strings.Contains(out, "Файл "+mod+", строки 2–2 из 3") || !strings.Contains(out, "2\t\tДвижения.Остатки.Записать();\n") ||
		!strings.Contains(out, "start_line=3") || strings.Contains(out, "\r") {
		t.Errorf("read: %v\n%q", err, out)
	}
	out, _ = f.readFile("Documents/Продажа/Ext/ObjectModule.bsl", 0, 0) // относительный при одном корне
	if strings.Contains(out, bom) || !strings.Contains(out, "1\tПроцедура") {
		t.Errorf("read BOM: %q", out)
	}
	if _, err := f.readFile(filepath.Join(root, "bin.dat"), 0, 0); err == nil {
		t.Error("binary must fail")
	}
	if _, err := f.readFile(filepath.Join(root, "Documents"), 0, 0); err == nil {
		t.Error("dir must fail")
	}

	out, _ = f.searchText(ctx, "движения", "", "", 0)
	want := "== " + sl(root, "CommonModules/ОбщийМодуль/Ext/Module.bsl") + "\n2: \tВозврат ДВИЖЕНИЯ;\n" +
		"== " + mod + "\n2: \tДвижения.Остатки.Записать();"
	if !strings.Contains(out, "совпадений 2 в 2 файлах, просмотрено файлов 2") || !strings.HasSuffix(out, want) {
		t.Errorf("search:\n%s", out)
	}
	out, _ = f.searchText(ctx, "движения", filepath.Join(root, "Documents"), "", 1)
	want = "== " + mod + "\n1- Процедура ОбработкаПроведения(Отказ, Режим)\n2: \tДвижения.Остатки.Записать();\n3- КонецПроцедуры"
	if !strings.Contains(out, "контекст ±1") || !strings.HasSuffix(out, want) {
		t.Errorf("search context:\n%s", out)
	}
	out, _ = f.findFiles(ctx, "*.xml", "")
	if !strings.Contains(out, "найдено 1") || !strings.Contains(out, sl(root, "Documents/Продажа.xml")) {
		t.Errorf("find:\n%s", out)
	}
	out, _ = f.findFiles(ctx, "продажа", filepath.Join(root, "Documents"))
	if !strings.Contains(out, sl(root, "Documents/Продажа")+"/") {
		t.Errorf("find substring:\n%s", out)
	}
}

func TestSearchMultipleRoots(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	writeTestFile(t, a, "m.bsl", "Цель А\n")
	writeTestFile(t, b, "m.bsl", "Цель Б\n")
	f := testFiles([]string{a, b}, nil)
	out, _ := f.searchText(context.Background(), "цель", "", "", 0)
	if !strings.Contains(out, "совпадений 2 в 2 файлах") || !strings.Contains(out, sl(a, "m.bsl")) || !strings.Contains(out, sl(b, "m.bsl")) {
		t.Errorf("got:\n%s", out)
	}
}

// Блоки контекста: пересекающиеся окна сливаются, далёкие разделяются «--».
func TestSearchContextBlocks(t *testing.T) {
	root := t.TempDir()
	var lines []string
	for i := 1; i <= 20; i++ {
		l := fmt.Sprintf("строка %d", i)
		if i == 3 || i == 5 || i == 15 {
			l += " Цель"
		}
		lines = append(lines, l)
	}
	writeTestFile(t, root, "m.bsl", strings.Join(lines, "\n"))
	f := testFiles([]string{root}, nil)
	out, _ := f.searchText(context.Background(), "цель", "", "", 2)
	want := "== " + sl(root, "m.bsl") + "\n1- строка 1\n2- строка 2\n3: строка 3 Цель\n4- строка 4\n5: строка 5 Цель\n6- строка 6\n7- строка 7\n--\n" +
		"13- строка 13\n14- строка 14\n15: строка 15 Цель\n16- строка 16\n17- строка 17"
	if !strings.Contains(out, "совпадений 3 в 1 файлах") || !strings.HasSuffix(out, want) {
		t.Errorf("got:\n%s", out)
	}
}

// Много файлов — проверка параллельного обхода: все найдены, порядок стабильный, лимит соблюдается.
func TestSearchParallelMany(t *testing.T) {
	root := t.TempDir()
	for i := range 300 {
		body := "Процедура Тест()\nКонецПроцедуры\n"
		if i%10 == 0 {
			body = "Процедура Тест()\n\tНайдиМеня();\nКонецПроцедуры\n"
		}
		writeTestFile(t, root, fmt.Sprintf("M%03d/Ext/Module.bsl", i), body)
	}
	f := testFiles([]string{root}, nil)
	out, _ := f.searchText(context.Background(), "найдименя", "", "", 0)
	if !strings.Contains(out, "совпадений 30 в 30 файлах, просмотрено файлов 300") || !strings.Contains(out, "== "+sl(root, "M000/Ext/Module.bsl")+"\n2: ") ||
		strings.Index(out, "M010/") > strings.Index(out, "M290/") {
		t.Errorf("got:\n%s", out[:min(len(out), 400)])
	}
	out, _ = f.searchText(context.Background(), "процедура", "", "", 0)
	if !strings.Contains(out, "показаны не все") {
		t.Errorf("limit not reported:\n%s", out[:min(len(out), 300)])
	}
	out, _ = f.findFiles(context.Background(), "module.bsl", "")
	if !strings.Contains(out, "найдено 100") || !strings.Contains(out, "показаны не все") {
		t.Errorf("find limit:\n%s", out[:min(len(out), 300)])
	}
}

func TestWritePermissions(t *testing.T) {
	root := testProject(t)
	w := filepath.Join(root, "Out")
	_ = os.MkdirAll(w, 0o755)

	ro := testFiles([]string{root}, nil)
	if _, err := ro.writeFile(filepath.Join(w, "a.bsl"), "x"); err == nil || !strings.Contains(err.Error(), "не разрешена") {
		t.Errorf("write without permission: %v", err)
	}
	f := testFiles([]string{root}, []string{w})
	for _, bad := range []string{
		filepath.Join(root, "Documents", "x.bsl"), // вне каталога записи
		filepath.Join(w, "..", "x.bsl"),
		filepath.Join(w, ".git", "config"),
		w, // сам каталог
		filepath.Join(filepath.Dir(root), "x.bsl"),
	} {
		if _, err := f.writeFile(bad, "x"); err == nil {
			t.Errorf("write %s must fail", bad)
		}
	}
	if _, err := f.editFile(filepath.Join(root, "Documents/Продажа/Ext/ObjectModule.bsl"), "Движения", "X", false); err == nil {
		t.Error("edit outside write root must fail")
	}
	out, _ := f.listDir("", "", 0)
	if !strings.Contains(out, "Запись разрешена в: "+sl(w)+"/") {
		t.Errorf("roots listing:\n%s", out)
	}
}

func TestWriteAndEdit(t *testing.T) {
	root := t.TempDir()
	f := testFiles([]string{root}, []string{root})

	// Новый .bsl — BOM + CRLF, каталоги создаются.
	p := filepath.Join(root, "CommonModules", "Тест", "Ext", "Module.bsl")
	out, err := f.writeFile(sl(p), "Функция Сложить(А, Б) Экспорт\n\tВозврат А + Б;\nКонецФункции\n")
	if err != nil || !strings.Contains(out, "создан: 3 строк (UTF-8 с BOM, CRLF)") {
		t.Fatalf("write: %v %s", err, out)
	}
	raw, _ := os.ReadFile(p)
	if string(raw) != bom+"Функция Сложить(А, Б) Экспорт\r\n\tВозврат А + Б;\r\nКонецФункции\r\n" {
		t.Errorf("new file bytes: %q", raw)
	}
	// Новый .txt — без BOM, LF.
	txt := filepath.Join(root, "readme.txt")
	if _, err := f.writeFile(txt, "a\r\nb\n"); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(txt); string(raw) != "a\nb\n" {
		t.Errorf("txt bytes: %q", raw)
	}

	// Правка сохраняет BOM и CRLF; old_text с LF совпадает с CRLF-файлом.
	out, err = f.editFile(p, "\tВозврат А + Б;\n", "\tРезультат = А + Б;\n\tВозврат Результат;\n", false)
	if err != nil || !strings.Contains(out, "замен 1") || !strings.Contains(out, "3\t\tВозврат Результат;") {
		t.Fatalf("edit: %v\n%s", err, out)
	}
	raw, _ = os.ReadFile(p)
	if !bytes.HasPrefix(raw, []byte(bom)) || !strings.Contains(string(raw), "\tРезультат = А + Б;\r\n\tВозврат Результат;\r\n") || strings.Count(string(raw), "\n") != strings.Count(string(raw), "\r\n") {
		t.Errorf("edited bytes: %q", raw)
	}

	// Ошибки: не найдено, неоднозначно, без изменений, несуществующий файл.
	if _, err := f.editFile(p, "НетТакого", "x", false); err == nil || !strings.Contains(err.Error(), "не найден") {
		t.Errorf("not found: %v", err)
	}
	if _, err := f.editFile(p, "Б", "В", false); err == nil || !strings.Contains(err.Error(), "раз") {
		t.Errorf("ambiguous: %v", err)
	}
	if _, err := f.editFile(p, "А", "А", false); err == nil {
		t.Error("no-op edit must fail")
	}
	if _, err := f.editFile(filepath.Join(root, "нет.bsl"), "a", "b", false); err == nil || !strings.Contains(err.Error(), "local_write_file") {
		t.Errorf("missing file: %v", err)
	}
	// replace_all.
	out, err = f.editFile(p, "Б", "Второе", true)
	if err != nil || !strings.Contains(out, "замен 2") {
		t.Errorf("replace_all: %v %s", err, out)
	}
	// Перезапись существующего файла без BOM/с LF сохраняет его формат.
	lf := writeTestFile(t, root, "lf.bsl", "старое\n")
	if _, err := f.writeFile(lf, "новое\r\n"); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(lf); string(raw) != "новое\n" {
		t.Errorf("overwrite format: %q", raw)
	}
	if len(f.changed) != 3 {
		t.Errorf("changed: %v", f.changed)
	}
}

// Напарник читает и правит файлы через клиентские инструменты — полный цикл через ask.
func TestAskWithFiles(t *testing.T) {
	root := testProject(t)
	out := filepath.Join(root, "Out")
	_ = os.MkdirAll(out, 0o755)
	target := writeTestFile(t, root, "Out/Module.bsl", bom+"Процедура А()\r\nКонецПроцедуры\r\n")
	mod := sl(root, "Documents/Продажа/Ext/ObjectModule.bsl")

	var toolsSeen [][]string
	round := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/public/v1/conversations"):
			fmt.Fprint(w, `{"uuid":"c"}`)
		case r.URL.Path == "/public/v1/conversations/c/messages":
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Role    string                `json:"role"`
				Tools   []naparnik.ClientTool `json:"tools"`
				Content json.RawMessage       `json:"content"`
			}
			_ = json.Unmarshal(body, &req)
			if req.Role == "user" {
				var names []string
				for _, tl := range req.Tools {
					names = append(names, tl.Name)
				}
				toolsSeen = append(toolsSeen, names)
				round = 0
				if len(req.Tools) == 0 {
					fmt.Fprint(w, "data: {\"uuid\":\"a0\",\"role\":\"assistant\",\"content\":{\"content\":\"Без файлов.\"},\"finished\":true}\n\n")
					return
				}
			}
			round++
			if round == 1 {
				call := func(id, name string, args map[string]any) string {
					a, _ := json.Marshal(args)
					b, _ := json.Marshal(map[string]any{"id": id, "function": map[string]string{"name": name, "arguments": string(a)}})
					return string(b)
				}
				calls := []string{
					call("k1", "local_read_file", map[string]any{"path": mod}),
					call("k2", "local_read_file", map[string]any{"path": "../secret"}),
					call("k3", "local_edit_file", map[string]any{"path": sl(target), "old_text": "Процедура А()", "new_text": "Процедура Б()"}),
				}
				fmt.Fprint(w, "data: {\"uuid\":\"a1\",\"role\":\"assistant\",\"content\":{\"tool_calls\":["+strings.Join(calls, ",")+"]},\"finished\":true}\n\n")
				return
			}
			var res []naparnik.ToolContent
			_ = json.Unmarshal(req.Content, &res)
			if req.Role != "tool" || len(res) != 3 || res[0].Status != "ok" || !strings.Contains(*res[0].Content, "Движения.Остатки") || res[1].Status != "error" {
				t.Errorf("tool results: %s", req.Content)
			}
			fmt.Fprintf(w, "data: {\"uuid\":\"a2\",\"role\":\"assistant\",\"content\":{\"content\":\"Готово, edit=%s.\"},\"finished\":true}\n\n", res[2].Status)
		default:
			w.WriteHeader(404)
		}
	}))
	defer api.Close()

	acc, _ := newFileAccess([]string{root}, []string{out})
	s := newServer(naparnik.New(api.URL, "k"), true, acc, defaultConfig(), defaultTexts())
	if d := s.tool("naparnik_ask").Description; !strings.Contains(d, sl(root)) || !strings.Contains(d, "allow_write=true") {
		t.Errorf("ask description: %s", d)
	}

	ask := func(args map[string]any) string {
		raw, _ := json.Marshal(args)
		text, err := s.ask(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		return text
	}

	// Только чтение: правка отклоняется, файл не меняется.
	text := ask(map[string]any{"question": "Что делает проведение?"})
	if !strings.Contains(text, "edit=error") || !strings.Contains(text, "Доступ к файлам (чтение), обращений: 3") || strings.Contains(text, "Изменённые файлы") {
		t.Errorf("read-only ask:\n%s", text)
	}
	if raw, _ := os.ReadFile(target); !strings.Contains(string(raw), "Процедура А()") {
		t.Error("file must not change without allow_write")
	}

	// С записью: файл изменён, путь в отчёте.
	text = ask(map[string]any{"question": "Переименуй процедуру", "allow_write": true})
	if !strings.Contains(text, "edit=ok") || !strings.Contains(text, "Изменённые файлы") || !strings.Contains(text, target) {
		t.Errorf("write ask:\n%s", text)
	}
	if raw, _ := os.ReadFile(target); string(raw) != bom+"Процедура Б()\r\nКонецПроцедуры\r\n" {
		t.Errorf("target: %q", raw)
	}

	// use_files=false — инструментов нет.
	text = ask(map[string]any{"question": "Общий вопрос", "use_files": false, "allow_write": true})
	if !strings.Contains(text, "Без файлов.") || !strings.Contains(text, "allow_write проигнорирован") {
		t.Errorf("no files ask:\n%s", text)
	}

	want := [][]string{
		{"local_list_dir", "local_read_file", "local_find_files", "local_search_text"},
		{"local_list_dir", "local_read_file", "local_find_files", "local_search_text", "local_edit_file", "local_write_file"},
		nil,
	}
	if fmt.Sprint(toolsSeen) != fmt.Sprint(want) {
		t.Errorf("tools passed:\n%v\nwant\n%v", toolsSeen, want)
	}
}
