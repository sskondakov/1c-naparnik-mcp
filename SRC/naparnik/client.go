package naparnik

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL — базовый адрес публичного API.
const DefaultBaseURL = "https://code.1c.ai"

// DefaultAPIPrefix — префикс путей публичного API.
const DefaultAPIPrefix = "/public/v1"

// Client — клиент API Напарника. Таймауты задаются через context.
type Client struct {
	BaseURL    string
	APIPrefix  string // пусто — DefaultAPIPrefix
	APIKey     string
	UserAgent  string
	MaxSSELine int // максимальная длина строки потока; 0 — DefaultMaxSSELine
	HTTP       *http.Client

	// OnRequest (может быть nil) вызывается после каждого запроса к API, для журнала:
	// метод, путь без префикса, HTTP-код (0 — ответа нет), время с учётом чтения потока, ошибка.
	OnRequest func(method, path string, status int, dur time.Duration, err error)
}

func (c *Client) report(method, path string, status int, start time.Time, err error) {
	if c.OnRequest != nil {
		c.OnRequest(method, path, status, time.Since(start), err)
	}
}

// New создаёт клиент. Пустой baseURL заменяется на DefaultBaseURL.
func New(baseURL, apiKey string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		APIPrefix: DefaultAPIPrefix,
		APIKey:    apiKey,
		HTTP:      &http.Client{},
	}
}

// APIError — ошибка API в едином формате {error, error_type}.
type APIError struct {
	StatusCode int
	Type       string
	Message    string
}

func (e *APIError) Error() string {
	s := "HTTP " + strconv.Itoa(e.StatusCode)
	if e.Type != "" {
		s += " " + e.Type
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// ErrorType возвращает error_type ошибки API или "".
func ErrorType(err error) string {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Type
	}
	return ""
}

// statusByType — HTTP-статус для ошибок, пришедших внутри потока со статусом 200.
var statusByType = map[string]int{
	"no_token": 401, "invalid_token": 401, "token_not_found": 401,
	"budget_exceeded": 402, "no_active_tariff": 402,
	"access_denied": 403, "user_conversation_conflict": 403,
	"resource_not_found": 404, "conflict": 409, "validation": 422,
	"insufficient_context": 424, "rate_limit_exceeded": 429,
}

func errorFromBody(status int, b []byte) *APIError {
	e := &APIError{StatusCode: status}
	var body struct {
		Error     json.RawMessage `json:"error"`
		ErrorType string          `json:"error_type"`
	}
	if json.Unmarshal(b, &body) == nil && (!isNull(body.Error) || body.ErrorType != "") {
		e.Type = body.ErrorType
		var s string
		if json.Unmarshal(body.Error, &s) == nil {
			e.Message = s
		} else if !isNull(body.Error) {
			e.Message = string(body.Error)
		}
	} else {
		msg := strings.TrimSpace(string(b))
		if len(msg) > 500 {
			msg = msg[:500] + "…"
		}
		e.Message = msg
	}
	if e.StatusCode < 400 {
		if s, ok := statusByType[e.Type]; ok {
			e.StatusCode = s
		} else {
			e.StatusCode = 500
		}
	}
	return e
}

func (c *Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	prefix := c.APIPrefix
	if prefix == "" {
		prefix = DefaultAPIPrefix
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+strings.TrimRight(prefix, "/")+path, rd)
	if err != nil {
		return nil, err
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	return req, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) (err error) {
	start, status := time.Now(), 0
	defer func() { c.report(method, path, status, start, err) }()
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	status = resp.StatusCode

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return errorFromBody(resp.StatusCode, b)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("разбор ответа %s %s: %w", method, path, err)
	}
	return nil
}

func convPath(id string, suffix string) string {
	return "/conversations/" + url.PathEscape(id) + suffix
}

// CreateConversation создаёт диалог.
func (c *Client) CreateConversation(ctx context.Context, req CreateConversationRequest) (*Conversation, error) {
	var out Conversation
	if err := c.do(ctx, http.MethodPost, "/conversations", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListConversations возвращает список диалогов (limit 1–100, offset ≥ 0).
func (c *Client) ListConversations(ctx context.Context, limit, offset int) ([]Conversation, error) {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	q.Set("offset", strconv.Itoa(offset))
	var out ConversationList
	if err := c.do(ctx, http.MethodGet, "/conversations?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Conversations, nil
}

// GetConversation возвращает диалог с сообщениями.
func (c *Client) GetConversation(ctx context.Context, id string) (*Conversation, error) {
	var out Conversation
	if err := c.do(ctx, http.MethodGet, convPath(id, ""), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteConversation удаляет диалог.
func (c *Client) DeleteConversation(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, convPath(id, ""), nil, nil)
}

// GenerateTitle генерирует (или возвращает сохранённое) название диалога.
func (c *Client) GenerateTitle(ctx context.Context, id string) (string, error) {
	var out struct {
		Title string `json:"title"`
	}
	if err := c.do(ctx, http.MethodPost, convPath(id, "/title"), nil, &out); err != nil {
		return "", err
	}
	return out.Title, nil
}

// SetTitle задаёт название диалога вручную.
func (c *Client) SetTitle(ctx context.Context, id, title string) error {
	return c.do(ctx, http.MethodPut, convPath(id, "/title"), map[string]string{"title": title}, nil)
}

// Compact запускает управляемую компактизацию и ждёт её завершения.
func (c *Client) Compact(ctx context.Context, id, instruction string) (*CompactResult, error) {
	var out CompactResult
	body := map[string]string{"instruction": instruction}
	if err := c.do(ctx, http.MethodPost, convPath(id, "/compact"), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ContextTokens возвращает заполненность контекста. branch — "" (latest) или uuid сообщения.
func (c *Client) ContextTokens(ctx context.Context, id, branch string) (*ContextTokens, error) {
	p := convPath(id, "/context_tokens")
	if branch != "" {
		p += "?branch=" + url.QueryEscape(branch)
	}
	var out ContextTokens
	if err := c.do(ctx, http.MethodGet, p, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Budget возвращает состояние лимитов.
func (c *Client) Budget(ctx context.Context) (*Budget, error) {
	var out Budget
	if err := c.do(ctx, http.MethodGet, "/budget", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SendMessage отправляет сообщение и читает SSE-поток до конца.
// onPacket (может быть nil) вызывается для каждого пакета — например, для прогресса.
// При ошибке возвращается и частично собранный результат.
func (c *Client) SendMessage(ctx context.Context, id string, req MessageRequest, onPacket func(StreamPacket)) (res *StreamResult, err error) {
	path := convPath(id, "/messages")
	start, status := time.Now(), 0
	defer func() { c.report(http.MethodPost, path, status, start, err) }()
	hr, err := c.newRequest(ctx, http.MethodPost, path, req)
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Accept", "text/event-stream")

	resp, err := c.HTTP.Do(hr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	status = resp.StatusCode

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if t := bytes.TrimSpace(b); bytes.HasPrefix(t, []byte("data:")) {
			var apiErr *APIError
			_ = ReadSSESize(bytes.NewReader(t), c.MaxSSELine, func(d []byte) error {
				if apiErr == nil {
					apiErr = errorFromBody(resp.StatusCode, d)
				}
				return nil
			})
			if apiErr != nil {
				return nil, apiErr
			}
		}
		return nil, errorFromBody(resp.StatusCode, b)
	}

	acc := newAccumulator()
	err = ReadSSESize(resp.Body, c.MaxSSELine, func(d []byte) error {
		d = bytes.TrimSpace(d)
		if len(d) == 0 || string(d) == "[DONE]" {
			return nil
		}
		var p StreamPacket
		if err := json.Unmarshal(d, &p); err != nil {
			return fmt.Errorf("некорректный пакет потока: %w", err)
		}
		if !isNull(p.Error) || p.ErrorType != "" {
			return errorFromBody(resp.StatusCode, d)
		}
		if onPacket != nil {
			onPacket(p)
		}
		acc.add(p)
		return nil
	})
	return &acc.res, err
}
