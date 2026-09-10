//nolint:testpackage // whitebox: verify the private delay policy without sleeping for 30 seconds
package lo

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestPollingRetryDelayIsBounded(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want time.Duration
	}{
		{"conflict", &APIError{Code: http.StatusConflict}, 10 * time.Second},
		{"wrapped conflict", fmt.Errorf("poll: %w", &APIError{Code: http.StatusConflict}), 10 * time.Second},
		{"network", errors.New("connection refused"), time.Second},
		{"missing hint", &APIError{Code: http.StatusTooManyRequests}, time.Second},
		{"short hint", &APIError{Code: http.StatusTooManyRequests, Delay: 3 * time.Second}, 3 * time.Second},
		{"long hint", &APIError{Code: http.StatusTooManyRequests, Delay: 24 * time.Hour}, 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pollingRetryDelay(tc.err); got != tc.want {
				t.Fatalf("delay=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestTransientPollingErrorsRemainRetryable(t *testing.T) {
	t.Parallel()
	for _, code := range []int{
		http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
	} {
		if fatalAPIError(&APIError{Code: code}) {
			t.Fatalf("status %d classified as permanent", code)
		}
	}
}

func TestConflictWindowCoversLongPoll(t *testing.T) {
	t.Parallel()
	delay := pollingRetryDelay(&APIError{Code: http.StatusConflict})
	if budget := time.Duration(maxPollingConflicts-1) * delay; budget <= time.Duration(pollTimeoutSeconds)*time.Second {
		t.Fatalf("conflict budget %v does not cover long poll", budget)
	}
}
