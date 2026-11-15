package api

import "encoding/json"

// ChatRequest is the body for POST /v1/chat/completions.
type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
	User     string    `json:"user,omitempty"`
	Tools    []Tool    `json:"tools,omitempty"`
}

// Message is one item of the conversation history.
type Message struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	Name             string     `json:"name,omitempty"`
}

// Tool describes a function the model may call.
type Tool struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

// FunctionDef is the schema of a callable function.
type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolCall is the model's request to invoke a function.
type ToolCall struct {
	Index    int         `json:"index,omitempty"`
	ID       string      `json:"id,omitempty"`
	Type     string      `json:"type,omitempty"`
	Function FunctionArg `json:"function"`
}

// FunctionArg carries the function name and JSON-encoded arguments.
type FunctionArg struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// ChatResponse is the non-streaming response body.
type ChatResponse struct {
	ID      string    `json:"id"`
	Object  string    `json:"object"`
	Created int64     `json:"created"`
	Model   string    `json:"model"`
	Choices []Choice  `json:"choices"`
	Usage   *Usage    `json:"usage,omitempty"`
	Error   *APIError `json:"error,omitempty"`
}

// Choice is one completion variant.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason *string `json:"finish_reason"`
}

// Usage reports token consumption.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// StreamChunk is one "data: {...}" event of the SSE stream.
type StreamChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []StreamDelta `json:"choices"`
	Error   *APIError     `json:"error,omitempty"`
}

// StreamDelta is the incremental delta inside one stream chunk.
type StreamDelta struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// Delta carries incremental fields of a streaming message.
type Delta struct {
	Role             string     `json:"role,omitempty"`
	Content          string     `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

// APIError is the standard error envelope returned by the server.
type APIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

func (e *APIError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code != "" {
		return e.Type + " (" + e.Code + "): " + e.Message
	}
	return e.Type + ": " + e.Message
}

// Agent is one entry of GET /v1/sessions.
type Agent struct {
	Agent           string  `json:"agent"`
	SessionID       *string `json:"session_id"`
	MessageCount    int     `json:"message_count"`
	Account         string  `json:"account"`
	HistorySize     int     `json:"history_size"`
	TrackedMessages int     `json:"tracked_messages"`
	RemoteChars     int     `json:"remote_chars"`
	Busy            bool    `json:"busy"`
	AgeMin          int     `json:"age_min"`
}

// SessionsResponse is the body of GET /v1/sessions.
type SessionsResponse struct {
	Agents []Agent `json:"agents"`
	Total  int     `json:"total"`
}

// ResetSessionResponse is the body of POST /reset-session.
type ResetSessionResponse struct {
	Status           string `json:"status"`
	Agent            string `json:"agent"`
	HistoryPreserved int    `json:"history_preserved"`
	History          string `json:"history,omitempty"`
}
