// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
)

// pointerPlan declares one criterion whose step expects a status, a JSON
// pointer's value, a header and body text.
var pointerPlan = strings.Replace(validPlan, "estimated_token_budget: 200000\n",
	"estimated_token_budget: 200000\n"+
		"acceptance:\n"+
		"  - id: ratings-order\n"+
		"    kind: http\n"+
		"    statement: the top rated pattern comes first\n"+
		"    target: app\n"+
		"    request: {method: GET, path: /api/ratings}\n"+
		"    expect:\n"+
		"      status: 200\n"+
		"      json: {\"/0/id\": \"p_mine\", \"/0/rating\": 1031}\n"+
		"      headers: {Content-Type: application/json}\n"+
		"      bodyContains: p_mine\n", 1)

// TestVerifyCommentNamesWhatFailed (beatbattle-44, rough edge 7): a step
// that answered with the right status but the wrong JSON names each part
// that failed, with what was expected and what was observed, the observed
// value made inert; the parts that matched are not listed.
func TestVerifyCommentNamesWhatFailed(t *testing.T) {
	e := newVerifyEnv(t, pointerPlan)
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 3)
	e.observe(e.onlyVerification(name).Name, v1alpha1.VerificationObserved,
		func(acceptance.Call) acceptance.Response {
			return acceptance.Response{Status: 200, Header: map[string][]string{"Content-Type": {"text/html"}},
				Body: []byte(`[{"id":"p_4e1013 @octocat","rating":1031}]`)}
		})
	e.review(name, 8)
	cs := e.verifyComments(name, 1)
	if len(cs) != 1 {
		t.Fatalf("verification comments = %d, want one", len(cs))
	}
	got := cs[0].Body
	for _, want := range []string{
		"JSON `/0/id`: expected `\"p_mine\"`, observed `\"p_4e1013 @octocat\"`",
		"header `Content-Type`: expected `\"application/json\"`, observed `text/html`",
		"body: expected it to contain `p_mine`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("comment lacks %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"`/0/rating`", "status: expected"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("comment lists %q, which matched:\n%s", unwanted, got)
		}
	}
}
