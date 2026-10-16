// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/stats"
	"github.com/bitwise-media-group/patchy/internal/usageview"
	"github.com/bitwise-media-group/patchy/internal/web/auth"
)

// Callers of the usage views: two GitHub sign-ins, one of them an admin,
// and a signed-in identity from another connector.
var (
	usageOctocat = &auth.Identity{Username: "github:octocat", DisplayName: "Octo", Session: true, GitHubID: 42}
	usageHubot   = &auth.Identity{Username: "github:hubot", DisplayName: "Hubot", Session: true, GitHubID: 7}
	usageAdmin   = &auth.Identity{Username: "github:admin", DisplayName: "Admin", Session: true, GitHubID: 1}
	// usageLDAPAdmin holds usage/all but signed in through another
	// connector: no GitHub id, so not even the admin view serves them.
	usageLDAPAdmin = &auth.Identity{Username: "github:admin", DisplayName: "Admin (LDAP)", Session: true}
)

// usageGranter grants usage/all to the github:admin username.
type usageGranter struct{ asked []string }

func (g *usageGranter) Allowed(_ context.Context, id auth.Identity, verb string) (bool, error) {
	g.asked = append(g.asked, id.Username+" "+verb)
	return id.Username == "github:admin" && verb == "get", nil
}

// usageFixture folds a few runs into rollups as the aggregator would.
func usageFixture(t *testing.T) []client.Object {
	t.Helper()
	type run struct {
		user, login, project, intent string
		tokens, cost                 int64
	}
	byName := map[string]*v1alpha1.UsageRollup{}
	for i, r := range []run{
		{"github-id:42", "octocat", "shop", "add-cart", 100, 2_000_000},
		{"github-id:42", "octocat", "", "", 50, 100},
		{"github-id:7", "hubot", "shop", "hubots-secret-intent", 900, 9_000_000},
		{"org", "", "", "", 5, 5},
	} {
		d := stats.UsageDelta{Kind: "build", CompletedAt: testClock.Add(-time.Hour), Intent: r.intent,
			Project: r.project, Source: v1alpha1.UsageSourceMeasured,
			Usage: v1alpha1.UsageAggregate{Runs: 1, InputTokens: r.tokens, CostMicroUSD: r.cost}}
		for _, scope := range stats.UsageScopes(r.user, r.project) {
			name := stats.UsageObjectName(scope)
			ur, ok := byName[name]
			if !ok {
				ur = &v1alpha1.UsageRollup{Spec: v1alpha1.UsageRollupSpec{Scope: scope}}
				ur.Name, ur.Namespace = name, "patchy"
				byName[name] = ur
			}
			login := r.login
			if scope.Type == v1alpha1.UsageScopeProject {
				login = ""
			}
			stats.ApplyUsage(&ur.Status, "u:"+string(rune('a'+i)), d, login,
				scope.Type == v1alpha1.UsageScopeUser, testClock)
		}
	}
	out := make([]client.Object, 0, len(byName))
	for _, ur := range byName {
		out = append(out, ur)
	}
	return out
}

func usageServer(t *testing.T) (*Server, *usageGranter) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(usageFixture(t)...).
		WithIndex(&v1alpha1.UsageRollup{}, UsageUserIndex, UsageUserIndexer).Build()
	g := &usageGranter{}
	s := NewServer(c, "patchy", stubAuth{}, stubGranter{grants: allGrants()}, nil).WithUsage(UsageOptions{All: g})
	s.now = func() time.Time { return testClock }
	return s, g
}

func TestUsageRoutesRequireASession(t *testing.T) {
	s, _ := usageServer(t)
	ts := as(t, s, nil)
	for _, path := range []string{"/api/usage/access", "/api/usage", "/api/usage/all"} {
		if code, _ := get(t, ts, path); code != http.StatusUnauthorized {
			t.Errorf("GET %s without a session = %d, want 401", path, code)
		}
	}
}

// An identity that did not come through the GitHub connector has no GitHub
// id: it is refused both views, even holding usage/all, and told why.
func TestUsageRefusesANonGitHubIdentity(t *testing.T) {
	s, g := usageServer(t)
	ts := as(t, s, usageLDAPAdmin)
	for _, path := range []string{"/api/usage", "/api/usage/all"} {
		code, body := get(t, ts, path)
		if code != http.StatusForbidden || !strings.Contains(body, "GitHub") {
			t.Errorf("GET %s as a non-GitHub identity = %d %q, want 403 naming GitHub", path, code, body)
		}
	}
	code, body := get(t, ts, "/api/usage/access")
	if code != http.StatusOK {
		t.Fatalf("GET /api/usage/access = %d", code)
	}
	if a := decode[UsageAccess](t, body); a.Self || a.All || !strings.Contains(a.Reason, "GitHub") {
		t.Errorf("access = %+v, want neither view and a reason", a)
	}
	if len(g.asked) != 0 {
		t.Errorf("a non-GitHub identity reached an access review: %v", g.asked)
	}
}

// A person sees exactly their own usage, keyed by their GitHub id, never
// another's intents; and cannot filter their way into someone else's.
func TestUsageSelfIsOwnOnly(t *testing.T) {
	s, _ := usageServer(t)
	code, body := get(t, as(t, s, usageOctocat), "/api/usage?days=90")
	if code != http.StatusOK {
		t.Fatalf("GET /api/usage = %d %s", code, body)
	}
	rep := decode[usageview.UsageReport](t, body)
	if rep.View != usageview.ViewSelf || rep.User != "github-id:42" || rep.Login != "octocat" || rep.Days != 90 ||
		rep.Totals.Tokens != 150 || rep.Totals.CostMicroUSD != 2_000_100 {
		t.Errorf("own report = %s %s %s %d days, totals %+v", rep.View, rep.User, rep.Login, rep.Days, rep.Totals)
	}
	if strings.Contains(body, "hubots-secret-intent") || strings.Contains(body, "hubot") {
		t.Errorf("own report leaks another person's usage: %s", body)
	}
	if len(rep.People) != 0 {
		t.Errorf("own report lists people: %+v", rep.People)
	}
	for _, q := range []string{"?user=hubot", "?project=shop", "?days=0", "?days=401", "?days=x"} {
		if code, _ := get(t, as(t, s, usageOctocat), "/api/usage"+q); code != http.StatusBadRequest {
			t.Errorf("GET /api/usage%s = %d, want 400", q, code)
		}
	}
	// Someone with no usage yet gets an empty window, not an error.
	code, body = get(t, as(t, s, usageAdmin), "/api/usage")
	if rep := decode[usageview.UsageReport](t, body); code != http.StatusOK || rep.Totals.Runs != 0 ||
		len(rep.Daily) != usageview.DefaultDays || rep.Login != "Admin" {
		t.Errorf("empty own report = %d %+v", code, rep.Totals)
	}
}

// Everyone's usage needs get on usage/all.
func TestUsageAllNeedsTheGrant(t *testing.T) {
	s, g := usageServer(t)
	code, body := get(t, as(t, s, usageOctocat), "/api/usage/all")
	if code != http.StatusForbidden || !strings.Contains(body, "usage/all") {
		t.Errorf("GET /api/usage/all without the grant = %d %q, want 403 naming usage/all", code, body)
	}
	if !slices.Contains(g.asked, "github:octocat get") {
		t.Errorf("no access review for the caller: %v", g.asked)
	}

	code, body = get(t, as(t, s, usageAdmin), "/api/usage/all")
	if code != http.StatusOK {
		t.Fatalf("GET /api/usage/all as admin = %d %s", code, body)
	}
	rep := decode[usageview.UsageReport](t, body)
	var people []string
	for _, p := range rep.People {
		people = append(people, p.User)
	}
	if rep.View != usageview.ViewAll || rep.Totals.Tokens != 1055 ||
		!slices.Equal(people, []string{"github-id:7", "github-id:42", "org"}) {
		t.Errorf("all = %s %+v people %v", rep.View, rep.Totals, people)
	}

	for q, want := range map[string]int{
		"?user=octocat": http.StatusOK, "?user=github-id:7": http.StatusOK, "?project=shop": http.StatusOK,
		"?user=nobody": http.StatusNotFound, "?days=500": http.StatusBadRequest,
		"?user=" + strings.Repeat("x", 65): http.StatusBadRequest,
	} {
		if code, body := get(t, as(t, s, usageAdmin), "/api/usage/all"+q); code != want {
			t.Errorf("GET /api/usage/all%s = %d %q, want %d", q, code, body, want)
		}
	}
	_, body = get(t, as(t, s, usageAdmin), "/api/usage/all?user=octocat&project=shop")
	if rep := decode[usageview.UsageReport](t, body); rep.View != usageview.ViewUserProject || rep.Totals.Tokens != 100 {
		t.Errorf("user-project = %s %+v", rep.View, rep.Totals)
	}

	code, body = get(t, as(t, s, usageAdmin), "/api/usage/access")
	if a := decode[UsageAccess](t, body); code != http.StatusOK || !a.Self || !a.All || a.Reason != "" {
		t.Errorf("admin access = %d %+v", code, a)
	}
	_, body = get(t, as(t, s, usageHubot), "/api/usage/access")
	if a := decode[UsageAccess](t, body); !a.Self || a.All {
		t.Errorf("person access = %+v", a)
	}
}

// The usage views harden the envelope as the intents views do: a
// same-site page (a preview app) cannot read them with the viewer's cookie.
func TestUsageHardensTheEnvelope(t *testing.T) {
	s, _ := usageServer(t)
	ts := as(t, s, usageOctocat)
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/usage", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Sec-Fetch-Site", "same-site")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusForbidden || res.Header.Get("Content-Security-Policy") == "" {
		t.Errorf("same-site GET /api/usage = %d (CSP %q), want 403 with a CSP", res.StatusCode,
			res.Header.Get("Content-Security-Policy"))
	}
}

// With the views off, none of the routes exist.
func TestUsageOffServesNothing(t *testing.T) {
	s := NewServer(fake.NewClientBuilder().WithScheme(kube.Scheme()).Build(), "patchy", stubAuth{},
		stubGranter{grants: allGrants()}, nil)
	ts := as(t, s, usageAdmin)
	for _, path := range []string{"/api/usage/access", "/api/usage", "/api/usage/all"} {
		if code, _ := get(t, ts, path); code != http.StatusNotFound {
			t.Errorf("GET %s with the views off = %d, want 404", path, code)
		}
	}
	if s.hardened() {
		t.Error("the envelope is hardened with neither view on")
	}
}
