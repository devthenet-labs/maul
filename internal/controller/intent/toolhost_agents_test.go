// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/jobs"
)

// testToolsImage is the preview-tools image the agents tool pod runs.
const testToolsImage = "registry.example/maul-preview-tools@sha256:" +
	"cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"

// cliSurface is a cli surface building the app into /work/bin.
func cliSurface(p *v1alpha1.Project) {
	p.Spec.Preview = nil
	p.Spec.Surfaces = &v1alpha1.ProjectSurfaces{CLI: &v1alpha1.CLISurface{
		Build: []string{"go", "build", "-o", "/work/bin/app", "."}, Binary: "/work/bin/app"}}
}

// agentsOn switches the agents-namespace tool pod on (or off) in every
// reconciler, with previews off: a cli or library Project verifies without
// them (Q33).
func (e *env) agentsOn(on bool) {
	for _, s := range []*Settings{&e.project.Settings, &e.intent.Settings, &e.runs.Settings} {
		s.Previews = false
		s.EvaluatorAgents = on
		s.ToolsImage, s.MaxToolPods, s.ToolBuildTimeoutMax = "", 0, 0
		if on {
			s.ToolsImage, s.MaxToolPods, s.ToolBuildTimeoutMax = testToolsImage, 2, v1alpha1.MaxCLIBuildTimeout
		}
	}
}

// newAgentsEnv is a cli Project with no Preview under the evaluator and
// the agents tool pod, its intent driven into review on image (the build's
// accepted runner image; "" for none): it returns the intent's name.
func newAgentsEnv(t *testing.T, image string, mutate ...func(*v1alpha1.Project)) (*env, string) {
	t.Helper()
	e := newVerifyEnv(t, outcomesPlan, append([]func(*v1alpha1.Project){cliSurface}, mutate...)...)
	e.evaluatorOn(0.4)
	e.agentsOn(true)
	name := e.awaiting()
	e.gh.label(1, "maul:approved", approver)
	e.drive(name, v1alpha1.IntentInReview, image)
	return e, name
}

// trees are the intent's workspace trees: its commit-pinned Repositories.
func (e *env) trees(name string) []v1alpha1.Repository {
	e.t.Helper()
	var list v1alpha1.RepositoryList
	if err := e.c.List(context.Background(), &list, client.InNamespace(testNS),
		client.MatchingLabels{v1alpha1.LabelIntent: name}); err != nil {
		e.t.Fatal(err)
	}
	var out []v1alpha1.Repository
	for _, r := range list.Items {
		if r.Spec.Ref.Commit != "" {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b v1alpha1.Repository) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// reviewReady runs review passes, readying every Repository after each.
func (e *env) reviewReady(name string, passes int) {
	e.t.Helper()
	for range passes {
		e.review(name, 1)
		e.readyEach(func(*v1alpha1.Repository) string { return "" })
	}
}

// TestAgentsRoundCreatesAWorkspaceVerification: a cli Project with no
// Preview reaches an outcome verification (not Off) once its checks are
// green: the gate reads the merge base of the pull request's head and its
// base branch (O3), creates two commit-pinned, Intent-owned trees and waits
// for them, then creates an agents PreviewVerification whose workspace is
// the head and base, the build's accepted image and its search path, the
// cli surface with its build timeout, and whose heads hold the workspace
// head; no Preview, no browser, no look-around, the agents timeout.
func TestAgentsRoundCreatesAWorkspaceVerification(t *testing.T) {
	e, name := newAgentsEnv(t, repoImage)
	head := e.get(name).Status.PullRequests[0].HeadSHA
	e.gh.mergeBase = strings.Repeat("b", 40)
	e.review(name, 3)
	if n := len(e.verifications(name)); n != 0 {
		t.Fatalf("%d verifications before the trees are ready", n)
	}
	byCommit := e.checkTrees(name, head, e.gh.mergeBase)
	if !slices.Contains(e.gh.mergeBases, "acme/app main..."+head) {
		t.Errorf("merge bases read = %v, want the head against the base branch", e.gh.mergeBases)
	}

	e.reviewReady(name, 2)
	pv := e.onlyVerification(name)
	checkWorkspace(t, &pv, byCommit[head], byCommit[e.gh.mergeBase])
	wantHeads := []v1alpha1.VerificationHead{{Component: "app", Repository: appRepoURL, Revision: head}}
	if !slices.Equal(pv.Spec.Heads, wantHeads) || pv.Spec.HeadSetDigest != headSetDigest(wantHeads) {
		t.Fatalf("heads = %+v (%s)", pv.Spec.Heads, pv.Spec.HeadSetDigest)
	}
	if tl := pv.Spec.Tools; tl == nil || tl.Browser || !tl.Sandbox || tl.LookAroundRoutes != 0 ||
		len(pv.Spec.Outcomes) != 3 || pv.Spec.Rung != v1alpha1.RungFull ||
		pv.Spec.Timeout.Duration != DefaultEvaluateTimeout+v1alpha1.DefaultCLIBuildTimeout+EvaluationToolsMargin {
		t.Fatalf("verification spec = %+v", pv.Spec)
	}
	if v := e.verification(name); v.State != v1alpha1.VerificationEvaluating || v.Ref == nil || v.Ref.UID != pv.UID {
		t.Fatalf("verification = %+v, want Evaluating on %s", v, pv.Name)
	}
	if body := e.statusBody(); strings.Contains(body, "--intent-previews-enabled") {
		t.Errorf("the status comment says previews are needed, with none needed:\n%s", body)
	}
}

// checkTrees: the intent has exactly two trees, Intent-owned commit
// Repositories of the app at head and base; it returns them by commit.
func (e *env) checkTrees(name, head, base string) map[string]v1alpha1.Repository {
	t := e.t
	t.Helper()
	trees := e.trees(name)
	if len(trees) != 2 {
		t.Fatalf("trees = %+v, want the head and the base", trees)
	}
	byCommit := map[string]v1alpha1.Repository{}
	for _, tr := range trees {
		if !controlledBy(tr.OwnerReferences, e.get(name).UID) || tr.Spec.URL != appRepoURL || tr.Spec.Ref.Branch != "" {
			t.Errorf("tree %s = %+v, want an Intent-owned commit Repository of the app", tr.Name, tr)
		}
		byCommit[tr.Spec.Ref.Commit] = tr
	}
	for _, sha := range []string{head, base} {
		if _, ok := byCommit[sha]; !ok {
			t.Fatalf("no tree at %s: %+v", sha, trees)
		}
	}
	return byCommit
}

// checkWorkspace: pv is an agents verification with no Preview whose
// workspace builds the app's cli at headTree and baseTree in the build's
// accepted image, with no network profile.
func checkWorkspace(t *testing.T, pv *v1alpha1.PreviewVerification, headTree, baseTree v1alpha1.Repository) {
	t.Helper()
	ws := pv.Spec.Workspace
	if !pv.Spec.IsAgents() || ws == nil || pv.Spec.PreviewRef != (v1alpha1.ObjectReference{}) ||
		pv.Spec.PreviewGeneration != 0 {
		t.Fatalf("verification spec = %+v, want an agents verification with no Preview", pv.Spec)
	}
	if ws.Repository != "app" || ws.HeadSHA != headTree.Spec.Ref.Commit || ws.BaseSHA != baseTree.Spec.Ref.Commit ||
		ws.HeadRepositoryRef != (v1alpha1.ObjectReference{Name: headTree.Name, UID: headTree.UID}) ||
		ws.BaseRepositoryRef != (v1alpha1.ObjectReference{Name: baseTree.Name, UID: baseTree.UID}) ||
		ws.SandboxImage != repoImage || ws.SandboxSearchPath != "/usr/bin:/bin" || ws.NetworkProfile != "" {
		t.Fatalf("workspace = %+v", ws)
	}
	if c := ws.Surface.CLI; c == nil || ws.Surface.Library != nil || c.Binary != "/work/bin/app" ||
		!slices.Equal(c.Build, []string{"go", "build", "-o", "/work/bin/app", "."}) ||
		c.BuildTimeout.Duration != v1alpha1.DefaultCLIBuildTimeout {
		t.Fatalf("surface = %+v", ws.Surface)
	}
}

// TestEvaluatorCreatedAfterToolsStatusAgents: as in the slot placement, an
// agents round launches no evaluator until its tool pod's address is in
// status and ready (the agents host writes it), then exactly one, pinned
// to that address.
func TestEvaluatorCreatedAfterToolsStatusAgents(t *testing.T) {
	e, name := newAgentsEnv(t, repoImage)
	e.reviewReady(name, 4)
	pv := e.onlyVerification(name)
	noEvaluator := func(when string) {
		t.Helper()
		e.runRuns()
		if runs := e.runsOf(name, v1alpha1.IntentStageEvaluate); len(runs) != 0 {
			t.Fatalf("%s: evaluate runs = %d, want none before status.tools", when, len(runs))
		}
		if n := len(e.specsOf("evaluate")); n != 0 {
			t.Fatalf("%s: %d evaluate Jobs, want none", when, n)
		}
	}
	noEvaluator("pending")
	e.playTools(pv.Name, v1alpha1.VerificationRunning, "", false)
	e.review(name, 2)
	noEvaluator("running, no tool pod")
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, false)
	e.review(name, 2)
	noEvaluator("tool pod not ready")
	e.toolsSecret(&pv, "")
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.checkEvaluateRun(name, &pv)
	e.runRuns()
	evals := e.specsOf("evaluate")
	if len(evals) != 1 || evals[0].ToolHost == nil || evals[0].ToolHost.IP != toolPodIP ||
		evals[0].ToolHost.Name != jobs.ToolsHostName || evals[0].ArtifactURL != "" || evals[0].RunnerImage != "" {
		t.Fatalf("evaluate Jobs = %+v, want one evaluator of the tool pod holding no tree", evals)
	}
}

// TestAgentsWithoutAnAcceptedImageIsUnconfirmed: with no accepted runner
// image on the build's Repository (O8) no tree and no tool pod is made, the
// verification is Unconfirmed and the intent stays in review, never
// blocked.
func TestAgentsWithoutAnAcceptedImageIsUnconfirmed(t *testing.T) {
	e, name := newAgentsEnv(t, "", func(p *v1alpha1.Project) { p.Spec.RequireRepositoryImage = new(false) })
	e.reviewReady(name, 4)
	if n, trees := len(e.verifications(name)), e.trees(name); n != 0 || len(trees) != 0 {
		t.Fatalf("%d verifications and trees %+v with no accepted image", n, trees)
	}
	in := e.get(name)
	if v := in.Status.Verification; v.State != v1alpha1.VerificationUnconfirmed || v.Ref != nil {
		t.Fatalf("verification = %+v, want Unconfirmed", v)
	}
	if in.Status.Phase != v1alpha1.IntentInReview {
		t.Fatalf("phase = %s, want InReview", in.Status.Phase)
	}
}

// TestAgentsSearchPathOutsideTheSchemaIsUnconfirmed: an accepted image
// whose search path the PreviewVerification schema would refuse settles
// the round Unconfirmed instead of failing to create it.
func TestAgentsSearchPathOutsideTheSchemaIsUnconfirmed(t *testing.T) {
	e, name := newAgentsEnv(t, repoImage)
	var list v1alpha1.RepositoryList
	if err := e.c.List(context.Background(), &list, client.InNamespace(testNS)); err != nil {
		t.Fatal(err)
	}
	for i := range list.Items {
		r := &list.Items[i]
		if r.Status.RunnerImage != nil {
			r.Status.RunnerImage.SearchPath = "/usr/bin:relative"
			if err := e.c.Status().Update(context.Background(), r); err != nil {
				t.Fatal(err)
			}
		}
	}
	e.reviewReady(name, 4)
	if v := e.verification(name); v.State != v1alpha1.VerificationUnconfirmed || len(e.verifications(name)) != 0 {
		t.Fatalf("verification = %+v, %d verifications; want Unconfirmed and none", v, len(e.verifications(name)))
	}
}

// TestAgentsHeadsMovedDropsTheVerification: a push to the pull request
// supersedes the in-flight agents verification: it is deleted, and the
// next round's trees are at the new head.
func TestAgentsHeadsMovedDropsTheVerification(t *testing.T) {
	e, name := newAgentsEnv(t, repoImage)
	e.reviewReady(name, 4)
	pv := e.onlyVerification(name)
	moved := strings.Repeat("f", 40)
	e.pushHead(e.get(name).Status.PullRequests[0].Number, moved)
	// A head maul did not push halts automatic fixes, which spaces automatic
	// rounds by verifyInterval.
	e.reviewReady(name, 8)
	var gone v1alpha1.PreviewVerification
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(&pv), &gone); !kerrors.IsNotFound(err) {
		t.Fatalf("the superseded verification is still there: %v", err)
	}
	want := headSetDigest([]v1alpha1.VerificationHead{{Component: "app", Repository: appRepoURL, Revision: moved}})
	if v := e.verification(name); v.HeadSetDigest != want {
		t.Fatalf("verification = %+v, want the moved head set", v)
	}
	if !slices.ContainsFunc(e.trees(name), func(r v1alpha1.Repository) bool { return r.Spec.Ref.Commit == moved }) {
		t.Fatalf("no tree at the moved head: %+v", e.trees(name))
	}
}

// TestAgentsTreesUnavailable: trees that never become Ready within the
// bound, or a commit GitHub cannot serve (CommitNotFound), make the
// verification Unavailable; no PreviewVerification is created.
func TestAgentsTreesUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stall bool
	}{{"never ready", false}, {"commit not found", true}} {
		t.Run(tc.name, func(t *testing.T) {
			e, name := newAgentsEnv(t, repoImage)
			e.review(name, 3)
			trees := e.trees(name)
			if len(trees) != 2 {
				t.Fatalf("trees = %+v", trees)
			}
			if tc.stall {
				r := trees[0]
				r.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionStalled, Status: metav1.ConditionTrue,
					Reason: v1alpha1.ReasonCommitNotFound, LastTransitionTime: metav1.NewTime(e.clock.Now())}}
				if err := e.c.Status().Update(context.Background(), &r); err != nil {
					t.Fatal(err)
				}
				e.review(name, 1)
			} else {
				e.review(name, 5)
				if v := e.verification(name); v.State == v1alpha1.VerificationUnavailable {
					t.Fatalf("verification = %+v, Unavailable before the bound", v)
				}
				e.clock.Advance(treeWaitMax)
				e.review(name, 1)
			}
			if v := e.verification(name); v.State != v1alpha1.VerificationUnavailable || v.Ref != nil ||
				len(e.verifications(name)) != 0 {
				t.Fatalf("verification = %+v, %d verifications; want Unavailable and none", v,
					len(e.verifications(name)))
			}
		})
	}
}

// TestAgentsProjectConditions: a verifying cli or library Project says why
// its intents are not evaluated: AgentsToolPodNotEnabled while the tool pod
// is off (whatever previews say), SurfaceCombinationUnsupported beside a
// preview (O6), and RepositoryImageRequired while repository images are off
// (O8); with the tool pod on and previews off it is not Off at all. A
// Project with a preview keeps its conditions as before.
func TestAgentsProjectConditions(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		mutate                func(*v1alpha1.Project)
		agents, previews      bool
		repositoryImagesOff   bool
		wantOff, wantImageReq string
	}{
		{"cli, agents on", cliSurface, true, false, false, "", ""},
		{"cli, agents off", cliSurface, false, false, false, v1alpha1.ReasonAgentsToolPodNotEnabled, ""},
		{"cli, agents off, previews on", cliSurface, false, true, false, v1alpha1.ReasonAgentsToolPodNotEnabled, ""},
		{"cli beside a preview", func(p *v1alpha1.Project) { cliSurface(p); p.Spec.Preview = &appPreview }, true,
			false, false, v1alpha1.ReasonSurfaceCombinationUnsupported, ""},
		{"cli, repository images off", cliSurface, true, false, true, "", v1alpha1.ReasonRepositoryImageRequired},
		{"preview, previews off", func(*v1alpha1.Project) {}, true, false, false, ReasonPreviewsDisabled, ""},
		{"preview, previews on", func(*v1alpha1.Project) {}, true, true, false, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newVerifyEnv(t, "", tc.mutate)
			e.evaluatorOn(0.4)
			e.agentsOn(tc.agents)
			e.verifying(true, tc.previews)
			e.project.RepositoryImagesOff = tc.repositoryImagesOff
			e.reconcileProject()
			conds := e.getProject().Status.Conditions
			off := meta.FindStatusCondition(conds, ConditionVerificationOff)
			switch {
			case tc.wantOff == "" && off != nil:
				t.Errorf("%s = %+v, want none", ConditionVerificationOff, off)
			case tc.wantOff != "" && (off == nil || off.Status != metav1.ConditionTrue || off.Reason != tc.wantOff):
				t.Errorf("%s = %+v, want %s", ConditionVerificationOff, off, tc.wantOff)
			}
			img := meta.FindStatusCondition(conds, ConditionRepositoryImageRequired)
			switch {
			case tc.wantImageReq == "" && img != nil:
				t.Errorf("%s = %+v, want none", ConditionRepositoryImageRequired, img)
			case tc.wantImageReq != "" && (img == nil || img.Status != metav1.ConditionTrue ||
				img.Reason != tc.wantImageReq):
				t.Errorf("%s = %+v, want %s", ConditionRepositoryImageRequired, img, tc.wantImageReq)
			}
			if !meta.IsStatusConditionTrue(conds, v1alpha1.ConditionReady) {
				t.Errorf("Ready = %+v, want True: these conditions never take a Project down", conds)
			}
		})
	}
}

// TestAgentsOffKeepsACLIProjectOff: with the tool pod off, a cli Project's
// intent in review is Off and nothing of the agents round is made.
func TestAgentsOffKeepsACLIProjectOff(t *testing.T) {
	e := newVerifyEnv(t, outcomesPlan, cliSurface)
	e.evaluatorOn(0.4)
	e.agentsOn(false)
	name := e.awaiting()
	e.gh.label(1, "maul:approved", approver)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.reviewReady(name, 4)
	if v := e.verification(name); v.State != v1alpha1.VerificationOff || len(e.verifications(name)) != 0 ||
		len(e.trees(name)) != 0 || len(e.gh.mergeBases) != 0 {
		t.Fatalf("verification = %+v, %d verifications, trees %+v, merge bases %v", v, len(e.verifications(name)),
			e.trees(name), e.gh.mergeBases)
	}
}

// TestAgentsWorkspaceSurface: a library surface's workspace carries its
// language; a cli's build timeout is its own, never over
// --intent-tool-build-timeout-max; the verification's wall clock is the
// evaluate timeout, that build timeout and the margin.
func TestAgentsWorkspaceSurface(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*v1alpha1.Project)
		max     time.Duration
		wantLib bool
		build   time.Duration
	}{
		{"library", func(p *v1alpha1.Project) {
			p.Spec.Surfaces = &v1alpha1.ProjectSurfaces{Library: &v1alpha1.LibrarySurface{
				Language: v1alpha1.LibraryLanguageGo}}
		}, 0, true, v1alpha1.DefaultCLIBuildTimeout},
		{"cli over the ceiling", func(p *v1alpha1.Project) {
			p.Spec.Surfaces.CLI.BuildTimeout = &metav1.Duration{Duration: 15 * time.Minute}
		}, 12 * time.Minute, false, 12 * time.Minute},
		{"cli under the ceiling", func(p *v1alpha1.Project) {
			p.Spec.Surfaces.CLI.BuildTimeout = &metav1.Duration{Duration: 3 * time.Minute}
		}, 0, false, 3 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, name := newAgentsEnv(t, repoImage, tc.mutate)
			if tc.max > 0 {
				e.intent.Settings.ToolBuildTimeoutMax = tc.max
			}
			e.reviewReady(name, 4)
			pv := e.onlyVerification(name)
			sf := pv.Spec.Workspace.Surface
			switch {
			case tc.wantLib && (sf.Library == nil || sf.Library.Language != v1alpha1.LibraryLanguageGo || sf.CLI != nil):
				t.Fatalf("surface = %+v, want the go library", sf)
			case !tc.wantLib && (sf.CLI == nil || sf.CLI.BuildTimeout.Duration != tc.build):
				t.Fatalf("surface = %+v, want a %s cli build", sf, tc.build)
			}
			if want := DefaultEvaluateTimeout + tc.build + EvaluationToolsMargin; pv.Spec.Timeout.Duration != want {
				t.Fatalf("timeout = %s, want %s", pv.Spec.Timeout.Duration, want)
			}
		})
	}
}

// TestVerifiesProjectAgreesWithVerificationOff: the predicate the plan
// (planAsk), the review (pass.verifies) and the hand-off share says a
// Project verifies exactly when its VerificationOff condition is absent, so
// a cli Project with the agents tool pod off is never asked for outcomes
// (nor told "not asked") while its condition says AgentsToolPodNotEnabled,
// whatever previews say.
func TestVerifiesProjectAgreesWithVerificationOff(t *testing.T) {
	projects := map[string]func(*v1alpha1.Project){
		"preview": func(*v1alpha1.Project) {},
		"cli":     cliSurface,
		"library": func(p *v1alpha1.Project) {
			p.Spec.Preview = nil
			p.Spec.Surfaces = &v1alpha1.ProjectSurfaces{Library: &v1alpha1.LibrarySurface{
				Language: v1alpha1.LibraryLanguageGo}}
		},
	}
	for name, mutate := range projects {
		for _, verification := range []bool{false, true} {
			for _, previews := range []bool{false, true} {
				for _, evaluator := range []bool{false, true} {
					for _, agents := range []bool{false, true} {
						p := testProject()
						p.Spec.Verification = &v1alpha1.ProjectVerification{Enabled: true}
						mutate(p)
						s := Settings{Verification: verification, Previews: previews, Evaluator: evaluator,
							EvaluatorAgents: agents && evaluator}
						off, _ := s.verificationOff(p)
						if got, want := s.verifiesProject(p), verification && off == ""; got != want {
							t.Errorf("%s, verification %v, previews %v, evaluator %v, agents %v: verifiesProject = %v, "+
								"VerificationOff %q", name, verification, previews, evaluator, agents, got, off)
						}
						if ask := s.planAsk(p); !s.verifiesProject(p) && ask.verified {
							t.Errorf("%s: planAsk = %+v for a Project that does not verify", name, ask)
						}
					}
				}
			}
		}
	}
}

// TestAgentsWorkspaceGetsTheBuildsProfile: the tool pod's network profile
// is the one the build of the surface's repository got (7.8: never wider),
// read through the same guard; a repository no profile matches gets none
// (checkWorkspace's case).
func TestAgentsWorkspaceGetsTheBuildsProfile(t *testing.T) {
	e := newVerifyEnv(t, outcomesPlan, cliSurface)
	e.evaluatorOn(0.4)
	e.agentsOn(true)
	withProfiles(t, e, "acme/app")
	name := e.awaiting()
	e.gh.label(1, "maul:approved", approver)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.reviewReady(name, 4)
	pv := e.onlyVerification(name)
	builds := e.runsOf(name, v1alpha1.IntentStageBuild)
	if len(builds) != 1 || builds[0].Status.RunnerImage == nil || builds[0].Status.RunnerImage.NetworkProfile != "go" {
		t.Fatalf("build runs = %+v, want one on the go profile", builds)
	}
	if ws := pv.Spec.Workspace; ws == nil || ws.NetworkProfile != "go" || ws.SandboxImage != repoImage {
		t.Fatalf("workspace = %+v, want the build's image and its go profile", ws)
	}
}

// TestAgentsMergeBaseRefusedIsUnavailable: GitHub refusing the compare
// (here a 404: the head is not in the repository) leaves the round with no
// base to build, so the verification is Unavailable, never failed, and no
// tree or PreviewVerification is made; a later pass that can read the
// merge base runs the round.
func TestAgentsMergeBaseRefusedIsUnavailable(t *testing.T) {
	e, name := newAgentsEnv(t, repoImage)
	e.gh.failNext("MergeBase", ghError(http.StatusNotFound, "No common ancestor"))
	e.review(name, 3)
	if v := e.verification(name); v.State != v1alpha1.VerificationUnavailable || v.Ref != nil ||
		len(e.verifications(name)) != 0 {
		t.Fatalf("verification = %+v, %d verifications; want Unavailable and none", v, len(e.verifications(name)))
	}
	e.reviewReady(name, 4)
	if pv := e.onlyVerification(name); !pv.Spec.IsAgents() {
		t.Fatalf("verification = %+v, want the agents round once the merge base reads", pv.Spec)
	}
}

// TestAgentsUnrunnableRoundHandsOff: a round that cannot run before any
// evaluation was made (no accepted image) hands the pull request over as
// couldn't confirm, once, and then writes nothing more: the pass goes on to
// the review's own rounds instead of rewriting a hand-off that was never
// recorded.
func TestAgentsUnrunnableRoundHandsOff(t *testing.T) {
	e, name := newAgentsEnv(t, "", func(p *v1alpha1.Project) { p.Spec.RequireRepositoryImage = new(false) })
	e.reviewReady(name, 4)
	in := e.get(name)
	v := in.Status.Verification
	if v.State != v1alpha1.VerificationUnconfirmed || v.Handoff == nil || !v.Handoff.Released ||
		v.Handoff.Reason != v1alpha1.HandoffReasonUnconfirmed {
		t.Fatalf("verification = %+v (hand-off %+v), want Unconfirmed and the hand-off released", v, v.Handoff)
	}
	e.review(name, 3) // the status comment catches up
	rv := e.get(name).ResourceVersion
	e.review(name, 3)
	if got := e.get(name).ResourceVersion; got != rv {
		t.Fatalf("the intent was written again (%s -> %s) with nothing changed", rv, got)
	}
}

// TestAgentsRequestAfterARoundSettlesWhenTheRoundCannotRun: an approver's
// /maul verify at heads an earlier round already ran at (its reference
// still on record) settles, as the first round does, when the round cannot
// run: with no accepted image Unconfirmed (SandboxImageUnavailable), with
// no fetchable tree Unavailable (TreeUnavailable); the request is consumed,
// the hand-off released as couldn't confirm, and a line logged. S2-B2: the
// request stayed pending, silently, for ever.
func TestAgentsRequestAfterARoundSettlesWhenTheRoundCannotRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove func(t *testing.T, e *env)
		state  v1alpha1.IntentVerificationState
		reason v1alpha1.VerificationUnavailableReason
		log    string
	}{
		{"no accepted image", dropRunnerImages, v1alpha1.VerificationUnconfirmed,
			v1alpha1.VerificationUnavailableSandboxImage, "the agents round cannot run"},
		{"no fetchable tree", func(_ *testing.T, e *env) {
			e.gh.failNext("MergeBase", ghError(http.StatusNotFound, "No common ancestor"))
		}, v1alpha1.VerificationUnavailable, v1alpha1.VerificationUnavailableTree,
			"the agents round's trees are unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, name := newAgentsEnv(t, repoImage)
			e.reviewReady(name, 4)
			pv := e.onlyVerification(name)
			// The earlier round settled without a judgement; its reference
			// stays on record at these heads.
			e.setStatus(name, func(in *v1alpha1.Intent) {
				v := in.Status.Verification
				v.State, v.UnavailableReason = v1alpha1.VerificationUnavailable, ""
				v.Requested = &v1alpha1.VerifyRequest{By: approver, ByID: 7, At: metav1.NewTime(e.clock.Now()),
					CommentID: 99}
			})
			tc.remove(t, e)
			var logs bytes.Buffer
			e.intent.Log = slog.New(slog.NewTextHandler(&logs, nil))
			e.review(name, 3)

			v := e.verification(name)
			if v.Requested != nil || v.State != tc.state || v.UnavailableReason != tc.reason {
				t.Fatalf("verification = %+v, want %s (%s) with the request consumed", v, tc.state, tc.reason)
			}
			if h := v.Handoff; h == nil || !h.Released || h.Reason != v1alpha1.HandoffReasonUnconfirmed {
				t.Fatalf("hand-off = %+v, want released as couldn't confirm", h)
			}
			if vs := e.verifications(name); len(vs) != 1 || vs[0].Name != pv.Name {
				t.Fatalf("verifications = %+v, want only the earlier round", vs)
			}
			if !strings.Contains(logs.String(), tc.log) {
				t.Fatalf("logs do not say why the request settled:\n%s", logs.String())
			}
			if in := e.get(name); in.Status.Phase != v1alpha1.IntentInReview {
				t.Fatalf("phase = %s, want InReview", in.Status.Phase)
			}
		})
	}
}

// dropRunnerImages clears every Repository's accepted runner image.
func dropRunnerImages(t *testing.T, e *env) {
	t.Helper()
	var list v1alpha1.RepositoryList
	if err := e.c.List(context.Background(), &list, client.InNamespace(testNS)); err != nil {
		t.Fatal(err)
	}
	for i := range list.Items {
		if r := &list.Items[i]; r.Status.RunnerImage != nil {
			r.Status.RunnerImage = nil
			if err := e.c.Status().Update(context.Background(), r); err != nil {
				t.Fatal(err)
			}
		}
	}
}
