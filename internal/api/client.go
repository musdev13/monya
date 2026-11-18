package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	// DefaultBaseURL is the llama.cpp server address used when no base URL is given.
	DefaultBaseURL = "http://127.0.0.1:8080"

	// DefaultAgent is the fallback agent name for session-scoped requests.
	DefaultAgent = "dev-agent"

	// maxBodyBytes caps how much of a response body we will read.
	maxBodyBytes = 8 << 20
)

// Client is a thin OpenAI-compatible API client. It does no rate limiting or retries;
// those are layered on top by wrapping httpClient.Transport.
type Client struct {
	baseURL           string
	agent             string
	httpClient        *http.Client
	contextHTTPClient *http.Client
}

// NewClient builds a Client. Empty baseURL/agent fall back to defaults.
func NewClient(baseURL, agent string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if agent == "" {
		agent = DefaultAgent
	}
	return &Client{
		baseURL:           strings.TrimRight(baseURL, "/"),
		agent:             agent,
		httpClient:        &http.Client{},
		contextHTTPClient: &http.Client{},
	}
}

// WithHTTPClient returns a copy of the client using the given HTTP client.
func (c *Client) WithHTTPClient(h *http.Client) *Client {
	cp := *c
	cp.httpClient = h
	return &cp
}

// WithAgent returns a copy of the client bound to another default agent.
func (c *Client) WithAgent(agent string) *Client {
	cp := *c
	if agent != "" {
		cp.agent = agent
	}
	return &cp
}

// BaseURL reports the configured API root.
func (c *Client) BaseURL() string { return c.baseURL }

// Agent reports the default agent name.
func (c *Client) Agent() string { return c.agent }

// endpoint builds an absolute URL and appends ?agent= when agent is set.
func (c *Client) endpoint(path, agent string) string {
	if agent == "" {
		agent = c.agent
	}
	if agent == "" {
		return c.baseURL + path
	}
	q := url.Values{}
	q.Set("agent", agent)
	return c.baseURL + path + "?" + q.Encode()
}

// doJSON sends req and decodes a JSON response into out.
func (c *Client) doJSON(ctx context.Context, req *http.Request, out any) error {
	return c.doJSONWithClient(ctx, c.httpClient, req, out)
}

func (c *Client) doContextJSON(ctx context.Context, req *http.Request, out any) error {
	return c.doJSONWithClient(ctx, c.contextHTTPClient, req, out)
}

func (c *Client) doJSONWithClient(ctx context.Context, client *http.Client, req *http.Request, out any) error {
	req = req.WithContext(ctx)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("api: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("api: read body: %w", err)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		var env struct {
			Error *APIError `json:"error"`
		}
		if json.Unmarshal(body, &env) == nil && env.Error != nil && env.Error.Message != "" {
			return fmt.Errorf("api: http %d: %w", resp.StatusCode, env.Error)
		}
		return fmt.Errorf("api: http %d: %s", resp.StatusCode, snippet(body))
	}

	if out == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("api: decode body: %w", err)
	}
	return nil
}

// ContextSize returns the llama.cpp server's configured context length.
func (c *Client) ContextSize(ctx context.Context) (int, error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/props", nil)
	if err != nil {
		return 0, fmt.Errorf("api: build context request: %w", err)
	}

	var props struct {
		DefaultGenerationSettings struct {
			ContextSize int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if err := c.doContextJSON(ctx, req, &props); err != nil {
		return 0, err
	}
	if props.DefaultGenerationSettings.ContextSize <= 0 {
		return 0, fmt.Errorf("api: llama.cpp returned an invalid context size")
	}
	return props.DefaultGenerationSettings.ContextSize, nil
}

// CountChatTokens counts the tokens in the fully rendered chat template,
// including special tokens and tools.
func (c *Client) CountChatTokens(ctx context.Context, chat ChatRequest) (int, error) {
	templateBody, err := json.Marshal(struct {
		Messages            []Message      `json:"messages"`
		Tools               []Tool         `json:"tools,omitempty"`
		ChatTemplateKwargs  map[string]any `json:"chat_template_kwargs,omitempty"`
		AddGenerationPrompt bool           `json:"add_generation_prompt"`
	}{
		Messages:            chat.Messages,
		Tools:               chat.Tools,
		ChatTemplateKwargs:  chat.ChatTemplateKwargs,
		AddGenerationPrompt: true,
	})
	if err != nil {
		return 0, fmt.Errorf("api: encode template request: %w", err)
	}

	templateReq, err := http.NewRequest(http.MethodPost, c.baseURL+"/apply-template", bytes.NewReader(templateBody))
	if err != nil {
		return 0, fmt.Errorf("api: build template request: %w", err)
	}
	templateReq.Header.Set("Content-Type", "application/json")
	var rendered struct {
		Prompt string `json:"prompt"`
	}
	if err := c.doContextJSON(ctx, templateReq, &rendered); err != nil {
		return 0, fmt.Errorf("api: render chat template: %w", err)
	}

	tokenizeBody, err := json.Marshal(struct {
		Content      string `json:"content"`
		AddSpecial   bool   `json:"add_special"`
		ParseSpecial bool   `json:"parse_special"`
	}{
		Content:      rendered.Prompt,
		AddSpecial:   false,
		ParseSpecial: true,
	})
	if err != nil {
		return 0, fmt.Errorf("api: encode tokenize request: %w", err)
	}

	tokenizeReq, err := http.NewRequest(http.MethodPost, c.baseURL+"/tokenize", bytes.NewReader(tokenizeBody))
	if err != nil {
		return 0, fmt.Errorf("api: build tokenize request: %w", err)
	}
	tokenizeReq.Header.Set("Content-Type", "application/json")
	var tokenized struct {
		Tokens []int `json:"tokens"`
	}
	if err := c.doContextJSON(ctx, tokenizeReq, &tokenized); err != nil {
		return 0, fmt.Errorf("api: tokenize chat prompt: %w", err)
	}
	return len(tokenized.Tokens), nil
}

// snippet shortens a body for error messages.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

// ChatCompletion performs a non-streaming chat completion.
func (c *Client) ChatCompletion(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	req.Stream = false
	if req.User == "" {
		req.User = c.agent
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("api: encode request: %w", err)
	}

	httpReq, err := http.NewRequest(http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("api: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	var out ChatResponse
	if err := c.doJSON(ctx, httpReq, &out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return &out, out.Error
	}
	if len(out.Choices) == 0 {
		return &out, fmt.Errorf("api: response has no choices")
	}
	return &out, nil
}

// ListSessions returns the active agent sessions.
func (c *Client) ListSessions(ctx context.Context) (*SessionsResponse, error) {
	httpReq, err := http.NewRequest(http.MethodGet, c.baseURL+"/v1/sessions", nil)
	if err != nil {
		return nil, fmt.Errorf("api: build request: %w", err)
	}
	var out SessionsResponse
	if err := c.doJSON(ctx, httpReq, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ResetSession resets one agent session, or all of them when agent is "all".
func (c *Client) ResetSession(ctx context.Context, agent string) (*ResetSessionResponse, error) {
	if agent == "" {
		agent = c.agent
	}
	u := c.baseURL + "/reset-session?" + url.Values{"agent": {agent}}.Encode()
	httpReq, err := http.NewRequest(http.MethodPost, u, nil)
	if err != nil {
		return nil, fmt.Errorf("api: build request: %w", err)
	}
	var out ResetSessionResponse
	if err := c.doJSON(ctx, httpReq, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
