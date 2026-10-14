// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestTokenShape(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pod := map[string]any{"pod": map[string]any{"name": "agent-pod-1"}}
	tests := []struct {
		name  string
		token string
		want  string // "" for accepted, else a substring of the error
	}{
		{"pod-bound array aud", tokWith(map[string]any{
			"aud": []string{"x", DefaultAudience}, "exp": now.Unix() + 60, "kubernetes.io": pod}), ""},
		{"pod-bound string aud", tokWith(map[string]any{
			"aud": DefaultAudience, "exp": now.Unix() + 60, "kubernetes.io": pod}), ""},
		{"empty", "", "missing"},
		{"two segments", "a.b", "not a JWT"},
		{"empty segment", "a..c", "not a JWT"},
		{"not base64url", "a.!!!.c", "base64url"},
		{"payload not json", "a.bm90IGpzb24.c", "not JSON"},
		{"wrong audience", tokWith(map[string]any{
			"aud": "other", "exp": now.Unix() + 60, "kubernetes.io": pod}), "audience"},
		{"no audience", tokWith(map[string]any{"exp": now.Unix() + 60, "kubernetes.io": pod}), "audience"},
		{"expired", tokWith(map[string]any{
			"aud": DefaultAudience, "exp": now.Unix(), "kubernetes.io": pod}), "expired"},
		{"no exp", tokWith(map[string]any{"aud": DefaultAudience, "kubernetes.io": pod}), "expired"},
		{"not pod-bound", tokWith(map[string]any{"aud": DefaultAudience, "exp": now.Unix() + 60}), "pod-bound"},
		{"pod without name", tokWith(map[string]any{
			"aud": DefaultAudience, "exp": now.Unix() + 60, "kubernetes.io": map[string]any{"pod": map[string]any{}}}),
			"pod-bound"},
		{"too large", strings.Repeat("a", maxTokenBytes) + ".b.c", "too large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tokenShape(tt.token, DefaultAudience, now)
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("rejected: %v", err)
			case tt.want != "" && err == nil:
				t.Fatalf("accepted, want an error naming %q", tt.want)
			case tt.want != "" && !strings.Contains(err.Error(), tt.want):
				t.Fatalf("error = %q, want it to name %q", err, tt.want)
			}
		})
	}
}

func TestReviewLimiter(t *testing.T) {
	var disabled *reviewLimiter
	if err := disabled.admit(t.Context()); err != nil {
		t.Fatal(err)
	}
	l := newReviewLimiter(1)
	if err := l.admit(t.Context()); err != nil {
		t.Fatalf("first review refused: %v", err)
	}
	// The next slot is a second away; a caller that cannot wait is refused
	// rather than queued indefinitely.
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := l.admit(ctx); err == nil {
		t.Fatal("second review admitted without waiting for a slot")
	}
}
