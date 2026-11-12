package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"slices"
	"sync"
)

// Версии протокола MCP, которые понимает сервер (первая — предпочтительная).
var supportedProtocols = []string{"2025-06-18", "2025-11-25", "2025-03-26", "2024-11-05"}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// rpc — транспорт MCP по stdio: JSON-RPC 2.0, одно сообщение на строку.
type rpc struct {
	srv *server

	outMu sync.Mutex
	out   io.Writer

	inflightMu sync.Mutex
	inflight   map[string]context.CancelFunc
}

func newRPC(srv *server, out io.Writer) *rpc {
	return &rpc{srv: srv, out: out, inflight: map[string]context.CancelFunc{}}
}

// serve читает запросы до EOF; tools/call выполняются параллельно.
func (r *rpc) serve(ctx context.Context, in io.Reader) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), int(r.srv.cfg.MCP.MaxMessageBytes))
	var wg sync.WaitGroup
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		r.dispatch(ctx, slices.Clone(line), &wg)
	}
	wg.Wait()
	return sc.Err()
}

func (r *rpc) dispatch(ctx context.Context, msg []byte, wg *sync.WaitGroup) {
	if msg[0] == '[' {
		r.send(rpcResponse{ID: json.RawMessage("null"), Error: &rpcError{codeInvalidRequest, "пакетные запросы не поддерживаются"}})
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(msg, &req); err != nil {
		r.send(rpcResponse{ID: json.RawMessage("null"), Error: &rpcError{codeParseError, "parse error: " + err.Error()}})
		return
	}
	if req.Method == "" {
		return // ответ клиента на наш запрос — сервер запросов не отправляет
	}
	isNotification := len(req.ID) == 0

	switch req.Method {
	case "initialize":
		r.reply(req.ID, r.initialize(req.Params))
	case "ping":
		r.reply(req.ID, struct{}{})
	case "tools/list":
		r.reply(req.ID, map[string]any{"tools": r.srv.tools})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			r.fail(req.ID, codeInvalidParams, "ожидаются params.name и params.arguments")
			return
		}
		tool := r.srv.tool(p.Name)
		if tool == nil {
			r.fail(req.ID, codeInvalidParams, "неизвестный инструмент: "+p.Name)
			return
		}
		cctx, cancel := context.WithCancel(ctx)
		key := string(req.ID)
		r.inflightMu.Lock()
		r.inflight[key] = cancel
		r.inflightMu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				r.inflightMu.Lock()
				delete(r.inflight, key)
				r.inflightMu.Unlock()
				cancel()
			}()
			res := r.srv.call(cctx, tool, p.Arguments)
			if cctx.Err() != nil && ctx.Err() == nil {
				return // запрос отменён клиентом — ответ не отправляем
			}
			r.reply(req.ID, res)
		}()
	case "notifications/cancelled":
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(req.Params, &p) == nil {
			r.inflightMu.Lock()
			if cancel, ok := r.inflight[string(p.RequestID)]; ok {
				cancel()
			}
			r.inflightMu.Unlock()
		}
	default:
		if !isNotification {
			r.fail(req.ID, codeMethodNotFound, "метод не поддерживается: "+req.Method)
		}
	}
}

func (r *rpc) initialize(params json.RawMessage) any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	v := supportedProtocols[0]
	if slices.Contains(supportedProtocols, p.ProtocolVersion) {
		v = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": v,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": serverName, "version": version},
		"instructions":    r.srv.instructions(),
	}
}

func (r *rpc) reply(id json.RawMessage, result any) {
	r.send(rpcResponse{ID: id, Result: result})
}

func (r *rpc) fail(id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		return
	}
	r.send(rpcResponse{ID: id, Error: &rpcError{code, msg}})
}

func (r *rpc) send(resp rpcResponse) {
	resp.JSONRPC = "2.0"
	// Без HTML-экранирования: код 1С с < > & остаётся читаемым. Encode добавляет '\n'.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(resp); err != nil {
		log.Printf("ошибка сериализации ответа: %v", err)
		return
	}
	r.outMu.Lock()
	defer r.outMu.Unlock()
	if _, err := r.out.Write(buf.Bytes()); err != nil {
		log.Printf("ошибка записи в stdout: %v", err)
	}
}
