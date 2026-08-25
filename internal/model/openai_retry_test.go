package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func okCompletion(content string) []byte {
	raw, _ := json.Marshal(oaiResp{
		Model: "test",
		Choices: []struct {
			FinishReason string `json:"finish_reason"`
			Message      oaiMsg `json:"message"`
		}{
			{FinishReason: "stop", Message: oaiMsg{Role: "assistant", Content: content}},
		},
		Usage: Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	})
	return raw
}

func testClient(url string) *OpenAI {
	return &OpenAI{
		BaseURL:    url,
		APIKey:     "sk-test",
		Model:      "test",
		HTTP:       &http.Client{Timeout: 5 * time.Second},
		MaxRetries: 3,
		Backoff:    time.Millisecond,
	}
}

func TestCompleteRetries429ThenSucceeds(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		if !json.Valid(body) {
			t.Errorf("invalid request body")
		}
		if n <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(okCompletion("hello"))
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	resp, err := c.Complete(context.Background(), CompleteRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "hello" {
		t.Fatalf("content=%q", resp.Message.Content)
	}
	if hits.Load() != 3 {
		t.Fatalf("hits=%d want 3", hits.Load())
	}
}

func TestCompleteDoesNotRetry400(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad","type":"invalid_request"}}`))
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	_, err := c.Complete(context.Background(), CompleteRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if IsRetryable(err) {
		t.Fatalf("400 must not be retryable: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits=%d want 1 (no retry)", hits.Load())
	}
}

func TestComplete503ExhaustsRetries(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"down","type":"server_error"}}`))
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	_, err := c.Complete(context.Background(), CompleteRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsRetryable(err) {
		t.Fatalf("exhausted 503 should remain retryable: %v", err)
	}
	if HTTPStatus(err) != 503 {
		t.Fatalf("status=%d", HTTPStatus(err))
	}
	if hits.Load() != 4 { // 1 initial + 3 retries
		t.Fatalf("hits=%d want 4", hits.Load())
	}
}

func TestCompleteCanceledContextDoesNotRetry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := testClient(srv.URL)
	_, err := c.Complete(ctx, CompleteRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if IsRetryable(err) {
		t.Fatalf("canceled must not be retryable: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("hits=%d want 0 (no request on canceled ctx)", hits.Load())
	}
}

func TestIsRetryable(t *testing.T) {
	if IsRetryable(nil) {
		t.Fatal("nil")
	}
	if IsRetryable(context.Canceled) || IsRetryable(context.DeadlineExceeded) {
		t.Fatal("parent ctx")
	}
	if IsRetryable(&RetryableError{Err: context.Canceled}) {
		t.Fatal("wrapped cancel")
	}
	if !IsRetryable(&RetryableError{Status: 429, Err: errors.New("slow")}) {
		t.Fatal("429")
	}
	if IsRetryable(errors.New("openai: HTTP 400: bad")) {
		t.Fatal("plain 400")
	}
}

func TestRetryAfterHonored(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write(okCompletion("ok"))
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	resp, err := c.Complete(context.Background(), CompleteRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Message.Content, "ok") {
		t.Fatalf("content=%q", resp.Message.Content)
	}
}
