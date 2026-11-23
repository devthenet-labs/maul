// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/e2e/fakeoidc"
	"github.com/devthenet-labs/maul/internal/rbactest"
	"github.com/devthenet-labs/maul/internal/templates"
)

// TestIntentEvaluateEvidenceOnStatusPage: the status page's evaluation
// evidence view (S1-23) over a real round. The scripted evaluator passes
// both outcomes and reports one look-around anomaly, a note for the
// reviewer: maul counts both verdicts by the calls they cite and hands the
// link over as ready, the note shown beside them. Served by the shipped
// status-server behind a real OIDC sign-in and the envtest API server's
// RBAC: a tier-1 viewer sees maul's decided outcomes in the comment's words
// with the calls it counted for each, the anomaly, the hand-off and the
// ledger index, never an output, a request or the evaluator's claims; a
// tier-2 reader also sees what each cited call sent and returned and the
// evaluator's verdicts.
func TestIntentEvaluateEvidenceOnStatusPage(t *testing.T) {
	const anomaly = "The version endpoint logs a warning on every request"
	e := startEvaluate(t, evalOptions{})
	e.kubelet.setAgentEnv("MAUL_FAKE_CLAUDE_ANOMALY", anomaly)
	_, name, pr := e.toReview(t)
	e.serve(t, name, pr.HeadSHA)
	in := e.waitEval(t, name, "the round judged, the anomaly recorded", func(v *v1alpha1.IntentVerification) bool {
		return v.Handoff != nil && v.Handoff.Released && len(v.Anomalies) == 1
	})
	if h := in.Status.Verification.Handoff; h.Reason != v1alpha1.HandoffReasonReady {
		t.Errorf("handoff %+v, want released ready: the anomaly is a note", h)
	}
	eventually(t, "the preview comment to show the note", func() bool {
		return strings.Contains(e.previewComment(name, pr.Number), anomaly)
	})
	runs := e.stageRuns(t, name, v1alpha1.IntentStageEvaluate)
	if len(runs) != 1 {
		t.Fatalf("evaluate runs = %d, want 1", len(runs))
	}

	oidc, err := fakeoidc.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(oidc.Close)
	oidc.AddUser(fakeoidc.User{Login: "viv", Name: "Viv Viewer"})
	oidc.AddUser(fakeoidc.User{Login: "rex", Name: "Rex Reader"})
	grantProjectTiers(t, e.cl, projectName, "github:viv", "github:rex")
	listen := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	authCfg := writeIntentsAuthConfig(t, oidc.URL, "http://"+listen+"/oauth2/callback")
	e.cl.controller(t, "status-server", "--listen-addr", listen, "--auth-config", authCfg,
		"--agent-namespace", agentsNS, "--intents-enabled")
	base := "http://" + listen
	url := base + "/api/intents/" + name + "/runs/" + runs[0].Name + "/evaluation"

	for _, tc := range []struct {
		login, tier string
		outputs     bool
	}{{"viv", "intents", false}, {"rex", "transcripts", true}} {
		var ev evidenceView
		getJSON(t, signIn(t, base, oidc, tc.login), url, &ev)
		ev.check(t, tc.login, tc.tier, tc.outputs, anomaly)
	}
}

// evidenceView is the evaluation evidence the status page serves, as much
// of it as the case reads (internal/web's RunEvaluation).
type evidenceView struct {
	Tier    string `json:"tier"`
	Current bool   `json:"current"`
	Handoff *struct {
		Mode     string `json:"mode"`
		Released bool   `json:"released"`
		Reason   string `json:"reason"`
	} `json:"handoff"`
	Lead     string `json:"lead"`
	Outcomes []struct {
		ID      string  `json:"id"`
		Result  string  `json:"result"`
		Words   string  `json:"words"`
		Cites   []int32 `json:"cites"`
		Verdict string  `json:"verdict"`
	} `json:"outcomes"`
	// Checks is the retired saved-check view, which is never served.
	Checks []struct {
		ID string `json:"id"`
	} `json:"checks"`
	Anomalies []struct {
		Summary string `json:"summary"`
	} `json:"anomalies"`
	Ledger *struct {
		Count   int32           `json:"count"`
		Outputs bool            `json:"outputs"`
		Entries []evidenceEntry `json:"entries"`
	} `json:"ledger"`
}

// evidenceEntry is one ledger entry of the view.
type evidenceEntry struct {
	Seq     int32  `json:"seq"`
	Kind    string `json:"kind"`
	Verb    string `json:"verb"`
	Refused bool   `json:"refused"`
	Args    string `json:"args"`
	Output  string `json:"output"`
	Own     bool   `json:"own"`
}

// check holds the view login was served to its tier: the current round,
// released ready, both outcomes passed in the comment's words, each with
// the calls maul counted for it (the evaluator's own unrefused calls of
// the index), no saved check, the anomaly, every ledger entry indexed; the
// evaluator's verdicts, what the cited calls sent and returned and the
// ledger's outputs only with outputs (tier 2).
func (ev evidenceView) check(t *testing.T, login, tier string, outputs bool, anomaly string) {
	t.Helper()
	if ev.Tier != tier || !ev.Current || ev.Handoff == nil || !ev.Handoff.Released ||
		ev.Handoff.Reason != string(v1alpha1.HandoffReasonReady) || ev.Handoff.Mode != "gated" {
		t.Errorf("%s: tier %q current %v handoff %+v; want %s, the current round, released ready",
			login, ev.Tier, ev.Current, ev.Handoff, tier)
	}
	if len(ev.Outcomes) != 2 || ev.Checks != nil || ev.Lead != templates.ObservedLead {
		t.Errorf("%s: outcomes %+v checks %+v lead %q, want both outcomes, no check and the comment's lead",
			login, ev.Outcomes, ev.Checks, ev.Lead)
	}
	if len(ev.Anomalies) != 1 || ev.Anomalies[0].Summary != anomaly {
		t.Errorf("%s: anomalies %+v, want the evaluator's one", login, ev.Anomalies)
	}
	if ev.Ledger == nil || ev.Ledger.Count == 0 || len(ev.Ledger.Entries) != int(ev.Ledger.Count) ||
		ev.Ledger.Outputs != outputs {
		t.Fatalf("%s: ledger %+v, want every entry indexed, outputs %v", login, ev.Ledger, outputs)
	}
	ev.checkOutcomes(t, login, tier, outputs)
	shown := slices.ContainsFunc(ev.Ledger.Entries, func(x evidenceEntry) bool { return x.Output != "" })
	if shown != outputs {
		t.Errorf("%s: ledger outputs shown %v, want %v", login, shown, outputs)
	}
}

// checkOutcomes holds both outcomes passed in the comment's words, each
// with the calls maul counted for it, the evaluator's verdict only with
// outputs (tier 2).
func (ev evidenceView) checkOutcomes(t *testing.T, login, tier string, outputs bool) {
	t.Helper()
	for _, o := range ev.Outcomes {
		if o.Result != "passed" || o.Words != templates.ObservedOutcomeWords("passed", "seen-to-hold") ||
			len(o.Cites) == 0 {
			t.Errorf("%s: outcome %+v, want passed in the comment's words, with the calls maul counted", login, o)
		}
		if (o.Verdict != "") != outputs {
			t.Errorf("%s: outcome %s shows the evaluator's verdict %q at tier %s", login, o.ID, o.Verdict, tier)
		}
		ev.checkCited(t, login, tier, outputs, o.ID, o.Cites)
	}
}

// checkCited holds the calls maul counted for outcome id: each an
// unrefused call of the evaluator's in the index, what it sent and returned
// and its own label shown only with outputs (tier 2).
func (ev evidenceView) checkCited(t *testing.T, login, tier string, outputs bool, id string, cites []int32) {
	t.Helper()
	for _, seq := range cites {
		i := slices.IndexFunc(ev.Ledger.Entries, func(x evidenceEntry) bool { return x.Seq == seq })
		if i < 0 {
			t.Errorf("%s: outcome %s cites %d, which the index does not hold", login, id, seq)
			continue
		}
		c := ev.Ledger.Entries[i]
		if c.Kind != "agent" || c.Refused {
			t.Errorf("%s: outcome %s counts call %+v, not an unrefused call of the evaluator's", login, id, c)
		}
		if (c.Args != "" && c.Output != "") != outputs {
			t.Errorf("%s: cited call %+v shows what it sent and returned %v at tier %s", login, c, !outputs, tier)
		}
		if c.Own && !outputs {
			t.Errorf("%s: cited call %+v is labelled at tier %s", login, c, tier)
		}
	}
}

// grantProjectTiers grants viewer project's intents tier and reader its
// transcripts tier too, in the documented grammar, and waits for the
// authorizer to see both.
func grantProjectTiers(t *testing.T, cl *cluster, project, viewer, reader string) {
	t.Helper()
	ctx := context.Background()
	for name, grant := range map[string]struct {
		user string
		subs []string
	}{
		project + "-viewers": {viewer, []string{"projects/intents"}},
		project + "-content": {reader, []string{"projects/intents", "projects/transcripts"}},
	} {
		if err := cl.client.Create(ctx, &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Rules: []rbacv1.PolicyRule{{APIGroups: []string{"maul.run"}, Resources: grant.subs,
				ResourceNames: []string{project}, Verbs: []string{"get"}}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := cl.client.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: grant.user}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	review := func(user, sub string) authorizationv1.SubjectAccessReviewSpec {
		return rbactest.Review(user, nil, authorizationv1.ResourceAttributes{Namespace: namespace,
			Verb: "get", Group: "maul.run", Resource: "projects", Subresource: sub, Name: project})
	}
	rbactest.WaitAllowed(t, cl.client, review(viewer, "intents"), review(reader, "transcripts"))
}
