package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"1c-naparnik-mcp/naparnik"
)

// fakeAPI имитирует API Напарника: создание диалога, поток с вызовом серверного инструмента, title, context.
func fakeAPI(t *testing.T) *httptest.Server {
	round := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/public/v1/conversations":
			fmt.Fprint(w, `{"uuid":"conv-1","create_time":"2026-08-25T10:00:00Z"}`)
		case r.Method == "POST" && r.URL.Path == "/public/v1/conversations/conv-1/messages":
			var req map[string]any
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &req)
			w.Header().Set("Content-Type", "text/event-stream")
			round++
			if round == 1 {
				if req["role"] != "user" || req["parent_uuid"] != nil {
					t.Errorf("first request %s", body)
				}
				fmt.Fprint(w, "data: {\"uuid\":\"u1\",\"role\":\"user\",\"finished\":true}\n\n")
				fmt.Fprint(w, "data: {\"uuid\":\"a1\",\"parent_uuid\":\"u1\",\"role\":\"assistant\",\"content\":{\"content\":\"Ищу.\",\"tool_calls\":[{\"id\":\"call_1\",\"function\":{\"name\":\"its\",\"arguments\":\"{}\"}}]},\"finished\":true}\n\n")
				return
			}
			if req["role"] != "tool" || req["parent_uuid"] != "a1" || !strings.Contains(string(body), `"status":"accepted"`) {
				t.Errorf("second request %s", body)
			}
			fmt.Fprint(w, "data: {\"uuid\":\"t1\",\"role\":\"tool\",\"finished\":true}\n\n")
			fmt.Fprint(w, "data: {\"uuid\":\"a2\",\"content_delta\":{\"content\":\"Отв\"},\"finished\":false}\n\n")
			fmt.Fprint(w, "data: {\"uuid\":\"a2\",\"parent_uuid\":\"t1\",\"role\":\"assistant\",\"content\":{\"content\":\"Ответ.\"},\"finished\":true}\n\n")
		case r.URL.Path == "/public/v1/conversations/conv-1/title":
			fmt.Fprint(w, `{"title":"Тест"}`)
		case r.URL.Path == "/public/v1/conversations/conv-1/context_tokens":
			fmt.Fprint(w, `{"token_count":1280,"max_context_length":128000}`)
		default:
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":"nope","error_type":"resource_not_found"}`)
		}
	}))
}

func runRPC(t *testing.T, s *server, lines ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := newRPC(s, &out).serve(context.Background(), strings.NewReader(strings.Join(lines, "\n"))); err != nil {
		t.Fatal(err)
	}
	var res []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("bad output line %q: %v", l, err)
		}
		res = append(res, m)
	}
	return res
}

func TestProtocol(t *testing.T) {
	s := newServer(naparnik.New("http://127.0.0.1:1", "k"), true, fileAccess{}, defaultConfig(), defaultTexts())
	out := runRPC(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":4,"method":"resources/list"}`,
		`not json`,
	)
	if len(out) != 5 {
		t.Fatalf("responses: %v", out)
	}
	if v := out[0]["result"].(map[string]any)["protocolVersion"]; v != "2025-03-26" {
		t.Errorf("protocolVersion %v", v)
	}
	if n := len(out[1]["result"].(map[string]any)["tools"].([]any)); n != 7 {
		t.Errorf("tools %d", n)
	}
	if _, ok := out[2]["result"]; !ok {
		t.Errorf("ping %v", out[2])
	}
	if code := out[3]["error"].(map[string]any)["code"]; code != float64(codeMethodNotFound) {
		t.Errorf("unknown method %v", out[3])
	}
	if code := out[4]["error"].(map[string]any)["code"]; code != float64(codeParseError) {
		t.Errorf("parse error %v", out[4])
	}
}

func TestAskFlow(t *testing.T) {
	api := fakeAPI(t)
	defer api.Close()
	s := newServer(naparnik.New(api.URL, "k"), true, fileAccess{}, defaultConfig(), defaultTexts())
	out := runRPC(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"naparnik_ask","arguments":{"question":"Как сделать запрос?"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"naparnik_get_conversation","arguments":{"conversation_uuid":"missing"}}}`,
	)
	if len(out) != 2 {
		t.Fatalf("responses: %v", out)
	}
	byID := map[float64]map[string]any{}
	for _, m := range out {
		byID[m["id"].(float64)] = m["result"].(map[string]any)
	}
	ask := byID[1]
	text := ask["content"].([]any)[0].(map[string]any)["text"].(string)
	for _, want := range []string{"Ищу.", "Ответ.", "conversation_uuid: conv-1", "title: Тест", "(1%)"} {
		if !strings.Contains(text, want) {
			t.Errorf("ask result missing %q:\n%s", want, text)
		}
	}
	if ask["isError"] == true {
		t.Errorf("ask isError")
	}
	if s.lastMsg["conv-1"] != "a2" {
		t.Errorf("lastMsg %v", s.lastMsg)
	}
	get := byID[2]
	if get["isError"] != true || !strings.Contains(get["content"].([]any)[0].(map[string]any)["text"].(string), "не найден") {
		t.Errorf("get missing: %v", get)
	}
}

func TestNoAPIKey(t *testing.T) {
	s := newServer(naparnik.New("", ""), false, fileAccess{}, defaultConfig(), defaultTexts())
	out := runRPC(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"naparnik_budget"}}`)
	if out[0]["result"].(map[string]any)["isError"] != true {
		t.Errorf("expected isError: %v", out[0])
	}
}
