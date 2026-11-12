package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"1c-naparnik-mcp/naparnik"
)

// Клиентские инструменты, которые получает Напарник: чтение (и, если разрешено, запись) файлов проекта.
// Разрешённые каталоги задаёт только пользователь в настройках сервера (NAPARNIK_FILES_ROOT / NAPARNIK_WRITE_ROOT).
// Напарник работает с настоящими абсолютными путями (разделитель /) — теми же, что видят агент и пользователь.
// Ссылки (symlink/junction) внутри каталогов разрешены: выгрузки 1С часто собраны из junction на другие диски.

// Лимиты — в cfg.json (секция files), тексты описаний — в tools.json (секция naparnik).

// fileAccess — каталоги, разрешённые пользователем в настройках сервера (абсолютные, без вложенных дублей).
type fileAccess struct {
	read  []string
	write []string // каждый внутри одного из read
}

// newFileAccess проверяет списки каталогов; возвращает предупреждения для журнала.
func newFileAccess(readList, writeList []string) (fileAccess, []string) {
	read, warns := parseDirs(readList)
	write, w2 := parseDirs(writeList)
	warns = append(warns, w2...)
	var a fileAccess
	a.read = read
	for _, w := range write {
		if slices.ContainsFunc(read, func(r string) bool { return within(r, w) }) {
			a.write = append(a.write, w)
		} else {
			warns = append(warns, "каталог записи "+w+" не входит ни в один каталог чтения (NAPARNIK_FILES_ROOT) — игнорируется")
		}
	}
	return a, warns
}

func parseDirs(list []string) ([]string, []string) {
	var dirs, warns []string
	for _, d := range list {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		abs, err := filepath.Abs(d)
		if err != nil {
			warns = append(warns, fmt.Sprintf("каталог %q: %v — пропущен", d, err))
			continue
		}
		if st, err := os.Stat(abs); err != nil || !st.IsDir() {
			warns = append(warns, "каталог "+abs+" не существует или не каталог — пропущен")
			continue
		}
		dirs = append(dirs, filepath.Clean(abs))
	}
	// Вложенные каталоги и дубли не нужны: они доступны через внешний.
	var out []string
	for i, d := range dirs {
		nested := false
		for j, o := range dirs {
			if i != j && within(o, d) && (!within(d, o) || j < i) {
				nested = true
				break
			}
		}
		if !nested {
			out = append(out, d)
		}
	}
	return out, warns
}

// within сообщает, что p совпадает с root или лежит внутри него (на Windows без учёта регистра).
func within(root, p string) bool {
	r, err := filepath.Rel(root, p)
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) && !filepath.IsAbs(r)
}

func slash(p string) string { return filepath.ToSlash(p) }

type fileTools struct {
	roots      []string // каталоги чтения
	writeRoots []string // каталоги записи; пусто — запись в этом вопросе не разрешена
	lim        *filesConfig
	tx         *texts

	mu      sync.Mutex
	log     []string // журнал обращений для отчёта агенту
	changed []string // изменённые файлы (абсолютные пути)
}

func newFileTools(roots, writeRoots []string, lim *filesConfig, tx *texts) *fileTools {
	return &fileTools{roots: roots, writeRoots: writeRoots, lim: lim, tx: tx}
}

func joinSlash(list []string) string {
	out := make([]string, len(list))
	for i, p := range list {
		out[i] = slash(p)
	}
	return strings.Join(out, ", ")
}

// clientTools — инструменты для 1С:Напарника. Схемы параметров — в коде, тексты — в tools.json.
func (f *fileTools) clientTools() []naparnik.ClientTool {
	env := newTextEnv(&config{Files: *f.lim}, f.roots, f.writeRoots)
	type param struct{ name, typ string }
	tool := func(name string, params []param, required ...string) naparnik.ClientTool {
		d := f.tx.Naparnik.tool(name, env)
		props := make([]string, 0, len(params)) // порядок параметров сохраняется
		for _, p := range params {
			prop := map[string]string{"type": p.typ}
			if desc := d.Params[p.name]; desc != "" {
				prop["description"] = desc
			}
			k, _ := json.Marshal(p.name)
			v, _ := json.Marshal(prop)
			props = append(props, string(k)+":"+string(v))
		}
		sch := `{"type":"object","properties":{` + strings.Join(props, ",") + "}"
		if len(required) > 0 {
			b, _ := json.Marshal(required)
			sch += `,"required":` + string(b)
		}
		return naparnik.ClientTool{Name: name, Description: d.Description, Parameters: json.RawMessage(sch + "}")}
	}
	tools := []naparnik.ClientTool{
		tool("local_list_dir", []param{{"path", "string"}, {"filter", "string"}, {"offset", "integer"}}),
		tool("local_read_file", []param{{"path", "string"}, {"start_line", "integer"}, {"max_lines", "integer"}}, "path"),
		tool("local_find_files", []param{{"pattern", "string"}, {"path", "string"}}, "pattern"),
		tool("local_search_text", []param{{"query", "string"}, {"path", "string"}, {"file_pattern", "string"}, {"context_lines", "integer"}}, "query"),
	}
	if len(f.writeRoots) > 0 {
		tools = append(tools,
			tool("local_edit_file", []param{{"path", "string"}, {"old_text", "string"}, {"new_text", "string"}, {"replace_all", "boolean"}}, "path", "old_text", "new_text"),
			tool("local_write_file", []param{{"path", "string"}, {"content", "string"}}, "path", "content"),
		)
	}
	return tools
}

func (f *fileTools) has(name string) bool {
	switch name {
	case "local_list_dir", "local_read_file", "local_find_files", "local_search_text", "local_edit_file", "local_write_file":
		return true
	}
	return false
}

// call выполняет инструмент. argsJSON — строка JSON из tool_calls.
func (f *fileTools) call(ctx context.Context, name, argsJSON string) (string, error) {
	var a struct {
		Path         string  `json:"path"`
		Filter       string  `json:"filter"`
		Offset       int     `json:"offset"`
		StartLine    int     `json:"start_line"`
		MaxLines     int     `json:"max_lines"`
		Pattern      string  `json:"pattern"`
		Query        string  `json:"query"`
		FilePattern  string  `json:"file_pattern"`
		ContextLines *int    `json:"context_lines"`
		OldText      string  `json:"old_text"`
		NewText      string  `json:"new_text"`
		ReplaceAll   bool    `json:"replace_all"`
		Content      *string `json:"content"`
	}
	if s := strings.TrimSpace(argsJSON); s != "" && s != "null" {
		if err := json.Unmarshal([]byte(s), &a); err != nil {
			return "", fmt.Errorf("некорректные аргументы (ожидается JSON): %v", err)
		}
	}
	where := slash(strings.TrimSpace(a.Path))
	if where == "" {
		where = "(все каталоги)"
	}
	note := func(s string) {
		f.mu.Lock()
		f.log = append(f.log, s)
		f.mu.Unlock()
	}
	switch name {
	case "local_list_dir":
		note("list " + where)
		return f.listDir(a.Path, a.Filter, a.Offset)
	case "local_read_file":
		note("read " + where)
		return f.readFile(a.Path, a.StartLine, a.MaxLines)
	case "local_find_files":
		note(fmt.Sprintf("find %q в %s", a.Pattern, where))
		return f.findFiles(ctx, a.Pattern, a.Path)
	case "local_search_text":
		note(fmt.Sprintf("search %q в %s", a.Query, where))
		cl := f.lim.SearchContext
		if a.ContextLines != nil {
			cl = *a.ContextLines
		}
		return f.searchText(ctx, a.Query, a.Path, a.FilePattern, cl)
	case "local_edit_file":
		note("edit " + where)
		return f.editFile(a.Path, a.OldText, a.NewText, a.ReplaceAll)
	case "local_write_file":
		note("write " + where)
		if a.Content == nil {
			return "", errors.New("не указан content")
		}
		return f.writeFile(a.Path, *a.Content)
	}
	return "", fmt.Errorf("неизвестный инструмент %s", name)
}

// resolve приводит путь от Напарника к абсолютному пути внутри одного из каталогов чтения.
// Пустой путь возвращает "" — «все каталоги».
func (f *fileTools) resolve(p string) (string, error) {
	p = strings.TrimSpace(strings.ReplaceAll(p, `\`, "/"))
	if p == "" {
		return "", nil
	}
	native := filepath.FromSlash(p)
	if !filepath.IsAbs(native) {
		if len(f.roots) != 1 {
			return "", fmt.Errorf("нужен абсолютный путь; доступные каталоги: %s", joinSlash(f.roots))
		}
		native = filepath.Join(f.roots[0], native)
	}
	native = filepath.Clean(native)
	for _, r := range f.roots {
		if within(r, native) {
			return native, nil
		}
	}
	return "", fmt.Errorf("путь %s вне доступных каталогов (%s)", slash(native), joinSlash(f.roots))
}

// isDir учитывает ссылки и junction: они не помечены как каталоги в DirEntry.
func isDir(p string, e fs.DirEntry) bool {
	if e.IsDir() {
		return true
	}
	if e.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
		st, err := os.Stat(p)
		return err == nil && st.IsDir()
	}
	return false
}

func (f *fileTools) canWrite(p string) bool {
	return slices.ContainsFunc(f.writeRoots, func(w string) bool { return within(w, p) })
}

func (f *fileTools) listDir(p, filter string, offset int) (string, error) {
	dir, err := f.resolve(p)
	if err != nil {
		return "", err
	}
	if dir == "" {
		var b strings.Builder
		b.WriteString("Доступные каталоги проекта:\n")
		for _, r := range f.roots {
			b.WriteString(slash(r) + "/")
			if f.canWrite(r) {
				b.WriteString(" (запись разрешена)")
			}
			b.WriteString("\n")
		}
		for _, w := range f.writeRoots {
			if !slices.Contains(f.roots, w) {
				b.WriteString("Запись разрешена в: " + slash(w) + "/\n")
			}
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("не удалось прочитать каталог %s: %v", slash(dir), pathErr(err))
	}
	filter = strings.ToLower(strings.TrimSpace(filter))
	var lines []string
	for _, e := range entries {
		if filter != "" && !strings.Contains(strings.ToLower(e.Name()), filter) {
			continue
		}
		if isDir(filepath.Join(dir, e.Name()), e) {
			lines = append(lines, e.Name()+"/")
		} else if info, err := e.Info(); err == nil {
			lines = append(lines, fmt.Sprintf("%s (%s)", e.Name(), humanSize(info.Size())))
		} else {
			lines = append(lines, e.Name())
		}
	}
	total := len(lines)
	offset = min(max(offset, 0), total)
	end := min(offset+f.lim.ListPage, total)
	var b strings.Builder
	fmt.Fprintf(&b, "Каталог %s/: %d элементов", slash(dir), total)
	if filter != "" {
		fmt.Fprintf(&b, " (filter %q)", filter)
	}
	if total == 0 {
		return b.String(), nil
	}
	fmt.Fprintf(&b, ", показаны %d–%d\n", offset+1, end)
	b.WriteString(strings.Join(lines[offset:end], "\n"))
	if end < total {
		fmt.Fprintf(&b, "\n… ещё %d; следующая страница: offset=%d (или уточните filter)", total-end, end)
	}
	return b.String(), nil
}

// loadText читает текстовый файл (для чтения и правки).
func (f *fileTools) loadText(p string) (raw []byte, text string, err error) {
	st, err := os.Stat(p)
	if err != nil {
		return nil, "", fmt.Errorf("файл %s: %v", slash(p), pathErr(err))
	}
	if st.IsDir() {
		return nil, "", fmt.Errorf("%s — каталог, используйте local_list_dir", slash(p))
	}
	if st.Size() > int64(f.lim.MaxFileSize) {
		return nil, "", fmt.Errorf("файл %s слишком большой (%s)", slash(p), humanSize(st.Size()))
	}
	raw, err = os.ReadFile(p)
	if err != nil {
		return nil, "", fmt.Errorf("файл %s: %v", slash(p), pathErr(err))
	}
	text, ok := decodeText(raw)
	if !ok {
		return nil, "", fmt.Errorf("файл %s не похож на текстовый", slash(p))
	}
	return raw, text, nil
}

func (f *fileTools) readFile(p string, start, count int) (string, error) {
	abs, err := f.resolve(p)
	if err != nil {
		return "", err
	}
	if abs == "" {
		return "", errors.New("не указан path файла")
	}
	_, text, err := f.loadText(abs)
	if err != nil {
		return "", err
	}
	lines := splitLines(text)
	total := len(lines)
	if start < 1 {
		start = 1
	}
	if count <= 0 {
		count = f.lim.ReadDefaultLines
	}
	count = min(count, f.lim.ReadMaxLines)
	if start > total {
		return fmt.Sprintf("Файл %s: всего %d строк, start_line=%d за пределами файла", slash(abs), total, start), nil
	}
	var body strings.Builder
	end := start - 1
	for i := start - 1; i < total && i < start-1+count; i++ {
		line := fmt.Sprintf("%d\t%s\n", i+1, lines[i])
		if body.Len()+len(line) > int(f.lim.ReadMaxBytes) && end >= start {
			break
		}
		body.WriteString(line)
		end = i + 1
	}
	head := fmt.Sprintf("Файл %s, строки %d–%d из %d\n", slash(abs), start, end, total)
	tail := ""
	if end < total {
		tail = fmt.Sprintf("… продолжение: start_line=%d", end+1)
	}
	return head + body.String() + tail, nil
}

// splitLines делит текст на строки без \r; завершающий перевод строки не даёт пустой строки.
func splitLines(text string) []string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if n := len(lines); n > 1 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// decodeText убирает BOM, понимает UTF-16 LE (с BOM) и отсекает бинарные файлы.
func decodeText(b []byte) (string, bool) {
	switch {
	case len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF:
		b = b[3:]
	case len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE:
		u := make([]uint16, 0, len(b)/2)
		for i := 2; i+1 < len(b); i += 2 {
			u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
		}
		return string(utf16.Decode(u)), true
	}
	if !utf8.Valid(b) || strings.IndexByte(string(b[:min(len(b), 8000)]), 0) >= 0 {
		return "", false
	}
	return string(b), true
}

// walk обходит каталоги параллельно (не больше lim.Workers одновременных чтений каталогов),
// заходя и в ссылки на каталоги; скрытые каталоги пропускает. fn вызывается конкурентно.
// Обход прекращается при отмене ctx.
func (f *fileTools) walk(ctx context.Context, roots []string, fn func(p string, isDir bool)) {
	sem := make(chan struct{}, f.lim.Workers)
	var wg sync.WaitGroup
	var visit func(dir string, depth int)
	visit = func(dir string, depth int) {
		defer wg.Done()
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		var subdirs []string
		if entries, err := os.ReadDir(dir); err == nil {
			for _, e := range entries {
				if ctx.Err() != nil {
					break
				}
				p := filepath.Join(dir, e.Name())
				d := isDir(p, e)
				fn(p, d)
				if d && depth < f.lim.WalkMaxDepth && !strings.HasPrefix(e.Name(), ".") {
					subdirs = append(subdirs, p)
				}
			}
		}
		<-sem
		for _, d := range subdirs {
			wg.Add(1)
			go visit(d, depth+1)
		}
	}
	for _, r := range roots {
		wg.Add(1)
		go visit(r, 0)
	}
	wg.Wait()
}

// walkRoots возвращает каталоги для обхода: path или все каталоги чтения.
func (f *fileTools) walkRoots(p string) ([]string, string, error) {
	dir, err := f.resolve(p)
	if err != nil {
		return nil, "", err
	}
	if dir == "" {
		return f.roots, "все доступные каталоги", nil
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, "", fmt.Errorf("%s — не каталог или не существует", slash(dir))
	}
	return []string{dir}, slash(dir), nil
}

func nameMatcher(pattern string) (func(name string) bool, error) {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "" || pattern == "*" {
		return func(string) bool { return true }, nil
	}
	if !strings.ContainsAny(pattern, "*?[") {
		return func(n string) bool { return strings.Contains(strings.ToLower(n), pattern) }, nil
	}
	if _, err := path.Match(pattern, ""); err != nil {
		return nil, fmt.Errorf("некорректный шаблон %q", pattern)
	}
	return func(n string) bool {
		ok, _ := path.Match(pattern, strings.ToLower(n))
		return ok
	}, nil
}

func (f *fileTools) findFiles(ctx context.Context, pattern, p string) (string, error) {
	if strings.TrimSpace(pattern) == "" {
		return "", errors.New("не указан pattern")
	}
	dirs, where, err := f.walkRoots(p)
	if err != nil {
		return "", err
	}
	match, err := nameMatcher(pattern)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(f.lim.WalkTimeout))
	defer cancel()

	var mu sync.Mutex
	var hits []string
	full := false
	f.walk(ctx, dirs, func(fp string, isDir bool) {
		if !match(filepath.Base(fp)) {
			return
		}
		fp = slash(fp)
		if isDir {
			fp += "/"
		}
		mu.Lock()
		defer mu.Unlock()
		if len(hits) >= f.lim.FindMaxHits {
			full = true
			cancel()
			return
		}
		hits = append(hits, fp)
	})
	timedOut := !full && errors.Is(ctx.Err(), context.DeadlineExceeded)
	slices.Sort(hits)

	var b strings.Builder
	fmt.Fprintf(&b, "Поиск %q в %s: найдено %d\n", pattern, where, len(hits))
	b.WriteString(strings.Join(hits, "\n"))
	switch {
	case full:
		b.WriteString("\n… показаны не все результаты, уточните pattern или path")
	case timedOut:
		b.WriteString("\n… обход остановлен по времени, результаты неполные — сузьте path")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

type fileMatches struct {
	path  string
	block string
	n     int
}

func (f *fileTools) searchText(ctx context.Context, query, p, filePattern string, contextLines int) (string, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return "", errors.New("не указан query")
	}
	dirs, where, err := f.walkRoots(p)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(filePattern) == "" {
		filePattern = f.lim.SearchDefaultPattern
	}
	match, err := nameMatcher(filePattern)
	if err != nil {
		return "", err
	}
	contextLines = min(max(contextLines, 0), f.lim.SearchMaxContext)
	ctx, cancel := context.WithTimeout(ctx, time.Duration(f.lim.WalkTimeout))
	defer cancel()

	// Обход каталогов кладёт подходящие файлы в очередь, lim.Workers воркеров читают и ищут.
	jobs := make(chan string, f.lim.Workers*4)
	var (
		mu      sync.Mutex
		results []fileMatches
		total   int
		scanned int
		full    bool
		wg      sync.WaitGroup
	)
	for range f.lim.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for fp := range jobs {
				if ctx.Err() != nil {
					continue
				}
				block, n, read := f.searchFile(fp, q, contextLines)
				mu.Lock()
				if read {
					scanned++
				}
				if n > 0 {
					results = append(results, fileMatches{slash(fp), block, n})
					total += n
					if total >= f.lim.SearchMaxHits {
						full = true
						cancel()
					}
				}
				mu.Unlock()
			}
		}()
	}
	f.walk(ctx, dirs, func(fp string, isDir bool) {
		if isDir || !match(filepath.Base(fp)) {
			return
		}
		select {
		case jobs <- fp:
		case <-ctx.Done():
		}
	})
	close(jobs)
	wg.Wait()
	timedOut := !full && errors.Is(ctx.Err(), context.DeadlineExceeded)

	slices.SortFunc(results, func(a, b fileMatches) int { return strings.Compare(a.path, b.path) })
	var body strings.Builder
	shown, shownFiles, cut := 0, 0, false
	for _, r := range results {
		if shownFiles > 0 && (shown >= f.lim.SearchMaxHits || body.Len()+len(r.block) > int(f.lim.SearchMaxBytes)) {
			cut = true
			break
		}
		body.WriteString(r.block)
		shown += r.n
		shownFiles++
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Поиск %q в %s (%s", query, where, filePattern)
	if contextLines > 0 {
		fmt.Fprintf(&b, ", контекст ±%d", contextLines)
	}
	fmt.Fprintf(&b, "): совпадений %d в %d файлах, просмотрено файлов %d\n", shown, shownFiles, scanned)
	if shown > 0 {
		b.WriteString("Формат: «N: строка» — совпадение, «N- строка» — контекст, «--» — разрыв.\n")
	}
	b.WriteString(body.String())
	switch {
	case full || cut:
		b.WriteString("… показаны не все совпадения, уточните query или path")
	case timedOut:
		b.WriteString("… поиск остановлен по времени, результаты неполные — сузьте path")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// searchFile ищет q (в нижнем регистре) в файле и формирует блок в стиле grep с контекстом.
// read сообщает, был ли файл прочитан как текст.
func (f *fileTools) searchFile(fp, q string, contextLines int) (block string, n int, read bool) {
	if st, err := os.Stat(fp); err != nil || st.Size() > int64(f.lim.SearchFileSize) {
		return "", 0, false
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		return "", 0, false
	}
	text, ok := decodeText(raw)
	if !ok {
		return "", 0, false
	}
	if !strings.Contains(strings.ToLower(text), q) {
		return "", 0, true
	}
	lines := strings.Split(text, "\n")
	isMatch := map[int]bool{}
	var matches []int
	for i, l := range lines {
		if strings.Contains(strings.ToLower(l), q) {
			isMatch[i] = true
			matches = append(matches, i)
			if len(matches) >= f.lim.SearchMaxHits {
				break
			}
		}
	}
	var b strings.Builder
	b.WriteString("== " + slash(fp) + "\n")
	prevEnd := -1
	for _, m := range matches {
		from, to := max(m-contextLines, 0), min(m+contextLines, len(lines)-1)
		if from <= prevEnd {
			from = prevEnd + 1
		} else if prevEnd >= 0 {
			b.WriteString("--\n")
		}
		for i := from; i <= to; i++ {
			sep := "-"
			if isMatch[i] {
				sep = ":"
			}
			fmt.Fprintf(&b, "%d%s %s\n", i+1, sep, cutRunes(strings.TrimRight(lines[i], "\r"), f.lim.SearchLineCut))
		}
		prevEnd = max(prevEnd, to)
	}
	return b.String(), len(matches), true
}

func pathErr(err error) string {
	if errors.Is(err, fs.ErrNotExist) {
		return "не найден"
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

func humanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d Б", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f КБ", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f МБ", float64(n)/(1<<20))
}

func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
