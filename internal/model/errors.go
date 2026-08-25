package model

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// RetryableError marks a model/HTTP failure that is safe to retry
// (rate limit, 5xx, transport, truncated body). Callers should not
// retry context.Canceled or DeadlineExceeded from the parent ctx.
type RetryableError struct {
	Err    error
	Status int
	After  time.Duration
}

func (e *RetryableError) Error() string {
	if e == nil {
		return "retryable error"
	}
	if e.Err != nil {
		if e.Status > 0 {
			return fmt.Sprintf("retryable (HTTP %d): %v", e.Status, e.Err)
		}
		return fmt.Sprintf("retryable: %v", e.Err)
	}
	if e.Status > 0 {
		return fmt.Sprintf("retryable HTTP %d", e.Status)
	}
	return "retryable error"
}

func (e *RetryableError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// IsRetryable reports whether err is a RetryableError. Parent-context
// cancel and deadline are never retryable, even if wrapped.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var re *RetryableError
	return errors.As(err, &re)
}

// HTTPStatus returns the HTTP status carried by err, or 0.
func HTTPStatus(err error) int {
	var re *RetryableError
	if errors.As(err, &re) {
		return re.Status
	}
	return 0
}
