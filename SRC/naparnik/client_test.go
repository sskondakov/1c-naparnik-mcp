package naparnik

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadSSE(t *testing.T) {
	in := ": comment\r\nevent: message\r\ndata: {\"a\":1}\r\n\r\ndata: line1\ndata: line2\n\ndata:{\"b\":2}"
	var got []string
	if err := ReadSSE(strings.NewReader(in), func(d []byte) error {
		got = append(got, string(d))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{`{"a":1}`, "line1\nline2", `{"b":2}`}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Поток: эхо → дельты текста → финал (исправленный) → второе сообщение с дельтами tool_call → финал.
const sampleStream = `data: {"uuid":"msg-1","parent_uuid":null,"role":"user","content":null,"content_delta":null,"finished":true}

data: {"uuid":"msg-2","parent_uuid":null,"role":null,"content_delta":{"content":"При"},"finished":false,"type":"delta"}

data: {"uuid":"msg-2","parent_uuid":null,"role":null,"content_delta":{"content":"вет"},"finished":false}

data: {"uuid":"msg-2","parent_uuid":"msg-1","role":"assistant","content":{"content":"Привет."},"finished":true}

data: {"uuid":"msg-3","role":null,"content_delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"search","arguments":"{\"q\""}}]},"finished":false}

data: {"uuid":"msg-3","role":null,"content_delta":{"tool_calls":[{"index":0,"function":{"name":null,"arguments":":1}"}}]},"finished":false}

`

func TestSendMessageStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/public/v1/conversations/c1/messages" {
			t.Errorf("path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer key" || r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("headers %v", r.Header)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sampleStream)
	}))
	defer srv.Close()

	c := New(srv.URL, "key")
	res, err := c.SendMessage(context.Background(), "c1", MessageRequest{Role: "user", Content: "hi"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.RequestUUIDs) != 1 || res.RequestUUIDs[0] != "msg-1" {
		t.Errorf("request uuids %v", res.RequestUUIDs)
	}
	if len(res.Messages) != 2 {
		t.Fatalf("messages %d", len(res.Messages))
	}
	m := res.Messages[0]
	if m.Content != "Привет." || m.ParentUUID != "msg-1" || !m.Finished {
		t.Errorf("msg-2 %+v", m)
	}
	tc := res.Last().ToolCalls
	if len(tc) != 1 || tc[0].ID != "call_1" || tc[0].Function.Name != "search" || tc[0].Function.Arguments != `{"q":1}` {
		t.Errorf("tool calls %+v", tc)
	}
	if res.Text() != "Привет." {
		t.Errorf("text %q", res.Text())
	}
}

func TestSendMessageErrorPacket(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"uuid\":\"m\",\"role\":\"user\",\"finished\":true}\n\n")
		fmt.Fprint(w, "data: {\"error\":\"limit\",\"error_type\":\"budget_exceeded\"}\n\n")
	}))
	defer srv.Close()

	_, err := New(srv.URL, "k").SendMessage(context.Background(), "c", MessageRequest{Role: "user", Content: "x"}, nil)
	ae, ok := err.(*APIError)
	if !ok || ae.StatusCode != 402 || ae.Type != "budget_exceeded" || ae.Message != "limit" {
		t.Fatalf("err %#v", err)
	}
}

func TestHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		fmt.Fprint(w, `{"error":{"id":"x"},"error_type":"resource_not_found"}`)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "k").GetConversation(context.Background(), "x")
	if ErrorType(err) != "resource_not_found" || !strings.Contains(err.Error(), `{"id":"x"}`) {
		t.Fatalf("err %v", err)
	}
}
