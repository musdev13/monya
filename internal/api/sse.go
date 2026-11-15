package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Stream is an open SSE stream of chat completion chunks.
type Stream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
}

// Next returns the next chunk from the stream.
// It returns io.EOF after the server sends "data: [DONE]" or closes the connection.
func (s *Stream) Next() (*StreamChunk, error) {
	for s.scanner.Scan() {
		line := strings.TrimRight(s.scanner.Text(), "\r")
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			return nil, io.EOF
		}
		var chunk StreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return nil, fmt.Errorf("api: decode stream chunk: %w", err)
		}
		if chunk.Error != nil {
			return &chunk, chunk.Error
		}
		return &chunk, nil
	}
	if err := s.scanner.Err(); err != nil {
		return nil, fmt.Errorf("api: stream read: %w", err)
	}
	return nil, io.EOF
}

// Close releases the underlying response body.
func (s *Stream) Close() error {
	if s.body == nil {
		return nil
	}
	return s.body.Close()
}

// ChatCompletionStream starts a streaming chat completion.
// The caller must Close the returned stream when done.
func (c *Client) ChatCompletionStream(ctx context.Context, req ChatRequest) (*Stream, error) {
	req.Stream = true
	if req.User == "" {
		req.User = c.agent
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("api: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("api: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("api: request failed: %w", err)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		var env struct {
			Error *APIError `json:"error"`
		}
		if json.Unmarshal(body, &env) == nil && env.Error != nil && env.Error.Message != "" {
			return nil, fmt.Errorf("api: http %d: %w", resp.StatusCode, env.Error)
		}
		return nil, fmt.Errorf("api: http %d: %s", resp.StatusCode, snippet(body))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)

	return &Stream{body: resp.Body, scanner: scanner}, nil
}

// ErrStreamClosed is returned by helpers when the caller closed the stream.
var ErrStreamClosed = errors.New("api: stream closed")
