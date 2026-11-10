// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentshost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/attribution"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/previewtools"
	"github.com/devthenet-labs/maul/internal/runnerguard"
	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolledger"
	"github.com/devthenet-labs/maul/internal/toolpod"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// TestStartRendersTheBuildsToolPod: a due agents verification gets one
// tool pod, named after it, of the workspace's trees on the build's image,
// its profile from that build's spec, the relay's config in workspace mode
// with the sandbox environment and the image's baked module cache; it runs
// once created, with no tools status yet.
func TestStartRendersTheBuildsToolPod(t *testing.T) {
	w := newWorld(t)
	h := newHarness(t, w)
	h.reconcile(t)
	if len(h.tools.created) != 1 {
		t.Fatalf("created %d tool pods, want 1", len(h.tools.created))
	}
	spec := h.tools.created[0]
	if spec.Name != pvName || spec.Verification != string(pvUID) || spec.Repo != "octo/render" ||
		spec.Image != image || spec.ToolsImage != toolsImage || spec.SearchPath != searchPath ||
		spec.Timeout != 45*time.Minute || !toolwire.IsKey(spec.Key) {
		t.Errorf("spec = %+v", spec)
	}
	checkTrees(t, spec)
	checkRelayConfig(t, spec)
	pv := h.pv(t)
	if pv.Status.Phase != v1alpha1.VerificationRunning || pv.Status.Tools != nil || pv.Status.StartedAt == nil {
		t.Errorf("status = %+v, want Running with no tools yet", pv.Status)
	}
}

// checkTrees: spec builds the workspace's head and base trees.
func checkTrees(t *testing.T, spec jobs.ToolPodSpec) {
	t.Helper()
	trees := map[string]jobs.Tree{}
	for _, tr := range spec.Trees {
		trees[tr.Key] = tr
	}
	if trees[jobs.ToolSideHead].BaseSHA != headSHA || trees[jobs.ToolSideHead].ArtifactDigest != headDigest ||
		trees[jobs.ToolSideBase].BaseSHA != baseSHA || trees[jobs.ToolSideBase].ArtifactDigest != baseDigest ||
		trees[jobs.ToolSideHead].URL != repoURL {
		t.Errorf("trees = %+v", spec.Trees)
	}
}

// checkRelayConfig: spec's relay config is the workspace's, with the
// sandbox environment less the shim's own names, plus the baked cache.
func checkRelayConfig(t *testing.T, spec jobs.ToolPodSpec) {
	t.Helper()
	cfg, err := previewtools.ParseConfig([]byte(spec.Data[previewtools.ConfigFile]))
	if err != nil {
		t.Fatalf("relay config: %v", err)
	}
	ws := cfg.Workspace
	if !cfg.IsWorkspace() || ws == nil || ws.Binary != "/work/bin/bb" || ws.BuildTimeoutSeconds != 600 ||
		ws.SearchPath != searchPath || cfg.MaxToolCalls != 80 || cfg.ReplayOnly {
		t.Fatalf("config = %+v / %+v", cfg, ws)
	}
	want := []toolexec.EnvVar{{Name: "GOPROXY", Value: "off"}, {Name: "GOMODCACHE", Value: "/opt/go/pkg/mod"}}
	if !slices.Equal(ws.Env, want) {
		t.Errorf("env = %v, want %v (the shim's own names and PATH dropped, GOROOT not passable)", ws.Env, want)
	}
	if spec.Data[toolpod.OutcomesKey] == "" {
		t.Error("no outcomes in the relay's data")
	}
}

// TestStartReusesTheSecretsKey: a Secret an earlier pass created keeps its
// key, so CreateToolPod adopts it rather than refusing other data.
func TestStartReusesTheSecretsKey(t *testing.T) {
	w := newWorld(t)
	h := newHarness(t, w)
	key := strings.Repeat("ab", 32)
	h.tools.keys[pvName], h.tools.keyUIDs[pvName] = key, string(pvUID)
	h.reconcile(t)
	if len(h.tools.created) != 1 || h.tools.created[0].Key != key {
		t.Fatalf("created %+v, want the Secret's key reused", h.tools.created)
	}
}

// TestStartRefusesAnotherVerificationsJob: a Job under the name that is
// another verification's is deleted, never adopted, and the round waits.
func TestStartRefusesAnotherVerificationsJob(t *testing.T) {
	w := newWorld(t)
	h := newHarness(t, w)
	h.tools.job = toolJob(pvName, "other-uid")
	h.tools.keys[pvName], h.tools.keyUIDs[pvName] = strings.Repeat("cd", 32), "other-uid"
	h.reconcile(t)
	if len(h.tools.created) != 0 || !slices.Contains(h.tools.deleted, pvName) {
		t.Fatalf("created %d, deleted %v; want the other's Job deleted and none created", len(h.tools.created),
			h.tools.deleted)
	}
	if pv := h.pv(t); pv.Status.Phase != v1alpha1.VerificationPending {
		t.Errorf("phase = %s, want Pending", pv.Status.Phase)
	}
}

// TestSaturationWaits: at the tool pod bound the round waits, never a
// verdict and never Infra, showing tool-pods-saturated; past its own
// timeout it is Infra.
func TestSaturationWaits(t *testing.T) {
	w := newWorld(t)
	h := newHarness(t, w)
	h.tools.listed = []batchv1.Job{*toolJob("a", "a"), *toolJob("b", "b")}
	if res := h.reconcile(t); res.RequeueAfter == 0 {
		t.Error("no requeue while saturated")
	}
	pv := h.pv(t)
	if len(h.tools.created) != 0 || pv.Status.Phase != v1alpha1.VerificationPending ||
		pv.Status.Reason != v1alpha1.VerificationReasonToolPodsSaturated {
		t.Fatalf("created %d, status %+v; want a saturated wait", len(h.tools.created), pv.Status)
	}
	// A finished tool Job does not count.
	finish(&h.tools.listed[1], t0)
	h.reconcile(t)
	if len(h.tools.created) != 1 || h.pv(t).Status.Reason != "" {
		t.Fatalf("created %d, reason %q; want it started", len(h.tools.created), h.pv(t).Status.Reason)
	}
	w2 := newWorld(t)
	h2 := newHarness(t, w2)
	h2.tools.listed = []batchv1.Job{*toolJob("a", "a"), *toolJob("b", "b")}
	h2.now = t0.Add(45*time.Minute + grace)
	h2.reconcile(t)
	if pv := h2.pv(t); pv.Status.Phase != v1alpha1.VerificationInfra ||
		pv.Status.Reason != v1alpha1.VerificationReasonToolPodsSaturated {
		t.Errorf("status = %+v, want Infra past the timeout", pv.Status)
	}
}

// TestRecheckDropsUnjudged: a verification whose trees, intent or pull
// request no longer hold is Stale, before any pod.
func TestRecheckDropsUnjudged(t *testing.T) {
	for name, mutate := range map[string]func(*world){
		"head tree recreated":      func(w *world) { w.head.UID = "new-uid" },
		"base tree moved":          func(w *world) { w.base.Status.ResolvedSHA = headSHA },
		"base tree not ready":      func(w *world) { w.base.Status.Conditions = nil },
		"head tree another intent": func(w *world) { w.head.OwnerReferences[0].UID = "x" },
		"head tree no artifact":    func(w *world) { w.head.Status.Artifact = nil },
		"tree of another repo":     func(w *world) { w.base.Spec.URL = "https://github.com/octo/other" },
		"intent replaced":          func(w *world) { w.intent.UID = "new-intent" },
		"pull request moved":       func(w *world) { w.intent.Status.PullRequests[0].HeadSHA = strings.Repeat("f", 40) },
		"pull request closed":      func(w *world) { w.intent.Status.PullRequests[0].State = "closed" },
		"outcomes revised": func(w *world) {
			w.intent.Status.Acceptance.Outcomes[0].Outcome = "another"
			w.intent.Status.Acceptance.OutcomesDigest = "sha256:" + strings.Repeat("0", 64)
		},
		"surface removed":       func(w *world) { w.project.Spec.Surfaces = nil },
		"repository renamed":    func(w *world) { w.project.Spec.Repositories[0].Name = "other" },
		"image moved":           func(w *world) { w.build.Status.RunnerImage.Image = strings.ReplaceAll(image, "c", "d") },
		"build repo not owned":  func(w *world) { w.build.OwnerReferences[0].UID = "x" },
		"image rejected":        func(w *world) { w.build.Status.RunnerImage.Rejected = "Unsigned" },
		"profile now different": func(w *world) { w.pv.Spec.Workspace.NetworkProfile = "go-public" },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			mutate(w)
			h := newHarness(t, w)
			h.reconcile(t)
			pv := h.pv(t)
			if len(h.tools.created) != 0 || (pv.Status.Phase != v1alpha1.VerificationStale &&
				pv.Status.Phase != v1alpha1.VerificationInfra) {
				t.Fatalf("created %d, status %+v; want it dropped unjudged", len(h.tools.created), pv.Status)
			}
		})
	}
}

// TestRecheckWhileRunning: a running verification whose tree moved is
// dropped and its running tool pod deleted at once.
func TestRecheckWhileRunning(t *testing.T) {
	w := newWorld(t)
	h := newHarness(t, w)
	h.reconcile(t)
	var head v1alpha1.Repository
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: w.head.Name}, &head); err != nil {
		t.Fatal(err)
	}
	head.Status.ResolvedSHA = baseSHA
	if err := h.c.Status().Update(context.Background(), &head); err != nil {
		t.Fatal(err)
	}
	h.reconcile(t)
	if pv := h.pv(t); pv.Status.Phase != v1alpha1.VerificationStale {
		t.Fatalf("phase = %s, want Stale", pv.Status.Phase)
	}
	if !slices.Contains(h.tools.deleted, pvName) {
		t.Error("the running tool pod was not deleted")
	}
}

// TestToolsStatusOnlyAfterReady: status.tools is written only once tools
// and both sandboxes are Ready, with the pod's address.
func TestToolsStatusOnlyAfterReady(t *testing.T) {
	w := newWorld(t)
	h := newHarness(t, w)
	h.reconcile(t)
	notReady := map[string]func(*corev1.Pod){
		"no address":       func(p *corev1.Pod) { p.Status.PodIP = "" },
		"tools not ready":  func(p *corev1.Pod) { p.Status.ContainerStatuses[0].Ready = false },
		"base not running": func(p *corev1.Pod) { p.Status.InitContainerStatuses[2].State = corev1.ContainerState{} },
		"head not ready":   func(p *corev1.Pod) { p.Status.InitContainerStatuses[1].Ready = false },
		"pod not ready":    func(p *corev1.Pod) { p.Status.Conditions = nil },
	}
	for name, mutate := range notReady {
		pod := readyPod()
		mutate(pod)
		h.tools.pod = pod
		h.reconcile(t)
		if pv := h.pv(t); pv.Status.Tools != nil {
			t.Fatalf("%s: tools = %+v, want none", name, pv.Status.Tools)
		}
	}
	h.tools.pod = readyPod()
	h.reconcile(t)
	if tl := h.pv(t).Status.Tools; tl == nil || tl.PodIP != "10.42.0.17" || tl.Port != jobs.ToolsPort || !tl.Ready {
		t.Fatalf("tools = %+v, want the pod's address", tl)
	}
}

// running is a harness whose verification runs with its tool pod Ready.
func running(t *testing.T, w *world) *harness {
	t.Helper()
	h := newHarness(t, w)
	h.reconcile(t)
	h.tools.pod = readyPod()
	h.reconcile(t)
	return h
}

// TestCollect: a finished round's verified ledger lands on status: the
// builds, the head's replay only on replay, the base's runs tagged base
// with base required, the head build's part, and <pv>-ledger.
func TestCollect(t *testing.T) {
	w := newWorld(t)
	h := running(t, w)
	log, msg := toolsLog(t, ledgerOpts{headBuild: v1alpha1.BuildBuilt, baseBuild: v1alpha1.BuildBuilt})
	finish(h.tools.job, h.now)
	h.tools.pod, h.tools.log = terminated(readyPod(), msg), log
	h.reconcile(t)
	pv := h.pv(t)
	if pv.Status.Phase != v1alpha1.VerificationObserved || pv.Status.Reason != v1alpha1.VerificationReasonOK {
		t.Fatalf("status = %s %s, want Observed", pv.Status.Phase, pv.Status.Reason)
	}
	checkCollected(t, pv)
	checkLedgerConfigMap(t, h)
	// The finished pod lingers for the isolation probe, then goes.
	if slices.Contains(h.tools.deleted, pvName) {
		t.Fatal("the finished tool pod was deleted at settle; the probe's ledger row needs it")
	}
	h.now = h.now.Add(linger)
	h.reconcile(t)
	h.reconcile(t) // idempotent
	if !slices.Contains(h.tools.deleted, pvName) {
		t.Error("the tool pod was not torn down after the linger")
	}
}

// checkCollected: pv's status holds the ledger's builds, the head's replay,
// the base's run tagged base with base required, and the head build part.
func checkCollected(t *testing.T, pv *v1alpha1.PreviewVerification) {
	t.Helper()
	checkLedgerBuilds(t, pv.Status.Ledger)
	checkRuns(t, pv)
}

// checkLedgerBuilds: the index holds every entry and the two builds.
func checkLedgerBuilds(t *testing.T, l *v1alpha1.LedgerSummary) {
	t.Helper()
	if l == nil || l.Count != 6 || len(l.Entries) != 6 || l.ConfigMap != pvName+"-ledger" || len(l.Builds) != 2 ||
		l.Builds[0] != (v1alpha1.LedgerBuild{Side: v1alpha1.TreeHead, Result: v1alpha1.BuildBuilt, Seq: 1,
			DurationMs: 4200}) || l.Builds[1].Side != v1alpha1.TreeBase || l.Builds[1].Seq != 2 {
		t.Fatalf("ledger = %+v", l)
	}
}

// checkRuns: the head's replay, the base's run, the head build part.
func checkRuns(t *testing.T, pv *v1alpha1.PreviewVerification) {
	t.Helper()
	if len(pv.Status.Replay) != 1 || pv.Status.Replay[0].Side() != v1alpha1.TreeHead ||
		pv.Status.Replay[0].Result != v1alpha1.CheckRunPass {
		t.Errorf("replay = %+v, want the head's one pass", pv.Status.Replay)
	}
	b := pv.Status.Base
	if b == nil || !b.Required || b.Unbuildable || len(b.Runs) != 1 || b.Runs[0].Tree != v1alpha1.TreeBase ||
		!b.Runs[0].RedByAssertion() {
		t.Errorf("base = %+v, want the base run red by assertion, base required", b)
	}
	env := pv.Status.Environment
	if len(env) != 1 || env[0].Role != v1alpha1.PreviewPartRole("build") || env[0].LastTermination == nil ||
		env[0].LastTermination.Reason != "Completed" || env[0].LastTermination.ExitCode != 0 {
		t.Errorf("environment = %+v, want the head build part, built", env)
	}
	if pv.Status.TesterImageID == "" {
		t.Error("no tools image ID")
	}
}

// checkLedgerConfigMap: <pv>-ledger holds the ledger, owned by the
// verification, labelled with its Intent.
func checkLedgerConfigMap(t *testing.T, h *harness) {
	t.Helper()
	var cm corev1.ConfigMap
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: pvName + "-ledger"},
		&cm); err != nil {
		t.Fatalf("ledger ConfigMap: %v", err)
	}
	if owner := metav1.GetControllerOf(&cm); owner == nil || owner.UID != pvUID ||
		cm.Labels[v1alpha1.LabelIntent] != intentName || cm.Data[toolledger.Key] == "" {
		t.Errorf("ledger ConfigMap meta %+v / owner %v", cm.Labels, owner)
	}
}

// TestCollectBuilds: a head that did not build is Infra tools-build-failed
// (never a fail); a base whose build failed is unbuildable, one maul could
// not build, that timed out or that was killed (a "failed" entry whose exit
// is not a normal one) is not, and its runs carry no cause (a base run is
// then unsure, never n/a: review VC-1).
func TestCollectBuilds(t *testing.T) {
	signal, shellSignal := -1, 139
	for _, tc := range []struct {
		head, base  v1alpha1.BuildResult
		baseExit    *int
		phase       v1alpha1.PreviewVerificationPhase
		reason      v1alpha1.PreviewVerificationReason
		unbuildable bool
	}{
		{v1alpha1.BuildFailed, v1alpha1.BuildBuilt, nil, v1alpha1.VerificationInfra,
			v1alpha1.VerificationReasonToolsBuildFailed, false},
		{v1alpha1.BuildTimeout, v1alpha1.BuildBuilt, nil, v1alpha1.VerificationInfra,
			v1alpha1.VerificationReasonToolsBuildFailed, false},
		{v1alpha1.BuildError, v1alpha1.BuildBuilt, nil, v1alpha1.VerificationInfra,
			v1alpha1.VerificationReasonToolsBuildFailed, false},
		{v1alpha1.BuildBuilt, v1alpha1.BuildFailed, nil, v1alpha1.VerificationObserved,
			v1alpha1.VerificationReasonOK, true},
		{v1alpha1.BuildBuilt, v1alpha1.BuildFailed, &signal, v1alpha1.VerificationObserved,
			v1alpha1.VerificationReasonOK, false},
		{v1alpha1.BuildBuilt, v1alpha1.BuildFailed, &shellSignal, v1alpha1.VerificationObserved,
			v1alpha1.VerificationReasonOK, false},
		{v1alpha1.BuildBuilt, v1alpha1.BuildTimeout, nil, v1alpha1.VerificationObserved,
			v1alpha1.VerificationReasonOK, false},
		{v1alpha1.BuildBuilt, v1alpha1.BuildError, nil, v1alpha1.VerificationObserved,
			v1alpha1.VerificationReasonOK, false},
	} {
		name := string(tc.head) + "-" + string(tc.base)
		if tc.baseExit != nil {
			name += fmt.Sprintf("-exit%d", *tc.baseExit)
		}
		t.Run(name, func(t *testing.T) {
			h := running(t, newWorld(t))
			log, msg := toolsLog(t, ledgerOpts{headBuild: tc.head, baseBuild: tc.base, baseExit: tc.baseExit})
			finish(h.tools.job, h.now)
			h.tools.pod, h.tools.log = terminated(readyPod(), msg), log
			h.reconcile(t)
			pv := h.pv(t)
			if pv.Status.Phase != tc.phase || pv.Status.Reason != tc.reason {
				t.Fatalf("status = %s %s, want %s %s", pv.Status.Phase, pv.Status.Reason, tc.phase, tc.reason)
			}
			if pv.Status.Base == nil || pv.Status.Base.Unbuildable != tc.unbuildable {
				t.Errorf("base = %+v, want unbuildable %v", pv.Status.Base, tc.unbuildable)
			}
			// The relay's run on a base that did not build has no run
			// token, so it is no base run: n/a only through Unbuildable.
			if b := pv.Status.Base; b != nil && tc.base != v1alpha1.BuildBuilt && len(b.Runs) != 0 {
				t.Errorf("base runs = %+v, want none on a base that did not build", b.Runs)
			}
			if tc.head != v1alpha1.BuildBuilt {
				f := pv.Status.Environment
				if len(f) != 1 || f[0].Role != v1alpha1.PreviewPartRole("build") || (f[0].LastTermination != nil &&
					f[0].LastTermination.ExitCode == 0 && f[0].LastTermination.Reason == "Completed") {
					t.Errorf("environment = %+v, want a head build part that did not build", f)
				}
			}
		})
	}
}

// TestCollectRefusesTheLedger: a log that does not hold its summary, one
// that holds the tools key (U23), and a full round no evaluator called are
// Infra, never a verdict, and keep no ledger.
func TestCollectRefusesTheLedger(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(h *harness, log []byte, msg string) ([]byte, string)
		opts   ledgerOpts
		reason v1alpha1.PreviewVerificationReason
	}{
		"tampered log": {mutate: func(_ *harness, log []byte, msg string) ([]byte, string) {
			return []byte(strings.Replace(string(log), "usage", "USAGE", 1)), msg
		}, reason: v1alpha1.VerificationReasonLedgerMismatch},
		"forged termination message": {mutate: func(_ *harness, log []byte, _ string) ([]byte, string) {
			return log, `{"v":3,"count":1,"sha256":"` + strings.Repeat("0", 64) + `"}`
		}, reason: v1alpha1.VerificationReasonLedgerMismatch},
		"the key in the ledger": {mutate: func(h *harness, _ []byte, _ string) ([]byte, string) {
			o := ledgerOpts{headBuild: v1alpha1.BuildBuilt, baseBuild: v1alpha1.BuildBuilt,
				extraOutput: h.tools.created[0].Key}
			return toolsLog(t, o)
		}, reason: v1alpha1.VerificationReasonLedgerMismatch},
		"no evaluator": {opts: ledgerOpts{headBuild: v1alpha1.BuildBuilt, baseBuild: v1alpha1.BuildBuilt,
			noEvaluator: true}, reason: v1alpha1.VerificationReasonEvaluatorNotRun},
	} {
		t.Run(name, func(t *testing.T) {
			h := running(t, newWorld(t))
			opts := tc.opts
			if opts.headBuild == "" {
				opts = ledgerOpts{headBuild: v1alpha1.BuildBuilt, baseBuild: v1alpha1.BuildBuilt}
			}
			log, msg := toolsLog(t, opts)
			if tc.mutate != nil {
				log, msg = tc.mutate(h, log, msg)
			}
			finish(h.tools.job, h.now)
			h.tools.pod, h.tools.log = terminated(readyPod(), msg), log
			h.reconcile(t)
			pv := h.pv(t)
			if pv.Status.Phase != v1alpha1.VerificationInfra || pv.Status.Reason != tc.reason {
				t.Fatalf("status = %s %s, want Infra %s", pv.Status.Phase, pv.Status.Reason, tc.reason)
			}
			if tc.reason == v1alpha1.VerificationReasonLedgerMismatch &&
				(pv.Status.Ledger != nil || pv.Status.Base != nil || pv.Status.Replay != nil) {
				t.Errorf("a refused ledger kept on status: %+v", pv.Status)
			}
		})
	}
}

// TestCollectGuardsTheImageID: an image ID longer than the status field's
// 512-character bound is left off status, as the slot host leaves it, so
// the settle's status write is never refused for it; the round's verdict
// stands.
func TestCollectGuardsTheImageID(t *testing.T) {
	long := "ghcr.io/devthenet-labs/maul/preview-tools@sha256:" + strings.Repeat("1", 512)
	for _, tc := range []struct {
		name, imageID, want string
	}{
		{"within the bound", "ghcr.io/devthenet-labs/maul/preview-tools@sha256:1111",
			"ghcr.io/devthenet-labs/maul/preview-tools@sha256:1111"},
		{"exactly 512", strings.Repeat("a", 512), strings.Repeat("a", 512)},
		{"over the bound", long, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := running(t, newWorld(t))
			log, msg := toolsLog(t, ledgerOpts{headBuild: v1alpha1.BuildBuilt, baseBuild: v1alpha1.BuildBuilt})
			finish(h.tools.job, h.now)
			pod := terminated(readyPod(), msg)
			pod.Status.ContainerStatuses[0].ImageID = tc.imageID
			h.tools.pod, h.tools.log = pod, log
			h.reconcile(t)
			pv := h.pv(t)
			if pv.Status.Phase != v1alpha1.VerificationObserved || pv.Status.Reason != v1alpha1.VerificationReasonOK {
				t.Fatalf("status = %s %s, want Observed", pv.Status.Phase, pv.Status.Reason)
			}
			if pv.Status.TesterImageID != tc.want {
				t.Errorf("image ID = %q (%d characters), want %q", pv.Status.TesterImageID,
					len(pv.Status.TesterImageID), tc.want)
			}
		})
	}
}

// TestCollectReplayCutShort (review VC-3): a replay round's relay that
// finished at the end of its replay is Observed; one it finished at its
// wall clock, or because its container was told to stop, may have left
// saved checks unreplayed, so it is Infra replay-incomplete, never judged.
func TestCollectReplayCutShort(t *testing.T) {
	for _, tc := range []struct {
		finish string
		phase  v1alpha1.PreviewVerificationPhase
		reason v1alpha1.PreviewVerificationReason
	}{
		{previewtools.FinishReplayOnly, v1alpha1.VerificationObserved, v1alpha1.VerificationReasonOK},
		{previewtools.FinishDeadline, v1alpha1.VerificationInfra, v1alpha1.VerificationReasonReplayIncomplete},
		{previewtools.FinishStopped, v1alpha1.VerificationInfra, v1alpha1.VerificationReasonReplayIncomplete},
	} {
		t.Run(tc.finish, func(t *testing.T) {
			w := newWorld(t)
			// A replay round replays the saved checks of the Intent's store.
			c, err := checks.Parse([]byte(checkBody))
			if err != nil {
				t.Fatal(err)
			}
			store := "sha256:" + strings.Repeat("c", 64)
			w.pv.Spec.Rung, w.pv.Spec.ChecksDigest = v1alpha1.RungReplay, store
			w.pv.Spec.Checks = []v1alpha1.SavedCheck{{ID: c.ID, Outcome: c.Outcome, Form: v1alpha1.CheckForm(c.Form),
				Body: string(checks.Canonical(c)), Digest: checks.Digest(c)}}
			w.intent.Status.Acceptance.Checks = &v1alpha1.CheckStoreRef{ConfigMap: intentName + "-checks-1",
				Digest: store, Count: 1}
			h := running(t, w)
			log, msg := toolsLog(t, ledgerOpts{headBuild: v1alpha1.BuildBuilt, baseBuild: v1alpha1.BuildBuilt,
				relayFinish: tc.finish})
			finish(h.tools.job, h.now)
			h.tools.pod, h.tools.log = terminated(readyPod(), msg), log
			h.reconcile(t)
			pv := h.pv(t)
			if pv.Status.Phase != tc.phase || pv.Status.Reason != tc.reason {
				t.Fatalf("status = %s %s, want %s %s", pv.Status.Phase, pv.Status.Reason, tc.phase, tc.reason)
			}
		})
	}
}

// TestCollectKeptCopyFailed (VC-2 follow-up): a relay that finished
// because a sandbox could not keep a built tree is Infra kept-copy-failed
// on either rung, never judged nor read as an evaluator that did not run.
func TestCollectKeptCopyFailed(t *testing.T) {
	for _, rung := range []v1alpha1.VerificationRung{v1alpha1.RungFull, v1alpha1.RungReplay} {
		t.Run(string(rung), func(t *testing.T) {
			w := newWorld(t)
			if rung == v1alpha1.RungReplay {
				c, err := checks.Parse([]byte(checkBody))
				if err != nil {
					t.Fatal(err)
				}
				store := "sha256:" + strings.Repeat("c", 64)
				w.pv.Spec.Rung, w.pv.Spec.ChecksDigest = v1alpha1.RungReplay, store
				w.pv.Spec.Checks = []v1alpha1.SavedCheck{{ID: c.ID, Outcome: c.Outcome,
					Form: v1alpha1.CheckForm(c.Form), Body: string(checks.Canonical(c)), Digest: checks.Digest(c)}}
				w.intent.Status.Acceptance.Checks = &v1alpha1.CheckStoreRef{ConfigMap: intentName + "-checks-1",
					Digest: store, Count: 1}
			}
			h := running(t, w)
			log, msg := toolsLog(t, ledgerOpts{headBuild: v1alpha1.BuildBuilt, baseBuild: v1alpha1.BuildBuilt,
				relayFinish: previewtools.FinishKeptCopyFailed})
			finish(h.tools.job, h.now)
			h.tools.pod, h.tools.log = terminated(readyPod(), msg), log
			h.reconcile(t)
			pv := h.pv(t)
			if pv.Status.Phase != v1alpha1.VerificationInfra ||
				pv.Status.Reason != v1alpha1.VerificationReasonKeptCopyFailed {
				t.Fatalf("status = %s %s, want Infra kept-copy-failed", pv.Status.Phase, pv.Status.Reason)
			}
		})
	}
}

// TestSandboxUnenforced: prepare's exit 78 is sandbox-unenforced and trips
// the breaker, so no later launch copies a repository image.
func TestSandboxUnenforced(t *testing.T) {
	h := running(t, newWorld(t))
	pod := readyPod()
	pod.Status.InitContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		Reason: "Error", ExitCode: jobs.ExitSandboxUnenforced}}
	pod.Status.Phase = corev1.PodFailed
	h.tools.pod = pod
	h.reconcile(t)
	pv := h.pv(t)
	if pv.Status.Phase != v1alpha1.VerificationInfra || pv.Status.Reason != v1alpha1.VerificationReasonSandboxUnenforced {
		t.Fatalf("status = %s %s, want Infra sandbox-unenforced", pv.Status.Phase, pv.Status.Reason)
	}
	if !h.r.Images.Breaker.Tripped() {
		t.Error("the sandbox breaker did not trip")
	}
	if !slices.Contains(h.tools.deleted, pvName) {
		t.Error("the refused tool pod was not deleted")
	}
}

// TestStuckToolPods: a pod that never comes, is never scheduled, cannot
// pull, crash-loops, is evicted or outlives its deadline is Infra.
func TestStuckToolPods(t *testing.T) {
	waiting := func(reason, msg string) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			p.Status.InitContainerStatuses[1].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: reason, Message: msg}}
		}
	}
	for _, tc := range []struct {
		name   string
		pod    func(*corev1.Pod)
		noPod  bool
		after  time.Duration
		reason v1alpha1.PreviewVerificationReason
	}{
		{name: "no pod", noPod: true, after: grace, reason: v1alpha1.VerificationReasonToolsNotReady},
		{name: "unschedulable", after: grace, reason: v1alpha1.VerificationReasonToolsUnscheduled,
			pod: func(p *corev1.Pod) {
				p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled,
					Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable}}
			}},
		{name: "image not found", after: runnerguard.PullGrace, reason: v1alpha1.VerificationReasonToolsImagePull,
			pod: waiting("ErrImagePull", "manifest unknown")},
		{name: "image back-off", after: grace, reason: v1alpha1.VerificationReasonToolsImagePull,
			pod: waiting("ImagePullBackOff", "")},
		{name: "crash loop", after: grace, reason: v1alpha1.VerificationReasonToolsCrashLoop,
			pod: waiting("CrashLoopBackOff", "")},
		{name: "evicted", reason: v1alpha1.VerificationReasonToolsEvicted, pod: func(p *corev1.Pod) {
			p.Status.Phase, p.Status.Reason = corev1.PodFailed, "Evicted"
		}},
		{name: "deadline", after: 45*time.Minute + deadlineMargin + grace,
			reason: v1alpha1.VerificationReasonToolsDeadline, pod: func(*corev1.Pod) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := running(t, newWorld(t))
			if tc.noPod {
				h.tools.pod = nil
			} else {
				pod := readyPod()
				pod.Status.ContainerStatuses[0].Ready = false
				tc.pod(pod)
				h.tools.pod = pod
			}
			h.now = h.now.Add(tc.after)
			h.reconcile(t)
			pv := h.pv(t)
			if pv.Status.Phase != v1alpha1.VerificationInfra || pv.Status.Reason != tc.reason {
				t.Fatalf("status = %s %s, want Infra %s", pv.Status.Phase, pv.Status.Reason, tc.reason)
			}
		})
	}
}

// TestCollectWaitsForTheLog: a log not yet readable is waited for within
// the grace, then Infra; a finished Job with no pod is tools-deadline.
func TestToolsNeverRan(t *testing.T) {
	h := running(t, newWorld(t))
	pod := readyPod()
	pod.Status.InitContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		Reason: "Error", ExitCode: 1}}
	pod.Status.ContainerStatuses[0].Ready = false
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
		Reason: "PodInitializing"}}
	pod.Status.Phase = corev1.PodFailed
	finish(h.tools.job, h.now)
	h.tools.pod, h.tools.logErr = pod, errors.New("container tools is waiting to start")
	h.reconcile(t)
	if pv := h.pv(t); pv.Status.Phase != v1alpha1.VerificationInfra ||
		pv.Status.Reason != v1alpha1.VerificationReasonToolsNotReady {
		t.Fatalf("status = %s %s, want Infra tools-not-ready at once", pv.Status.Phase, pv.Status.Reason)
	}
	if h.r.Images.Breaker.Tripped() {
		t.Error("a prepare failure other than exit 78 tripped the breaker")
	}
}

// TestNoSummary: a log with no summary is tools-deadline when the pod ran
// out of time, deadline-no-summary otherwise; Infra either way.
func TestNoSummary(t *testing.T) {
	for _, deadline := range []bool{true, false} {
		h := running(t, newWorld(t))
		log, msg := toolsLog(t, ledgerOpts{headBuild: v1alpha1.BuildBuilt, baseBuild: v1alpha1.BuildBuilt})
		lines := strings.SplitAfter(strings.TrimSuffix(string(log), "\n"), "\n")
		log = []byte(strings.Join(lines[:len(lines)-1], "")) // the summary line cut off
		pod := terminated(readyPod(), msg)
		want := v1alpha1.VerificationReasonDeadlineNoSummary
		if deadline {
			pod.Status.Reason = "DeadlineExceeded"
			want = v1alpha1.VerificationReasonToolsDeadline
		}
		finish(h.tools.job, h.now)
		h.tools.pod, h.tools.log = pod, log
		h.reconcile(t)
		if pv := h.pv(t); pv.Status.Phase != v1alpha1.VerificationInfra || pv.Status.Reason != want {
			t.Errorf("deadline %v: status = %s %s, want Infra %s", deadline, pv.Status.Phase, pv.Status.Reason, want)
		}
	}
}

func TestCollectWaitsForTheLog(t *testing.T) {
	h := running(t, newWorld(t))
	finish(h.tools.job, h.now)
	h.tools.pod, h.tools.logErr = terminated(readyPod(), ""), errors.New("the kubelet is busy")
	h.reconcile(t)
	if pv := h.pv(t); pv.Status.Phase != v1alpha1.VerificationRunning {
		t.Fatalf("phase = %s, want a wait", pv.Status.Phase)
	}
	h.now = h.now.Add(grace)
	h.reconcile(t)
	if pv := h.pv(t); pv.Status.Reason != v1alpha1.VerificationReasonLedgerMismatch {
		t.Fatalf("reason = %s, want ledger-mismatch past the grace", pv.Status.Reason)
	}
	h2 := running(t, newWorld(t))
	finish(h2.tools.job, h2.now)
	h2.tools.pod = nil
	h2.reconcile(t)
	if pv := h2.pv(t); pv.Status.Reason != v1alpha1.VerificationReasonToolsDeadline {
		t.Fatalf("reason = %s, want tools-deadline", pv.Status.Reason)
	}
}

// TestJobName: a verification's name when it is a short DNS label, else a
// hash, either way a Secret name that is a DNS label too.
func TestJobName(t *testing.T) {
	for _, name := range []string{pvName, strings.Repeat("x", 60) + "-a1", "a.b-v1-a1"} {
		pv := &v1alpha1.PreviewVerification{ObjectMeta: metav1.ObjectMeta{Name: name}}
		got := JobName(pv)
		if len(got) > jobs.ToolPodNameMax || strings.ContainsAny(got, ".") {
			t.Errorf("JobName(%q) = %q", name, got)
		}
		if name == pvName && got != pvName {
			t.Errorf("JobName(%q) = %q, want the verification's own name", name, got)
		}
	}
}

// TestReadKey: the evaluator's key is the verification's own Secret's.
func TestReadKey(t *testing.T) {
	w := newWorld(t)
	key := strings.Repeat("ef", 32)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: pvName + jobs.ToolsSecretSuffix, Namespace: agentsNS,
		Labels: map[string]string{v1alpha1.LabelPreviewVerification: string(pvUID)}},
		Data: map[string][]byte{jobs.ToolsKeyFile: []byte(key)}}
	h := newHarness(t, w)
	if _, err := ReadKey(context.Background(), h.c, agentsNS, w.pv); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("no Secret: %v, want ErrKeyNotFound", err)
	}
	if err := h.c.Create(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadKey(context.Background(), h.c, agentsNS, w.pv); err != nil || got != key {
		t.Errorf("ReadKey = %q, %v", got, err)
	}
	other := w.pv.DeepCopy()
	other.UID = "other"
	if _, err := ReadKey(context.Background(), h.c, agentsNS, other); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("another verification's Secret: %v, want ErrKeyNotFound", err)
	}
}

// TestRelayEnv: the relay's environment is the sandbox's as jobs renders
// it, less PATH and the shim's own names, plus the image's Go directories
// a profile does not set.
func TestRelayEnv(t *testing.T) {
	rendered := []corev1.EnvVar{{Name: "HOME", Value: "/work/home"}, {Name: "PATH", Value: "/bin"},
		{Name: "GOPROXY", Value: "http://proxy:8091"}, {Name: "GOMODCACHE", Value: "/tmp/go/pkg/mod"},
		{Name: "HTTPS_PROXY", Value: "http://proxy:8091"}, {Name: "GOCACHE", Value: "/work/cache/go-build"}}
	got := relayEnv(rendered, map[string]string{"GOMODCACHE": "/opt/go/pkg/mod", "GOPATH": "/go",
		"GOROOT": "/usr/local/go"})
	want := []toolexec.EnvVar{{Name: "GOPROXY", Value: "http://proxy:8091"},
		{Name: "GOMODCACHE", Value: "/tmp/go/pkg/mod"}, {Name: "HTTPS_PROXY", Value: "http://proxy:8091"},
		{Name: "GOPATH", Value: "/go"}}
	if !slices.Equal(got, want) {
		t.Errorf("relayEnv = %v, want %v", got, want)
	}
}

// TestStartAttributesTheToolPodToTheOrg (S2-B1): under --attribution-mode
// attribute the tool pod is the org's, whoever approved the plan (it is no
// run's, holds no token and spends nothing), so the chart's Job policy
// admits it; off it carries no attribution at all.
func TestStartAttributesTheToolPodToTheOrg(t *testing.T) {
	for _, tc := range []struct {
		mode attribution.Mode
		want string
	}{{attribution.ModeOff, ""}, {"", ""}, {attribution.ModeAttribute, attribution.ValueOrg}} {
		w := newWorld(t)
		w.intent.Status.Approval = &v1alpha1.IntentApproval{By: "octocat", ByID: 42}
		h := newHarness(t, w)
		h.r.Settings.Attribution = tc.mode
		h.reconcile(t)
		if len(h.tools.created) != 1 {
			t.Fatalf("mode %q: created %d tool pods, want 1", tc.mode, len(h.tools.created))
		}
		if got := h.tools.created[0].AttributedTo; got != tc.want {
			t.Errorf("mode %q: AttributedTo %q, want %q", tc.mode, got, tc.want)
		}
	}
}

// TestStartReportsTheAPIRefusal (S2-B1): the API server refusing the tool
// Job at admission is Infra tools-refused, never tools-unscheduled (nothing
// was scheduled), and its message is logged, bounded; a quota's refusal is
// quota-refused. Either is Infra: never a verdict.
func TestStartReportsTheAPIRefusal(t *testing.T) {
	gr := batchv1.Resource("jobs")
	policy := "ValidatingAdmissionPolicy 'maul-attribution-jobs' with binding 'maul-attribution-jobs' " +
		"denied request: job " + pvName + " is launched by a maul controller and must carry the " +
		"maul.run/attributed-to annotation" + strings.Repeat(" and more", 200)
	for _, tc := range []struct {
		name   string
		err    error
		reason v1alpha1.PreviewVerificationReason
		logged string
	}{
		{"admission policy", kerrors.NewForbidden(gr, pvName, errors.New(policy)),
			v1alpha1.VerificationReasonToolsRefused, "denied request: job " + pvName},
		{"invalid", kerrors.NewInvalid(batchv1.SchemeGroupVersion.WithKind("Job").GroupKind(), pvName, nil),
			v1alpha1.VerificationReasonToolsRefused, "is invalid"},
		{"quota", kerrors.NewForbidden(gr, pvName, errors.New("exceeded quota: agents, requested: cpu=2")),
			v1alpha1.VerificationReasonQuotaRefused, "exceeded quota"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, newWorld(t))
			var log bytes.Buffer
			h.r.Log = slog.New(slog.NewTextHandler(&log, nil))
			h.tools.createFn = func(jobs.ToolPodSpec) error {
				return fmt.Errorf("jobs: create job %s: %w", pvName, tc.err)
			}
			h.reconcile(t)
			pv := h.pv(t)
			if pv.Status.Phase != v1alpha1.VerificationInfra || pv.Status.Reason != tc.reason {
				t.Fatalf("status = %s %s, want Infra %s", pv.Status.Phase, pv.Status.Reason, tc.reason)
			}
			out := log.String()
			if !strings.Contains(out, tc.logged) || !strings.Contains(out, "reason="+string(tc.reason)) {
				t.Errorf("log %q lacks the refusal %q and its reason", out, tc.logged)
			}
			if len(out) > refusalLogMax+1024 {
				t.Errorf("logged %d bytes, want the API message bounded to %d", len(out), refusalLogMax)
			}
		})
	}
}
