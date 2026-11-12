package naparnik

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// DefaultMaxSSELine — максимальная длина одной строки потока по умолчанию.
const DefaultMaxSSELine = 16 << 20

// ReadSSE читает поток Server-Sent Events и вызывает fn с полем data каждого события.
// Многострочные data склеиваются через "\n"; комментарии и прочие поля пропускаются.
func ReadSSE(r io.Reader, fn func(data []byte) error) error {
	return ReadSSESize(r, DefaultMaxSSELine, fn)
}

// ReadSSESize — ReadSSE с заданной максимальной длиной строки (0 — DefaultMaxSSELine).
func ReadSSESize(r io.Reader, maxLine int, fn func(data []byte) error) error {
	if maxLine <= 0 {
		maxLine = DefaultMaxSSELine
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, min(64*1024, maxLine)), maxLine)

	var data []byte
	has := false
	flush := func() error {
		if !has {
			return nil
		}
		d := data
		data, has = nil, false
		return fn(d)
	}

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if line[0] == ':' {
			continue
		}
		field, value, found := bytes.Cut(line, []byte(":"))
		if !found {
			value = nil
		} else if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		if string(field) == "data" {
			if has {
				data = append(data, '\n')
			}
			data = append(data, value...)
			has = true
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return flush()
}

// AssistantMessage — собранное сообщение ассистента.
type AssistantMessage struct {
	UUID             string
	ParentUUID       string
	Content          string
	ReasoningContent string
	ToolCalls        []ToolCall
	Finished         bool
}

// StreamResult — итог потока сообщений.
type StreamResult struct {
	// RequestUUIDs — uuid сохранённых сообщений запроса (эхо user, подтверждения tool).
	RequestUUIDs []string
	// Messages — сообщения ассистента в порядке поступления.
	Messages []*AssistantMessage
}

// Last возвращает последнее сообщение ассистента или nil.
func (r *StreamResult) Last() *AssistantMessage {
	if r == nil || len(r.Messages) == 0 {
		return nil
	}
	return r.Messages[len(r.Messages)-1]
}

// Text склеивает текст всех сообщений ассистента.
func (r *StreamResult) Text() string {
	if r == nil {
		return ""
	}
	var parts []string
	for _, m := range r.Messages {
		if t := strings.TrimSpace(m.Content); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

// accumulator собирает сообщения ассистента из дельт и финальных пакетов.
type accumulator struct {
	res    StreamResult
	byUUID map[string]*AssistantMessage
	cur    *AssistantMessage
}

func newAccumulator() *accumulator {
	return &accumulator{byUUID: map[string]*AssistantMessage{}}
}

func (a *accumulator) add(p StreamPacket) {
	role := ""
	if p.Role != nil {
		role = *p.Role
	}
	// Эхо запроса пользователя и подтверждения tool-сообщений.
	if role != "" && role != "assistant" {
		if p.UUID != "" {
			a.res.RequestUUIDs = append(a.res.RequestUUIDs, p.UUID)
		}
		return
	}
	if p.ContentDelta == nil && !p.Finished {
		return
	}

	m := a.message(p.UUID)
	if p.ParentUUID != nil {
		m.ParentUUID = *p.ParentUUID
	}
	if d := p.ContentDelta; d != nil && !p.Finished {
		m.Content += d.Content
		m.ReasoningContent += d.ReasoningContent
		for _, tc := range d.ToolCalls {
			m.ToolCalls = mergeToolCall(m.ToolCalls, tc)
		}
	}
	if p.Finished {
		// Финальный content приоритетнее накопленных дельт.
		if !isNull(p.Content) {
			var c AssistantContent
			if err := json.Unmarshal(p.Content, &c); err == nil {
				m.Content = c.Content
				m.ReasoningContent = c.ReasoningContent
				m.ToolCalls = c.ToolCalls
			}
		}
		m.Finished = true
		a.cur = nil
	}
}

// message находит сообщение по uuid или текущее незавершённое, иначе создаёт новое.
func (a *accumulator) message(uuid string) *AssistantMessage {
	if uuid != "" {
		if m, ok := a.byUUID[uuid]; ok {
			return m
		}
	}
	if a.cur != nil && (uuid == "" || a.cur.UUID == "") {
		if uuid != "" {
			a.cur.UUID = uuid
			a.byUUID[uuid] = a.cur
		}
		return a.cur
	}
	m := &AssistantMessage{UUID: uuid}
	a.res.Messages = append(a.res.Messages, m)
	if uuid != "" {
		a.byUUID[uuid] = m
	}
	a.cur = m
	return m
}

// mergeToolCall добавляет дельту вызова инструмента (по index, иначе по id).
func mergeToolCall(calls []ToolCall, d ToolCall) []ToolCall {
	i := -1
	for j := range calls {
		if d.Index != nil && calls[j].Index != nil && *calls[j].Index == *d.Index {
			i = j
			break
		}
		if d.Index == nil && d.ID != "" && calls[j].ID == d.ID {
			i = j
			break
		}
	}
	if i < 0 {
		calls = append(calls, ToolCall{Index: d.Index})
		i = len(calls) - 1
	}
	c := &calls[i]
	if d.ID != "" {
		c.ID = d.ID
	}
	if d.Function.Name != "" {
		c.Function.Name = d.Function.Name
	}
	c.Function.Arguments += d.Function.Arguments
	return calls
}
