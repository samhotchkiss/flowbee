package github

import (
	"net/http"
	"testing"
)

// TestErrGitHubPermanentClassification: 4xx client errors are permanent (retry is
// futile) EXCEPT 408/429 which are retryable; 5xx are transient.
func TestErrGitHubPermanentClassification(t *testing.T) {
	perm := []int{http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusGone, http.StatusConflict}
	for _, code := range perm {
		if !(&ErrGitHub{StatusCode: code}).Permanent() {
			t.Errorf("status %d should be permanent", code)
		}
	}
	transient := []int{http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout}
	for _, code := range transient {
		if (&ErrGitHub{StatusCode: code}).Permanent() {
			t.Errorf("status %d should NOT be permanent (retryable)", code)
		}
	}
}
