// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/forge"
	"github.com/devthenet-labs/maul/internal/ghsecret"
	"github.com/devthenet-labs/maul/internal/kube"
)

// idleReads are the GitHub reads an InReview intent's pass makes while
// nothing changes (D-B7), as the port fake counts them, each with the call
// TestIdleReadsAreConditional makes for it. The fake counts a pull request
// comment listing as an issue comment listing too (its GitHub shares the
// endpoint), so ListIssueComments is two per pass: the issue's and the
// pull request's.
var idleReads = map[string]struct {
	perPass int
	call    func(ctx context.Context, g GitHub, intents, app, head string) error
}{
	"GetIssue": {1, func(ctx context.Context, g GitHub, intents, _, _ string) error {
		_, err := g.GetIssue(ctx, intents, 1)
		return err
	}},
	"ListIssueEvents": {1, func(ctx context.Context, g GitHub, intents, _, _ string) error {
		_, err := g.ListIssueEvents(ctx, intents, 1)
		return err
	}},
	"ListIssueComments": {2, func(ctx context.Context, g GitHub, intents, _, _ string) error {
		_, err := g.ListIssueComments(ctx, intents, 1, time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC))
		return err
	}},
	"GetPullRequest": {1, func(ctx context.Context, g GitHub, _, app, _ string) error {
		_, err := g.GetPullRequest(ctx, app, 2)
		return err
	}},
	"ListPullRequestComments": {1, func(ctx context.Context, g GitHub, _, app, _ string) error {
		_, err := g.ListPullRequestComments(ctx, app, 2, time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC))
		return err
	}},
	"ListPullRequestReviews": {1, func(ctx context.Context, g GitHub, _, app, _ string) error {
		_, err := g.ListPullRequestReviews(ctx, app, 2)
		return err
	}},
	"ListCommitStatuses": {1, func(ctx context.Context, g GitHub, _, app, head string) error {
		_, err := g.ListCommitStatuses(ctx, app, head)
		return err
	}},
}

// idleAdmin are the port calls an idle pass makes that cost GitHub
// nothing: RateRemaining (GET /rate_limit is free, and kept a minute) and
// BotLogin (kept per Forge version, never an installation request).
var idleAdmin = []string{"BotLogin", "RateRemaining"}

// TestIdleReviewPassReads pins what an InReview intent with an open pull
// request reads from GitHub on each pass while nothing changes: the reads
// TestIdleReadsAreConditional proves cost the installation's rate budget
// once, not once per pass, each asked again at the same URL (the comment
// listings' since holds still). A read added to the idle pass fails here
// until it is listed there, and proven conditional.
func TestIdleReviewPassReads(t *testing.T) {
	const passes = 30
	e, name := passedEvaluation(t)
	e.review(name, 3)
	e.gh.mu.Lock()
	e.gh.calls = map[string]int{}
	e.gh.mu.Unlock()
	// A conditional GET only saves a request asked again at the same URL:
	// the comment listings' since must hold still while nothing changes.
	var first []time.Time
	for i := range passes {
		e.gh.mu.Lock()
		e.gh.sinces = nil
		e.gh.mu.Unlock()
		e.review(name, 1)
		e.gh.mu.Lock()
		sinces := slices.Clone(e.gh.sinces)
		e.gh.mu.Unlock()
		if i == 0 {
			first = sinces
		} else if !slices.Equal(sinces, first) {
			t.Fatalf("idle pass %d listed comments since %v, the first since %v", i, sinces, first)
		}
	}
	if ph := e.get(name).Status.Phase; ph != v1alpha1.IntentInReview {
		t.Fatalf("phase = %s, want InReview throughout", ph)
	}
	want := map[string]int{}
	for m, r := range idleReads {
		want[m] = r.perPass * passes
	}
	e.gh.mu.Lock()
	got := maps.Clone(e.gh.calls)
	e.gh.mu.Unlock()
	for _, m := range idleAdmin {
		delete(got, m)
	}
	if !maps.Equal(got, want) {
		t.Fatalf("GitHub reads over %d idle passes:\n got  %v\n want %v", passes, got, want)
	}
}

// passedEvaluation is an intent in review whose first evaluation round
// passed: both saved checks replayed green, the manual outcome accepted,
// the hand-off released. It returns the env and the intent.
func passedEvaluation(t *testing.T) (*env, string) {
	t.Helper()
	e, name := newEvaluatorEnv(t, 0.4)
	version, health := savedCheck(t, versionCheck), savedCheck(t, healthCheck)
	e.storeChecks(name, version, health)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Acceptance.Accepted = append(in.Status.Acceptance.Accepted, v1alpha1.AcceptanceRecord{ID: oManual,
			By: approver, ByID: 7, At: metav1.NewTime(e.clock.Now()), CommentID: 99,
			HeadSetDigest: in.Status.Verification.HeadSetDigest})
	})
	e.evalRound(name, replays(version, health, v1alpha1.CheckRunPass, v1alpha1.CheckRunPass)...)
	if v := e.verification(name); v.State != v1alpha1.VerificationPassed || pv.Name != v.Ref.Name {
		t.Fatalf("verification = %s %+v, want the round Passed", v.State, v.Outcomes)
	}
	return e, name
}

// conditionalGitHub is a GitHub API answering every GET the way GitHub
// does: a weak ETag over the body, and a bodiless 304, which costs no rate
// budget, when If-None-Match names it. billed counts every other answer.
type conditionalGitHub struct {
	mu     sync.Mutex
	billed map[string]int
}

func (s *conditionalGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")
	body := `[{"id":7,"node_id":"N7","body":"hello","user":{"login":"bob","id":9,"type":"User"},` +
		`"state":"COMMENTED","context":"ci","created_at":"2026-09-24T09:00:00Z","updated_at":"2026-09-24T09:00:00Z"}]`
	switch {
	case strings.HasSuffix(path, "/issues/1"):
		body = `{"number":1,"state":"open","title":"t","body":"b","user":{"login":"amy"}}`
	case strings.HasSuffix(path, "/pulls/2"):
		body = `{"number":2,"state":"open","head":{"sha":"` + strings.Repeat("a", 40) + `"}}`
	}
	sum := sha256.Sum256([]byte(body))
	tag := `W/"` + hex.EncodeToString(sum[:16]) + `"`
	w.Header().Set("ETag", tag)
	if r.Method == http.MethodGet && r.Header.Get("If-None-Match") == tag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	s.mu.Lock()
	s.billed[r.Method+" "+path]++
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// TestIdleReadsAreConditional (D-B7): the production GitHub asks each idle
// read conditionally, so an unchanged answer costs the installation's rate
// budget once, not once per pass. Ten intents polled every minute with
// every read billed spent the whole hourly budget in about fifty minutes.
// Each answer is still asked of GitHub on every pass: a change is seen at
// once.
func TestIdleReadsAreConditional(t *testing.T) {
	const passes = 20
	ctx := context.Background()
	gh := &conditionalGitHub{billed: map[string]int{}}
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	forgeObj := &v1alpha1.Forge{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "github"},
		Spec: v1alpha1.ForgeSpec{Provider: v1alpha1.ForgeProviderGitHub, BaseURL: srv.URL,
			SecretRef: v1alpha1.LocalSecretReference{Name: "cred"}},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "cred"},
		Data:       map[string][]byte{ghsecret.KeyToken: []byte("ghp_dev")},
	}
	store := forge.NewStore(fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(forgeObj, secret).Build())
	g := NewForgeGitHub(store, testNS)
	intents, app, head := srv.URL+"/acme/intents", srv.URL+"/acme/app", strings.Repeat("a", 40)

	names := slices.Sorted(maps.Keys(idleReads))
	for range passes {
		for _, m := range names {
			if err := idleReads[m].call(ctx, g, intents, app, head); err != nil {
				t.Fatalf("%s: %v", m, err)
			}
		}
	}
	gh.mu.Lock()
	defer gh.mu.Unlock()
	total := 0
	for req, n := range gh.billed {
		if n != 1 {
			t.Errorf("%s billed %d times over %d idle passes, want once", req, n, passes)
		}
		total += n
	}
	// ListIssueComments and ListPullRequestComments are one endpoint each
	// here, in two repositories: seven requests in all.
	if total != len(names) {
		t.Errorf("%d billed requests over %d idle passes, want %d: %v", total, passes, len(names), gh.billed)
	}
}
