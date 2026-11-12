package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Запись файлов Напарником: только внутри каталогов NAPARNIK_WRITE_ROOT и только если агент разрешил allow_write.

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// textFormat — кодировочные особенности файла, которые сохраняются при записи.
type textFormat struct {
	bom  bool
	crlf bool
}

// formatOf определяет формат существующего файла.
func formatOf(raw []byte) textFormat {
	return textFormat{bom: bytes.HasPrefix(raw, utf8BOM), crlf: bytes.Contains(raw, []byte("\r\n"))}
}

// formatForNew — формат нового файла: расширения из files.bom_crlf_extensions — UTF-8 с BOM и CRLF, как в выгрузке 1С.
func (f *fileTools) formatForNew(p string) textFormat {
	ext := filepath.Ext(p)
	if slices.ContainsFunc(f.lim.BomCrlfExtensions, func(e string) bool { return strings.EqualFold(e, ext) }) {
		return textFormat{bom: true, crlf: true}
	}
	return textFormat{}
}

func normalizeNL(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

func encodeText(text string, f textFormat) []byte {
	text = normalizeNL(text)
	if f.crlf {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	if f.bom {
		return append(slices.Clone(utf8BOM), text...)
	}
	return []byte(text)
}

// writable проверяет, что путь разрешён для записи, и возвращает абсолютный путь.
func (f *fileTools) writable(p string) (string, error) {
	if len(f.writeRoots) == 0 {
		return "", errors.New("запись файлов в этом вопросе не разрешена")
	}
	abs, err := f.resolve(p)
	if err != nil {
		return "", err
	}
	if abs == "" {
		return "", errors.New("не указан path файла")
	}
	for _, w := range f.writeRoots {
		if !within(w, abs) {
			continue
		}
		rel, _ := filepath.Rel(w, abs)
		if rel == "." {
			return "", fmt.Errorf("%s — каталог, укажите путь файла", filepath.ToSlash(abs))
		}
		for _, seg := range strings.Split(rel, string(filepath.Separator)) {
			if strings.HasPrefix(seg, ".") {
				return "", fmt.Errorf("запись в скрытые файлы и каталоги (%s) запрещена", seg)
			}
		}
		return abs, nil
	}
	return "", fmt.Errorf("запись в %s запрещена; разрешены только каталоги: %s", slash(abs), joinSlash(f.writeRoots))
}

// atomicWrite пишет во временный файл рядом и переименовывает его поверх целевого.
func atomicWrite(p string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".naparnik-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(name, perm)
	}
	if err == nil {
		err = os.Rename(name, p)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}

func (f *fileTools) markChanged(p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Contains(f.changed, p) {
		f.changed = append(f.changed, p)
	}
}

// existingText читает существующий файл для записи: формат, текст без BOM и права.
func (f *fileTools) existingText(abs string) (textFormat, string, fs.FileMode, error) {
	st, err := os.Stat(abs)
	if err != nil {
		return textFormat{}, "", 0, err
	}
	raw, text, err := f.loadText(abs)
	if err != nil {
		return textFormat{}, "", 0, err
	}
	if bytes.HasPrefix(raw, []byte{0xFF, 0xFE}) {
		return textFormat{}, "", 0, fmt.Errorf("файл %s в кодировке UTF-16 — запись не поддерживается", slash(abs))
	}
	return formatOf(raw), text, st.Mode().Perm(), nil
}

func (f *fileTools) writeFile(p, content string) (string, error) {
	abs, err := f.writable(p)
	if err != nil {
		return "", err
	}
	if len(content) > int(f.lim.WriteMaxBytes) {
		return "", fmt.Errorf("содержимое слишком большое (%s), максимум %s", humanSize(int64(len(content))), humanSize(int64(f.lim.WriteMaxBytes)))
	}
	format, perm, action := f.formatForNew(abs), fs.FileMode(0o644), "создан"
	if st, err := os.Stat(abs); err == nil {
		if st.IsDir() {
			return "", fmt.Errorf("%s — каталог", slash(abs))
		}
		format, _, perm, err = f.existingText(abs)
		if err != nil {
			return "", err
		}
		action = "перезаписан"
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("файл %s: %v", slash(abs), pathErr(err))
	}
	content = strings.TrimPrefix(content, string(utf8BOM))
	if err := atomicWrite(abs, encodeText(content, format), perm); err != nil {
		return "", fmt.Errorf("не удалось записать %s: %v", slash(abs), err)
	}
	f.markChanged(abs)
	return fmt.Sprintf("Файл %s %s: %d строк%s.", slash(abs), action, len(splitLines(normalizeNL(content))), formatNote(format)), nil
}

func formatNote(f textFormat) string {
	var parts []string
	if f.bom {
		parts = append(parts, "UTF-8 с BOM")
	} else {
		parts = append(parts, "UTF-8")
	}
	if f.crlf {
		parts = append(parts, "CRLF")
	} else {
		parts = append(parts, "LF")
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func (f *fileTools) editFile(p, oldText, newText string, replaceAll bool) (string, error) {
	abs, err := f.writable(p)
	if err != nil {
		return "", err
	}
	format, text, perm, err := f.existingText(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("файл %s не найден; для создания используйте local_write_file", slash(abs))
		}
		return "", err
	}
	text = normalizeNL(text)
	oldN, newN := normalizeNL(oldText), normalizeNL(newText)
	if oldN == "" {
		return "", errors.New("не указан old_text")
	}
	if oldN == newN {
		return "", errors.New("old_text и new_text совпадают — изменений нет")
	}
	count := strings.Count(text, oldN)
	switch {
	case count == 0:
		return "", fmt.Errorf("фрагмент old_text не найден в %s; перечитайте файл через local_read_file — old_text должен совпадать точно, без номеров строк", slash(abs))
	case count > 1 && !replaceAll:
		return "", fmt.Errorf("old_text встречается в %s %d раз; добавьте соседние строки, чтобы фрагмент был уникален, или укажите replace_all=true", slash(abs), count)
	}
	first := strings.Index(text, oldN)
	n := 1
	if replaceAll {
		n = -1
	}
	result := strings.Replace(text, oldN, newN, n)
	if len(result) > int(f.lim.WriteMaxBytes)*8 {
		return "", errors.New("результат правки слишком большой")
	}
	if err := atomicWrite(abs, encodeText(result, format), perm); err != nil {
		return "", fmt.Errorf("не удалось записать %s: %v", slash(abs), err)
	}
	f.markChanged(abs)

	// Фрагмент результата вокруг первой замены — чтобы Напарник видел итог.
	lines := splitLines(result)
	startLine := strings.Count(result[:first], "\n") // индекс с 0
	endLine := startLine + strings.Count(newN, "\n")
	from, to := max(startLine-f.lim.EditContextLines, 0), min(endLine+f.lim.EditContextLines, len(lines)-1)
	var b strings.Builder
	replaced := 1
	if replaceAll {
		replaced = count
	}
	fmt.Fprintf(&b, "Файл %s изменён: замен %d. Фрагмент после правки", slash(abs), replaced)
	if replaced > 1 {
		b.WriteString(" (первое вхождение)")
	}
	b.WriteString(":\n")
	for i := from; i <= to && i < len(lines); i++ {
		fmt.Fprintf(&b, "%d\t%s\n", i+1, lines[i])
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
