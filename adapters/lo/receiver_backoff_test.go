//nolint:testpackage // whitebox: verify the private delay policy without sleeping for 30 seconds
package lo

import (
	"errors"
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
