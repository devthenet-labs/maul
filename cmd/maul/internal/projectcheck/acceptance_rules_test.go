// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectcheck

import (
	"context"
	"errors"
	"net/http"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/cmd/maul/internal/checkreport"
)

// TestAcceptanceRules: with verification on, each app repository's branch
// rules are judged for maul/acceptance; with it off, no line at all.
func TestAcceptanceRules(t *testing.T) {
	const web = "https://github.com/acme/shop-web"
	tests := []struct {
		name    string
		enabled bool
		rules   map[string][]RequiredCheck
		err     error
		status  checkreport.Status
		want    string
	}{
		{"required from the App", true, map[string][]RequiredCheck{web: {{Context: "ci"},
			{Context: "maul/acceptance", IntegrationID: 15368}}}, nil, checkreport.Pass, "from App 15368"},
		{"not required", true, map[string][]RequiredCheck{web: {{Context: "ci", IntegrationID: 1}}}, nil,
			checkreport.Warn, "no ruleset on main requires the maul/acceptance status"},
		{"no rules at all", true, nil, nil, checkreport.Warn, "no ruleset"},
		{"what requiring it asks of approvers", true, nil, nil, checkreport.Warn,
			"turn it green only through an approver's /maul accept"},
		{"any source", true, map[string][]RequiredCheck{web: {{Context: "maul/acceptance"}}}, nil,
			checkreport.Warn, "from any source"},
		{"unreadable", true, nil, errors.New("GitHub answered 403 Forbidden"), checkreport.Skip,
			"cannot read the rules on main: GitHub answered 403"},
		{"verification off", false, nil, nil, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			w.project.Spec.Verification = &v1alpha1.ProjectVerification{Enabled: tt.enabled}
			w.github.rules, w.github.rulesErr = tt.rules, tt.err
			r := w.run()
			if !tt.enabled {
				for _, c := range r.Checks {
					if c.Name == CheckAcceptanceRules {
						t.Fatalf("verification off still reported:\n%s", dump(r))
					}
				}
				return
			}
			expect(t, r, CheckAcceptanceRules, "web", tt.status, tt.want)
			if tt.status == checkreport.Warn && r.Failed() != 0 {
				t.Errorf("a WARN failed the run:\n%s", dump(r))
			}
		})
	}
}

// TestAcceptanceRulesHeadUnreadable: without the default branch there is
// nothing to read the rules of.
func TestAcceptanceRulesHeadUnreadable(t *testing.T) {
	w := newWorld(t)
	w.project.Spec.Verification = &v1alpha1.ProjectVerification{Enabled: true}
	delete(w.github.heads, "https://github.com/acme/shop-web")
	expect(t, w.run(), CheckAcceptanceRules, "web", checkreport.Skip, "cannot read the default branch")
}

func TestHTTPGitHubBranchRules(t *testing.T) {
	_, root := newGitHub(t)
	g := &HTTPGitHub{APIURL: root}
	ctx := context.Background()
	got, err := g.BranchRules(ctx, "https://github.com/acme/web", "trunk")
	if err != nil {
		t.Fatal(err)
	}
	want := []RequiredCheck{{Context: "ci"}, {Context: "maul/acceptance", IntegrationID: 15368}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("BranchRules = %+v, want %+v", got, want)
	}
	_, err = g.BranchRules(ctx, "https://github.com/acme/web", "locked")
	var he *httpError
	if !errors.As(err, &he) || he.status != http.StatusForbidden {
		t.Errorf("BranchRules on a forbidden branch = %v, want a 403", err)
	}
}
