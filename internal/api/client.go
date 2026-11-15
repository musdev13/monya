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
	// DefaultBaseURL is the DS-API address used when no base URL is given.
	DefaultBaseURL = "http://127.0.0.1:9655"

	// DefaultAgent is the fallback agent name for session-scoped requests.
	DefaultAgent = "dev-agent"

	// maxBodyBytes caps how much of a response body we will read.
	maxBodyBytes = 8 << 20
)

// Client is a thin DS-API client. It does no rate limiting or retries;
// those are layered on top by wrapping httpClient.Transport.
type Client struct {
	baseURL    string
	agent      string
	httpClient *http.Client
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
		baseURL:    strings.TrimRight(baseURL, "/"),
		agent:      agent,
		httpClient: &http.Client{},
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
	req = req.WithContext(ctx)
	resp, err := c.httpClient.Do(req)
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
