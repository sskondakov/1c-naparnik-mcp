// Package naparnik — клиент публичного API 1С:Напарника (https://code.1c.ai).
// Использует только стандартную библиотеку Go.
package naparnik

import "encoding/json"

// CreateConversationRequest — тело POST /public/v1/conversations.
type CreateConversationRequest struct {
	UILanguage          string  `json:"ui_language,omitempty"`          // "ru" | "en"
	ProgrammingLanguage string  `json:"programming_language,omitempty"` // "1c" | "java" | ...
	ScriptLanguage      string  `json:"script_language,omitempty"`      // язык комментариев к коду
	Title               *string `json:"title"`
}

// ConversationDetails — настройки диалога.
type ConversationDetails struct {
	UILanguage          string `json:"ui_language"`
	ScriptLanguage      string `json:"script_language"`
	ProgrammingLanguage string `json:"programming_language"`
}

// Conversation — диалог (ответ создания, элемент списка или полная информация).
type Conversation struct {
	UUID       string                `json:"uuid"`
	CreateTime string                `json:"create_time"`
	SkillName  string                `json:"skill_name,omitempty"`
	Title      *string               `json:"title,omitempty"`
	Details    *ConversationDetails  `json:"details,omitempty"`
	Messages   []ConversationMessage `json:"messages,omitempty"`
}

// ConversationList — ответ GET /public/v1/conversations.
type ConversationList struct {
	Conversations []Conversation `json:"conversations"`
}

// ConversationMessage — сообщение в истории диалога.
// Content: строка (user), AssistantContent (assistant) или ToolContent (tool).
type ConversationMessage struct {
	UUID       string          `json:"uuid"`
	ParentUUID *string         `json:"parent_uuid"`
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	CreateTime string          `json:"create_time"`
}

// FunctionCall — имя функции и аргументы (строка JSON).
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolCall — вызов инструмента ассистентом. Index заполнен в дельтах.
type ToolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Function FunctionCall `json:"function"`
}

// AssistantContent — содержимое сообщения ассистента (финальное или дельта).
type AssistantContent struct {
	Content          string     `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

// ClientTool — описание клиентского инструмента (JSON Schema в Parameters).
type ClientTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Статусы результата инструмента.
const (
	ToolStatusOK       = "ok"
	ToolStatusAccepted = "accepted"
	ToolStatusRejected = "rejected"
	ToolStatusError    = "error"
	ToolStatusTimeout  = "timeout"
	ToolStatusIgnored  = "ignored"
)

// ToolContent — результат выполнения инструмента (role: tool).
type ToolContent struct {
	Content           *string         `json:"content"`
	StructuredContent json.RawMessage `json:"structured_content,omitempty"`
	Details           json.RawMessage `json:"details,omitempty"`
	ToolCallID        string          `json:"tool_call_id"`
	Status            string          `json:"status"`
}

// MessageRequest — тело POST /public/v1/conversations/{id}/messages.
// Content: string для role=user, []ToolContent для role=tool.
type MessageRequest struct {
	ParentUUID *string      `json:"parent_uuid"`
	Role       string       `json:"role"`
	Content    any          `json:"content"`
	Tools      []ClientTool `json:"tools,omitempty"`
	NoGenerate bool         `json:"no_generate,omitempty"`
}

// StreamPacket — один SSE-пакет потока сообщений. Неизвестные поля игнорируются.
type StreamPacket struct {
	UUID         string            `json:"uuid"`
	ParentUUID   *string           `json:"parent_uuid"`
	Role         *string           `json:"role"`
	Content      json.RawMessage   `json:"content"`
	ContentDelta *AssistantContent `json:"content_delta"`
	Finished     bool              `json:"finished"`
	Error        json.RawMessage   `json:"error"`
	ErrorType    string            `json:"error_type"`
}

// ContextTokens — заполненность контекста диалога.
type ContextTokens struct {
	TokenCount       int `json:"token_count"`
	MaxContextLength int `json:"max_context_length"`
}

// CompactResult — ответ управляемой компактизации.
type CompactResult struct {
	Success bool `json:"success"`
	Result  struct {
		Status string `json:"status"` // compact_finished | compact_limited
	} `json:"result"`
}

// BudgetWindow — состояние одного лимита.
type BudgetWindow struct {
	UsedPercent float64 `json:"used_percent"`
	ResetsAt    *string `json:"resets_at"`
}

// Budget — ответ GET /public/v1/budget.
type Budget struct {
	Limits struct {
		Sliding5h BudgetWindow `json:"sliding_5h"`
		Weekly    BudgetWindow `json:"weekly"`
	} `json:"limits"`
}

// isNull сообщает, что RawMessage пуст или равен JSON null.
func isNull(r json.RawMessage) bool {
	return len(r) == 0 || string(r) == "null"
}
