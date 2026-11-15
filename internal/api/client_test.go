package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "test-agent"), srv
}

func TestChatCompletion_Success(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		var body ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.User != "test-agent" {
			t.Errorf("user = %q, want test-agent", body.User)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"id":"ds-1","object":"chat.completion","created":1,"model":"deepseek-chat",
			"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)
	})

	resp, err := c.ChatCompletion(context.Background(), ChatRequest{
		Model:    "deepseek-chat",
		Messages: []Message{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "pong" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 2 {
		t.Fatalf("usage not parsed: %+v", resp.Usage)
	}
}

func TestChatCompletion_APIError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"bad model","type":"invalid_request_error","code":"model_not_found"}}`)
	})

	_, err := c.ChatCompletion(context.Background(), ChatRequest{Model: "nope"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected wrapped *APIError, got %T: %v", err, err)
	}
	if apiErr.Code != "model_not_found" {
		t.Errorf("code = %q, want model_not_found", apiErr.Code)
	}
}

func TestChatCompletionStream_ReasoningAndContent(t *testing.T) {
	const sse = "" +
		"data: {\"id\":\"ds-1\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"think \"}}]}\n\n" +
		"data: {\"id\":\"ds-1\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"more\"}}]}\n\n" +
		"data: {\"id\":\"ds-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"}}]}\n\n" +
		"data: {\"id\":\"ds-1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"

	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stream, err := c.ChatCompletionStream(ctx, ChatRequest{
		Model:    "deepseek-reasoner-search",
		Messages: []Message{{Role: "user", Content: "q"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer stream.Close()

	var reasoning, content, finish string
	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		d := chunk.Choices[0].Delta
		reasoning += d.ReasoningContent
		content += d.Content
		if fr := chunk.Choices[0].FinishReason; fr != nil {
			finish = *fr
		}
	}

	if reasoning != "think more" {
		t.Errorf("reasoning = %q, want %q", reasoning, "think more")
	}
	if content != "answer" {
		t.Errorf("content = %q, want %q", content, "answer")
	}
	if finish != "stop" {
		t.Errorf("finish = %q, want stop", finish)
	}
}

func TestChatCompletionStream_ToolCalls(t *testing.T) {
	// Shaped exactly like the probe result: one complete tool_call chunk.
	const sse = "" +
		"data: {\"id\":\"ds-1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"run_bash\",\"arguments\":\"{\\\"command\\\":\\\"df -h\\\"}\"}}]}}]}\n\n" +
		"data: {\"id\":\"ds-1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"

	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	})

	stream, err := c.ChatCompletionStream(context.Background(), ChatRequest{Model: "deepseek-chat"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer stream.Close()

	var calls []ToolCall
	var finish string
	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		calls = append(calls, chunk.Choices[0].Delta.ToolCalls...)
		if fr := chunk.Choices[0].FinishReason; fr != nil {
			finish = *fr
		}
	}

	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(calls))
	}
	if calls[0].Function.Name != "run_bash" {
		t.Errorf("name = %q, want run_bash", calls[0].Function.Name)
	}
	if !strings.Contains(calls[0].Function.Arguments, "df -h") {
		t.Errorf("arguments = %q, want contains df -h", calls[0].Function.Arguments)
	}
	if finish != "tool_calls" {
		t.Errorf("finish = %q, want tool_calls", finish)
	}
}

func TestChatCompletionStream_ContextCancel(t *testing.T) {
	started := make(chan struct{})
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		io.WriteString(w, "data: {\"id\":\"ds-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		close(started)
		<-r.Context().Done() // hold the stream until client cancels
	})

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := c.ChatCompletionStream(ctx, ChatRequest{Model: "x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer stream.Close()

	if _, err := stream.Next(); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	<-started
	cancel()

	if _, err := stream.Next(); err == nil {
		t.Fatal("expected error after cancel, got nil")
	}
}

func TestListSessions(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		io.WriteString(w, `{"agents":[{"agent":"dev-agent","session_id":"uuid-1","message_count":3,"account":"account_1","busy":false,"age_min":5}],"total":1}`)
	})

	resp, err := c.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Total != 1 || len(resp.Agents) != 1 {
		t.Fatalf("unexpected: %+v", resp)
	}
	if resp.Agents[0].SessionID == nil || *resp.Agents[0].SessionID != "uuid-1" {
		t.Errorf("session_id not parsed: %+v", resp.Agents[0])
	}
}

func TestResetSession(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/reset-session" {
			t.Errorf("method/path = %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("agent"); got != "my-agent" {
			t.Errorf("agent = %q, want my-agent", got)
		}
		io.WriteString(w, `{"status":"session_reset","agent":"my-agent","history_preserved":2}`)
	})

	resp, err := c.ResetSession(context.Background(), "my-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != "session_reset" {
		t.Errorf("status = %q", resp.Status)
	}
}
