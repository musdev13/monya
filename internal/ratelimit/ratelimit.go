package ratelimit

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/time/rate"
)

// Transport is an http.RoundTripper that applies rate limiting,
// concurrency limits, and exponential backoff on 429/5xx responses.
type Transport struct {
	Base        http.RoundTripper
	Limiter     *rate.Limiter
	Sem         chan struct{}
	MaxRetries  int
	BaseBackoff time.Duration
}

// NewTransport creates a new rate-limiting RoundTripper.
// rps is requests per second, burst is max instant burst (usually 1),
// maxConcurrent caps parallel requests.
func NewTransport(base http.RoundTripper, rps float64, burst, maxConcurrent int) *Transport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &Transport{
		Base:        base,
		Limiter:     rate.NewLimiter(rate.Limit(rps), burst),
		Sem:         make(chan struct{}, maxConcurrent),
		MaxRetries:  3,
		BaseBackoff: 1 * time.Second,
	}
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	// 1. Concurrency limit
	select {
	case t.Sem <- struct{}{}:
		defer func() { <-t.Sem }()
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}

	// 2. Rate limit (blocks until allowed)
	if err := t.Limiter.Wait(req.Context()); err != nil {
		return nil, fmt.Errorf("rate limit wait: %w", err)
	}

	// 3. Execute with retries
	var resp *http.Response
	var err error

	for attempt := 0; attempt <= t.MaxRetries; attempt++ {
		attemptReq := req

		// Clone request for retries to reset the body
		if attempt > 0 {
			if req.GetBody != nil {
				body, getErr := req.GetBody()
				if getErr != nil {
					return resp, fmt.Errorf("retry get body: %w", getErr)
				}
				attemptReq = req.Clone(req.Context())
				attemptReq.Body = body
			} else if req.Body != nil && req.Body != http.NoBody {
				return resp, fmt.Errorf("retry impossible: request has body but no GetBody")
			} else {
				attemptReq = req.Clone(req.Context())
			}
		}

		resp, err = t.Base.RoundTrip(attemptReq)

		shouldRetry := false
		if err != nil {
			shouldRetry = true // network error
		} else if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			shouldRetry = true
		}

		if !shouldRetry {
			return resp, nil
		}

		if attempt < t.MaxRetries {
			if resp != nil {
				resp.Body.Close() // closing because we will retry
			}
			backoff := t.BaseBackoff * time.Duration(math.Pow(2, float64(attempt)))
			if resp != nil && resp.Header.Get("Retry-After") != "" {
				if sec, parseErr := strconv.Atoi(resp.Header.Get("Retry-After")); parseErr == nil {
					backoff = time.Duration(sec) * time.Second
				}
			}

			select {
			case <-time.After(backoff):
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
	}

	return resp, err
}
