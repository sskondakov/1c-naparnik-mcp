package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"1c-naparnik-mcp/naparnik"
)

type toolHandler func(ctx context.Context, args json.RawMessage) (string, error)

// toolDef — описание MCP-инструмента.
type toolDef struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`

	timeout time.Duration
	handler toolHandler
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type server struct {
	api    *naparnik.Client
	hasKey bool
	tools  []*toolDef
	cfg    *config
	texts  *texts
	env    *textEnv // подстановки для текстов агента

	// files — каталоги, которые пользователь разрешил Напарнику читать и писать (NAPARNIK_FILES_ROOT / NAPARNIK_WRITE_ROOT).
	files fileAccess

	mu      sync.Mutex
	lastMsg map[string]string // conversation_uuid → uuid последнего сообщения ассистента
}

func newServer(api *naparnik.Client, hasKey bool, files fileAccess, cfg *config, tx *texts) *server {
	s := &server{api: api, hasKey: hasKey, files: files, cfg: cfg, texts: tx, lastMsg: map[string]string{}}
	s.env = newTextEnv(cfg, files.read, files.write)
	s.tools = s.defineTools()
	return s
}

// instructions — инструкция сервера для агента (initialize).
func (s *server) instructions() string { return s.texts.instructions(s.env) }

func (s *server) tool(name string) *toolDef {
	for _, t := range s.tools {
		if t.Name == name {
			return t
		}
	}
	return nil
}

func (s *server) call(ctx context.Context, t *toolDef, args json.RawMessage) toolResult {
	if !s.hasKey {
		return errResult(s.texts.agentError("no_api_key", s.env))
	}
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	text, err := t.handler(ctx, args)
	if err != nil {
		return errResult(err.Error())
	}
	return toolResult{Content: []textContent{{Type: "text", Text: text}}}
}

func errResult(msg string) toolResult {
	return toolResult{Content: []textContent{{Type: "text", Text: msg}}, IsError: true}
}

// --- схемы ---

func schema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// prop — свойство схемы параметров; описание берётся из текстов (tools.json) по имени параметра.
func prop(doc toolDoc, name, typ string, extra ...any) map[string]any {
	p := map[string]any{"type": typ}
	if d := doc.Params[name]; d != "" {
		p["description"] = d
	}
	for i := 0; i+1 < len(extra); i += 2 {
		p[extra[i].(string)] = extra[i+1]
	}
	return p
}

// defineTools — MCP-инструменты для агента. Схемы (имена и типы параметров) — в коде, тексты — в tools.json.
func (s *server) defineTools() []*toolDef {
	readOnly := map[string]any{"readOnlyHint": true, "openWorldHint": true}
	t := s.cfg.Timeouts
	def := func(name string, props func(d toolDoc) map[string]any, required []string, ann map[string]any, timeout duration, h toolHandler) *toolDef {
		d := s.texts.Agent.tool(name, s.env)
		return &toolDef{
			Name: name, Title: d.Title, Description: d.Description,
			InputSchema: schema(props(d), required...),
			Annotations: ann, timeout: time.Duration(timeout), handler: h,
		}
	}
	uuidOnly := func(d toolDoc) map[string]any {
		return map[string]any{"conversation_uuid": prop(d, "conversation_uuid", "string")}
	}
	uuidReq := []string{"conversation_uuid"}
	return []*toolDef{
		def("naparnik_ask", func(d toolDoc) map[string]any {
			return map[string]any{
				"question":             prop(d, "question", "string"),
				"conversation_uuid":    prop(d, "conversation_uuid", "string"),
				"use_files":            prop(d, "use_files", "boolean"),
				"allow_write":          prop(d, "allow_write", "boolean"),
				"programming_language": prop(d, "programming_language", "string"),
			}
		}, []string{"question"}, map[string]any{"readOnlyHint": false, "destructiveHint": false, "openWorldHint": true}, s.cfg.Ask.Timeout, s.ask),
		def("naparnik_list_conversations", func(d toolDoc) map[string]any {
			return map[string]any{
				"limit":  prop(d, "limit", "integer", "minimum", 1, "maximum", 100),
				"offset": prop(d, "offset", "integer", "minimum", 0),
			}
		}, nil, readOnly, t.Default, s.listConversations),
		def("naparnik_get_conversation", uuidOnly, uuidReq, readOnly, t.Default, s.getConversation),
		def("naparnik_delete_conversation", uuidOnly, uuidReq,
			map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": true, "openWorldHint": true}, t.Default, s.deleteConversation),
		def("naparnik_compact", func(d toolDoc) map[string]any {
			return map[string]any{
				"conversation_uuid": prop(d, "conversation_uuid", "string"),
				"instruction":       prop(d, "instruction", "string"),
			}
		}, uuidReq, map[string]any{"readOnlyHint": false, "destructiveHint": false, "openWorldHint": true}, t.Compact, s.compact),
		def("naparnik_context", uuidOnly, uuidReq, readOnly, t.Default, s.contextTokens),
		def("naparnik_budget", func(toolDoc) map[string]any { return map[string]any{} }, nil, readOnly, t.Default, s.budget),
	}
}

// --- обработчики ---

func decodeArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("некорректные аргументы: %v", err)
	}
	return nil
}

func requireUUID(raw json.RawMessage) (string, error) {
	var a struct {
		ConversationUUID string `json:"conversation_uuid"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	id := strings.TrimSpace(a.ConversationUUID)
	if id == "" {
		return "", errors.New("не указан conversation_uuid")
	}
	return id, nil
}

func (s *server) ask(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Question            string `json:"question"`
		ConversationUUID    string `json:"conversation_uuid"`
		ProgrammingLanguage string `json:"programming_language"`
		UseFiles            *bool  `json:"use_files"`
		AllowWrite          bool   `json:"allow_write"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Question) == "" {
		return "", errors.New("не указан question")
	}

	// Доступ к файлам: каталоги задаёт только пользователь (env), агент лишь включает/выключает.
	var files *fileTools
	var notes []string
	useFiles := a.UseFiles == nil || *a.UseFiles
	if useFiles && len(s.files.read) > 0 {
		var write []string
		if a.AllowWrite {
			write = s.files.write
		}
		files = newFileTools(s.files.read, write, &s.cfg.Files, s.texts)
	}
	if a.AllowWrite && (files == nil || len(s.files.write) == 0) {
		switch {
		case len(s.files.write) == 0:
			notes = append(notes, "allow_write проигнорирован: каталоги для записи не заданы в настройках сервера (NAPARNIK_WRITE_ROOT).")
		default:
			notes = append(notes, "allow_write проигнорирован: доступ к файлам выключен (use_files=false).")
		}
	}

	conv := strings.TrimSpace(a.ConversationUUID)
	isNew := conv == ""
	var parent *string
	if isNew {
		lang := strings.TrimSpace(a.ProgrammingLanguage)
		if lang == "" {
			lang = s.cfg.Ask.DefaultLanguage
		}
		c, err := s.api.CreateConversation(ctx, naparnik.CreateConversationRequest{
			UILanguage: "ru", ProgrammingLanguage: lang, ScriptLanguage: "ru",
		})
		if err != nil {
			return "", s.explain(ctx, "Не удалось создать диалог", err)
		}
		conv = c.UUID
	} else {
		p, err := s.parentFor(ctx, conv)
		if err != nil {
			return "", s.explain(ctx, "Не удалось получить диалог "+conv, err)
		}
		parent = p
	}

	req := naparnik.MessageRequest{ParentUUID: parent, Role: "user", Content: a.Question}
	if files != nil {
		req.Tools = files.clientTools()
	}
	var answer []string
	limitHit := false
	for round := 0; ; round++ {
		res, err := s.api.SendMessage(ctx, conv, req, nil)
		if t := res.Text(); t != "" {
			answer = append(answer, t)
		}
		if last := res.Last(); last != nil && last.UUID != "" {
			s.setLast(conv, last.UUID)
		}
		if err != nil {
			msg := s.explain(ctx, "Ошибка при получении ответа", err).Error()
			if len(answer) > 0 {
				msg += "\n\nЧастичный ответ:\n" + strings.Join(answer, "\n\n")
			}
			return "", errors.New(msg + "\n\nconversation_uuid: " + conv)
		}
		// Ответ завершился вызовом инструментов: свои (файлы) выполняем, серверные подтверждаем.
		last := res.Last()
		if last == nil || len(last.ToolCalls) == 0 || last.UUID == "" {
			break
		}
		if round >= s.cfg.Ask.MaxToolRounds {
			limitHit = true
			break
		}
		results := make([]naparnik.ToolContent, 0, len(last.ToolCalls))
		for _, tc := range last.ToolCalls {
			results = append(results, s.runToolCall(ctx, files, tc))
		}
		lastUUID := last.UUID
		req = naparnik.MessageRequest{ParentUUID: &lastUUID, Role: "tool", Content: results}
	}

	var b strings.Builder
	if len(answer) == 0 {
		b.WriteString("(1С:Напарник вернул пустой ответ)")
	} else {
		b.WriteString(strings.Join(answer, "\n\n"))
	}
	if limitHit {
		fmt.Fprintf(&b, "\n\n(Остановлено: достигнут лимит %d обращений к инструментам за один вопрос. Продолжите диалог уточняющим вопросом.)", s.cfg.Ask.MaxToolRounds)
	}
	b.WriteString("\n\n---")
	for _, n := range notes {
		b.WriteString("\n" + n)
	}
	if files != nil {
		mode := "чтение"
		if len(files.writeRoots) > 0 {
			mode = "чтение и запись"
		}
		fmt.Fprintf(&b, "\nДоступ к файлам (%s), обращений: %d", mode, len(files.log))
		for i, l := range files.log {
			if i == s.cfg.Ask.FileLogShow {
				fmt.Fprintf(&b, "\n  … ещё %d", len(files.log)-i)
				break
			}
			b.WriteString("\n  " + l)
		}
		if len(files.changed) > 0 {
			b.WriteString("\nИзменённые файлы (проверьте, например, git diff):")
			for _, c := range files.changed {
				b.WriteString("\n  " + c)
			}
		}
	}
	b.WriteString("\nconversation_uuid: " + conv + " (передайте в naparnik_ask, чтобы продолжить этот диалог)")

	xctx, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.Timeouts.Extra))
	defer cancel()
	if isNew {
		if title, err := s.api.GenerateTitle(xctx, conv); err == nil && title != "" {
			b.WriteString("\ntitle: " + title)
		}
	}
	if ct, err := s.api.ContextTokens(xctx, conv, ""); err == nil && ct.MaxContextLength > 0 {
		b.WriteString("\ncontext: " + formatContext(ct))
	}
	return b.String(), nil
}

// runToolCall отвечает на один вызов инструмента Напарником.
func (s *server) runToolCall(ctx context.Context, files *fileTools, tc naparnik.ToolCall) naparnik.ToolContent {
	res := naparnik.ToolContent{ToolCallID: tc.ID}
	name := tc.Function.Name
	switch {
	case files != nil && files.has(name):
		out, err := files.call(ctx, name, tc.Function.Arguments)
		if err != nil {
			msg := err.Error()
			res.Status, res.Content = naparnik.ToolStatusError, &msg
		} else {
			res.Status, res.Content = naparnik.ToolStatusOK, &out
		}
	case strings.HasPrefix(name, "local_"):
		msg := "Доступ к файлам в этом вопросе не предоставлен."
		res.Status, res.Content = naparnik.ToolStatusError, &msg
	default:
		// Серверный инструмент Напарника — подтверждаем выполнение.
		res.Status = naparnik.ToolStatusAccepted
	}
	return res
}

// parentFor возвращает uuid сообщения, к которому крепится следующий вопрос.
func (s *server) parentFor(ctx context.Context, conv string) (*string, error) {
	s.mu.Lock()
	p, ok := s.lastMsg[conv]
	s.mu.Unlock()
	if ok {
		return &p, nil
	}
	c, err := s.api.GetConversation(ctx, conv)
	if err != nil {
		return nil, err
	}
	for i := len(c.Messages) - 1; i >= 0; i-- {
		if c.Messages[i].Role == "assistant" {
			return &c.Messages[i].UUID, nil
		}
	}
	if n := len(c.Messages); n > 0 {
		return &c.Messages[n-1].UUID, nil
	}
	return nil, nil
}

func (s *server) setLast(conv, msg string) {
	s.mu.Lock()
	s.lastMsg[conv] = msg
	s.mu.Unlock()
}

func (s *server) forget(conv string) {
	s.mu.Lock()
	delete(s.lastMsg, conv)
	s.mu.Unlock()
}

func (s *server) listConversations(ctx context.Context, raw json.RawMessage) (string, error) {
	a := struct {
		Limit  int `json:"limit"`
		Offset int `json:"offset"`
	}{Limit: 10}
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	a.Limit = min(max(a.Limit, 1), 100)
	a.Offset = max(a.Offset, 0)
	list, err := s.api.ListConversations(ctx, a.Limit, a.Offset)
	if err != nil {
		return "", s.explain(ctx, "Не удалось получить список диалогов", err)
	}
	if len(list) == 0 {
		return "Диалогов нет.", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Диалоги (offset %d, получено %d):\n", a.Offset, len(list))
	for _, c := range list {
		fmt.Fprintf(&b, "- %s | %s | %s\n", c.UUID, c.CreateTime, titleOf(c.Title))
	}
	if len(list) == a.Limit {
		fmt.Fprintf(&b, "Возможно, есть ещё — следующая страница: offset %d.", a.Offset+a.Limit)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func (s *server) getConversation(ctx context.Context, raw json.RawMessage) (string, error) {
	id, err := requireUUID(raw)
	if err != nil {
		return "", err
	}
	c, err := s.api.GetConversation(ctx, id)
	if err != nil {
		return "", s.explain(ctx, "Не удалось получить диалог", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Диалог %s\nНазвание: %s\nСоздан: %s\n", c.UUID, titleOf(c.Title), c.CreateTime)
	if d := c.Details; d != nil {
		fmt.Fprintf(&b, "Язык программирования: %s, язык интерфейса: %s, комментарии: %s\n", d.ProgrammingLanguage, d.UILanguage, d.ScriptLanguage)
	}
	fmt.Fprintf(&b, "Сообщений: %d\n", len(c.Messages))
	for _, m := range c.Messages {
		fmt.Fprintf(&b, "\n### %s (%s)\n%s\n", roleName(m.Role), m.CreateTime, messageText(m, s.cfg.History.ToolResultChars))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func (s *server) deleteConversation(ctx context.Context, raw json.RawMessage) (string, error) {
	id, err := requireUUID(raw)
	if err != nil {
		return "", err
	}
	if err := s.api.DeleteConversation(ctx, id); err != nil {
		return "", s.explain(ctx, "Не удалось удалить диалог", err)
	}
	s.forget(id)
	return "Диалог " + id + " удалён.", nil
}

func (s *server) compact(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		ConversationUUID string `json:"conversation_uuid"`
		Instruction      string `json:"instruction"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	id := strings.TrimSpace(a.ConversationUUID)
	if id == "" {
		return "", errors.New("не указан conversation_uuid")
	}
	r, err := s.api.Compact(ctx, id, a.Instruction)
	if err != nil {
		return "", s.explain(ctx, "Не удалось сжать диалог", err)
	}
	s.forget(id) // после сжатия родительское сообщение берём заново из истории
	status := r.Result.Status
	switch status {
	case "compact_finished":
		status = "сжатие выполнено"
	case "compact_limited":
		status = "сжатие выполнено частично (ограничено)"
	}
	out := "Диалог " + id + ": " + status
	if !r.Success {
		out = "Сжатие не удалось. Статус: " + r.Result.Status
	}
	xctx, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.Timeouts.Extra))
	defer cancel()
	if ct, err := s.api.ContextTokens(xctx, id, ""); err == nil && ct.MaxContextLength > 0 {
		out += "\nКонтекст: " + formatContext(ct)
	}
	return out, nil
}

func (s *server) contextTokens(ctx context.Context, raw json.RawMessage) (string, error) {
	id, err := requireUUID(raw)
	if err != nil {
		return "", err
	}
	ct, err := s.api.ContextTokens(ctx, id, "")
	if err != nil {
		return "", s.explain(ctx, "Не удалось получить контекст", err)
	}
	return "Контекст диалога " + id + ": " + formatContext(ct), nil
}

func (s *server) budget(ctx context.Context, _ json.RawMessage) (string, error) {
	b, err := s.api.Budget(ctx)
	if err != nil {
		return "", s.explain(ctx, "Не удалось получить лимиты", err)
	}
	return formatBudget(b), nil
}

// --- форматирование ---

func titleOf(t *string) string {
	if t == nil || *t == "" {
		return "(без названия)"
	}
	return *t
}

func roleName(r string) string {
	switch r {
	case "user":
		return "Пользователь"
	case "assistant":
		return "1С:Напарник"
	case "tool":
		return "Инструмент"
	}
	return r
}

// messageText — текст сообщения истории; результаты инструментов обрезаются до toolChars символов.
func messageText(m naparnik.ConversationMessage, toolChars int) string {
	switch m.Role {
	case "user":
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			return s
		}
	case "assistant":
		var c naparnik.AssistantContent
		if json.Unmarshal(m.Content, &c) == nil {
			text := c.Content
			for _, tc := range c.ToolCalls {
				text += fmt.Sprintf("\n[вызов инструмента %s %s]", tc.Function.Name, tc.Function.Arguments)
			}
			return strings.TrimSpace(text)
		}
	case "tool":
		var tcs []naparnik.ToolContent
		if json.Unmarshal(m.Content, &tcs) != nil {
			var one naparnik.ToolContent
			if json.Unmarshal(m.Content, &one) == nil {
				tcs = []naparnik.ToolContent{one}
			}
		}
		if tcs != nil {
			var parts []string
			for _, t := range tcs {
				p := "[" + t.Status + "]"
				if t.Content != nil {
					p += " " + truncate(*t.Content, toolChars)
				}
				parts = append(parts, p)
			}
			return strings.Join(parts, "\n")
		}
	}
	return string(m.Content)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + fmt.Sprintf("… [обрезано, всего %d символов]", len(r))
}

func formatContext(ct *naparnik.ContextTokens) string {
	pct := 0.0
	if ct.MaxContextLength > 0 {
		pct = float64(ct.TokenCount) * 100 / float64(ct.MaxContextLength)
	}
	return fmt.Sprintf("занято %d из %d токенов (%.0f%%)", ct.TokenCount, ct.MaxContextLength, pct)
}

func formatBudget(b *naparnik.Budget) string {
	w := func(name string, x naparnik.BudgetWindow) string {
		s := fmt.Sprintf("%s: использовано %.1f%%", name, x.UsedPercent)
		if x.ResetsAt != nil && *x.ResetsAt != "" {
			s += ", сброс " + *x.ResetsAt
		}
		return s
	}
	return w("5-часовое окно", b.Limits.Sliding5h) + "\n" + w("Неделя", b.Limits.Weekly)
}

// explain превращает ошибку API в понятный текст с подсказкой.
func (s *server) explain(ctx context.Context, prefix string, err error) error {
	msg := prefix + ": " + err.Error()
	var ae *naparnik.APIError
	if !errors.As(err, &ae) {
		if ctx.Err() == context.DeadlineExceeded {
			msg += "\nПревышено время ожидания ответа."
		}
		return errors.New(msg)
	}
	switch {
	case ae.StatusCode == 401:
		msg += "\nПроверьте API-ключ в NAPARNIK_API_KEY."
	case ae.Type == "budget_exceeded":
		msg += "\nЛимит исчерпан."
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(s.cfg.Timeouts.Extra))
		defer cancel()
		if b, err := s.api.Budget(bctx); err == nil {
			msg += "\n" + formatBudget(b)
		}
	case ae.Type == "no_active_tariff":
		msg += "\nУ ключа нет активной опции (тарифа) 1С:Напарника."
	case ae.StatusCode == 404:
		msg += "\nДиалог не найден — проверьте conversation_uuid или начните новый диалог без него."
	case ae.StatusCode == 409:
		msg += "\nВ этом диалоге уже идёт генерация — дождитесь её завершения и повторите."
	case ae.StatusCode == 429:
		msg += "\nСлишком много запросов — повторите чуть позже."
	}
	return errors.New(msg)
}
