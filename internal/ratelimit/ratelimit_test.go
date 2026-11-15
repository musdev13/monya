package ratelimit

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTransport_Success_NoRetry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	}))
	defer srv.Close()

	tr := NewTransport(srv.Client().Transport, 100, 1, 1)
	client := &http.Client{Transport: tr}

	req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if hits.Load() != 1 {
		t.Errorf("hits = %d, want 1", hits.Load())
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestTransport_Retry_On429(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, "rate limited")
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	}))
	defer srv.Close()

	tr := NewTransport(srv.Client().Transport, 100, 1, 1)
	tr.BaseBackoff = 10 * time.Millisecond // speed up test
	client := &http.Client{Transport: tr}

	start := time.Now()
	req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if hits.Load() != 3 {
		t.Errorf("hits = %d, want 3", hits.Load())
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	// Expected backoff: attempt0 fail -> sleep 10ms, attempt1 fail -> sleep 20ms, attempt2 success
	if elapsed < 30*time.Millisecond {
		t.Errorf("elapsed = %v, want at least 30ms", elapsed)
	}
}

func TestTransport_Retry_On500(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, "boom")
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := NewTransport(srv.Client().Transport, 100, 1, 1)
	tr.BaseBackoff = 10 * time.Millisecond
	client := &http.Client{Transport: tr}

	req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2", hits.Load())
	}
}

func TestTransport_Retry_Exhausted(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "always fails")
	}))
	defer srv.Close()

	tr := NewTransport(srv.Client().Transport, 100, 1, 1)
	tr.BaseBackoff = 5 * time.Millisecond
	tr.MaxRetries = 2
	client := &http.Client{Transport: tr}

	req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if hits.Load() != 3 {
		t.Errorf("hits = %d, want 3 (1 original + 2 retries)", hits.Load())
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	// The body of the LAST failed attempt must be readable (we fixed the close bug)
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "always fails" {
		t.Errorf("body = %q, want 'always fails'", string(body))
	}
}

func TestTransport_ConcurrencyLimit(t *testing.T) {
	var current atomic.Int32
	var maxCurrent atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := current.Add(1)
		for {
			m := maxCurrent.Load()
			if c <= m || maxCurrent.CompareAndSwap(m, c) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		current.Add(-1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := NewTransport(srv.Client().Transport, 100, 1, 2) // max 2 concurrent
	client := &http.Client{Transport: tr}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()

	if m := maxCurrent.Load(); m > 2 {
		t.Errorf("max concurrent = %d, want <= 2", m)
	}
	if m := maxCurrent.Load(); m < 2 {
		t.Errorf("max concurrent = %d, want exactly 2 (semaphore should allow 2)", m)
	}
}

func TestTransport_ContextCancel(t *testing.T) {
	tr := NewTransport(nil, 100, 1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:9999", nil)
	_, err := tr.RoundTrip(req)
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}
