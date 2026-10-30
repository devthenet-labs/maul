// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/google/go-github/v90/github"
)

// TestIsForbiddenExcludesThrottling: GitHub throttles with a 403 that
// go-github types as a rate-limit error only for some shapes; any other 403
// carrying a throttling mark is no permission refusal.
func TestIsForbiddenExcludesThrottling(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		headers map[string]string
		message string
		want    bool
	}{
		{"plain 403", 403, nil, "Resource not accessible by integration", true},
		{"403 remaining non-zero", 403, map[string]string{"X-RateLimit-Remaining": "4999"}, "Forbidden", true},
		{"403 retry-after", 403, map[string]string{"Retry-After": "60"}, "Forbidden", false},
		{"403 remaining zero", 403, map[string]string{"X-RateLimit-Remaining": "0"}, "Forbidden", false},
		{"403 rate limit message", 403, nil, "API Rate Limit exceeded for installation", false},
		{"403 secondary message", 403, nil, "You have exceeded a secondary rate limit", false},
		{"404", 404, nil, "Not Found", false},
		{"500", 500, nil, "Server Error", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := &http.Response{StatusCode: tc.status, Header: http.Header{}}
			for k, v := range tc.headers {
				res.Header.Set(k, v)
			}
			err := fmt.Errorf("wrapped: %w", &github.ErrorResponse{Response: res, Message: tc.message})
			if got := IsForbidden(err); got != tc.want {
				t.Errorf("IsForbidden() = %v, want %v", got, tc.want)
			}
		})
	}
	if IsForbidden(nil) || IsForbidden(&github.ErrorResponse{}) {
		t.Error("IsForbidden of nil or a response-less error must be false")
	}
}
