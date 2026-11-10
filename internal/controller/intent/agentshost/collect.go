// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentshost

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/previewtools"
	"github.com/devthenet-labs/maul/internal/toolledger"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// ledgerSuffix names the verification's ledger ConfigMap: <pv>-ledger.
const ledgerSuffix = "-ledger"

// maxBuilds is the builds a workspace ledger may record (status.ledger.
// builds' bound); the relay records one of each tree.
const maxBuilds = v1alpha1.MaxLedgerBuilds

// builds is what a workspace ledger's build entries show.
type builds struct {
	list []v1alpha1.LedgerBuild
	// head is the head build's part (previewtools.BuildPart); a ledger with
	// no head build has one that did not build.
	head v1alpha1.PartFact
	// headBuilt and baseFailed: the head built; the base's build ran to its
	// own end and refused the tree (the relay's failed, with a normal exit:
	// previewtools.RefusalExit), the only base that is unbuildable. A base that timed
	// out, was killed, or that maul could not build, gave no evidence either
	// way: its checks then have no base run, which is unsure, never n/a
	// (review VC-1).
	headBuilt, baseFailed bool
	// ok is false for a ledger whose builds are not one of each tree.
	ok bool
}

// collect settles a finished tool pod's attempt from the tools container's
// log, verified against its termination message. A ledger that holds and
// does not carry the tools key is indexed onto status with its builds, its
// head's runs and confirmed checks, the base's runs and the head build's
// part, and stored whole in <pv>-ledger. A head that did not build is
// Infra tools-build-failed; a tree built but not kept for its check runs is
// Infra kept-copy-failed; a full round no evaluator called is Infra
// evaluator-not-run; a replay round cut short is Infra replay-incomplete; a
// ledger that does not hold is Infra, never a verdict (collectedAs).
func (r *Reconciler) collect(ctx context.Context, pv *v1alpha1.PreviewVerification, pod *corev1.Pod,
	log []byte) error {
	msg := terminationMessage(pod)
	out := toolledger.Parse(log, msg)
	if out.Reason == v1alpha1.VerificationReasonDeadlineNoSummary && pod.Status.Reason == "DeadlineExceeded" {
		out.Reason = v1alpha1.VerificationReasonToolsDeadline
	}
	if out.Reason == "" {
		key, uid, found, err := r.Tools.Key(ctx, JobName(pv))
		switch {
		case err != nil:
			return err
		case !found || uid != string(pv.UID) || !toolwire.IsKey(key) ||
			bytes.Contains(log, []byte(key)) || strings.Contains(msg, key):
			// The key must never be on record (U23); a key that cannot be
			// read back cannot be ruled out.
			out.Reason = v1alpha1.VerificationReasonLedgerMismatch
		}
	}
	var b builds
	if out.Reason == "" {
		if b = workspaceBuilds(out.Ledger.Entries); !b.ok {
			out.Reason = v1alpha1.VerificationReasonLedgerMismatch
		}
	}
	phase, reason := collectedAs(pv, out, b)
	if out.Reason == "" {
		if err := r.storeLedger(ctx, pv, out, b); err != nil {
			return err
		}
	}
	pv.Status.TesterImageID = imageIDOf(pod, toolsContainer)
	return r.settle(ctx, pv, phase, reason)
}

// collectedAs is the phase and reason a collected attempt settles at: Infra
// for a ledger that does not hold (out.Reason), a head that did not build, a
// tree its sandbox could not keep for its check runs (the relay's
// FinishKeptCopyFailed), a full round no evaluator called, and a replay
// round its relay finished
// before its replay's end (review VC-3: a saved check may never have run);
// Observed ok otherwise.
func collectedAs(pv *v1alpha1.PreviewVerification, out toolledger.Output, b builds) (
	v1alpha1.PreviewVerificationPhase, v1alpha1.PreviewVerificationReason) {
	switch {
	case out.Reason != "":
		return v1alpha1.VerificationInfra, out.Reason
	case !b.headBuilt:
		return v1alpha1.VerificationInfra, v1alpha1.VerificationReasonToolsBuildFailed
	case previewtools.KeptCopyFailed(out.Ledger.Entries):
		return v1alpha1.VerificationInfra, v1alpha1.VerificationReasonKeptCopyFailed
	case pv.Spec.Rung == v1alpha1.RungFull && !evaluatorCalled(out.Ledger.Entries):
		if pv.Status.Tools == nil {
			return v1alpha1.VerificationInfra, v1alpha1.VerificationReasonToolsNotReady
		}
		return v1alpha1.VerificationInfra, v1alpha1.VerificationReasonEvaluatorNotRun
	case pv.Spec.Rung == v1alpha1.RungReplay && !previewtools.ReplayFinished(out.Ledger.Entries):
		return v1alpha1.VerificationInfra, v1alpha1.VerificationReasonReplayIncomplete
	}
	return v1alpha1.VerificationObserved, v1alpha1.VerificationReasonOK
}

// workspaceBuilds reads the relay's build entries: at most one of each
// tree, each of kind relay, its result in BuildArgs.
func workspaceBuilds(entries []toolwire.LedgerEntry) builds {
	b := builds{ok: true, head: v1alpha1.PartFact{Name: "build-head", Role: v1alpha1.PreviewPartRole("build")}}
	seen := map[toolwire.Side]bool{}
	for _, e := range entries {
		if e.Verb != toolwire.VerbBuild {
			continue
		}
		var a previewtools.BuildArgs
		side := e.Side.Recorded()
		if e.Kind != toolwire.KindRelay || seen[side] || len(b.list) == maxBuilds ||
			json.Unmarshal(e.Args, &a) != nil || !buildResult(a.Result) {
			return builds{}
		}
		seen[side] = true
		tree := v1alpha1.TreeHead
		if side == toolwire.SideBase {
			tree = v1alpha1.TreeBase
		}
		b.list = append(b.list, v1alpha1.LedgerBuild{Side: tree, Result: a.Result, Seq: toolledger.ClampInt32(e.Seq),
			ExitCode: toolledger.ClampInt32(e.Exit), DurationMs: toolledger.ClampInt32(min(max(a.DurationMs, 0),
				3_600_000))})
		switch tree {
		case v1alpha1.TreeHead:
			b.head = partOf(previewtools.BuildPart(a, e.Exit))
			b.headBuilt = a.Result == v1alpha1.BuildBuilt && e.Exit == 0
		case v1alpha1.TreeBase:
			b.baseFailed = a.Result == v1alpha1.BuildFailed && previewtools.RefusalExit(e.Exit)
		}
	}
	return b
}

// buildResult reports one of the build results.
func buildResult(r v1alpha1.BuildResult) bool {
	switch r {
	case v1alpha1.BuildBuilt, v1alpha1.BuildFailed, v1alpha1.BuildTimeout, v1alpha1.BuildError:
		return true
	}
	return false
}

// partOf is the head build's part fact as the verification's environment
// holds it.
func partOf(p acceptance.PartFact) v1alpha1.PartFact {
	f := v1alpha1.PartFact{Name: p.Name, Role: v1alpha1.PreviewPartRole(p.Role)}
	if t := p.LastTermination; t != nil {
		f.LastTermination = &v1alpha1.Termination{Reason: t.Reason, ExitCode: t.ExitCode}
	}
	return f
}

// evaluatorCalled reports an evaluator's call in the ledger: one of its own
// tool calls, or agent-runner's after it exited (confirm, finish).
func evaluatorCalled(entries []toolwire.LedgerEntry) bool {
	return slices.ContainsFunc(entries, func(e toolwire.LedgerEntry) bool {
		return e.Kind == toolwire.KindAgent || e.Kind == toolwire.KindRunner
	})
}

// storeLedger writes the verified ledger onto pv's status (its index and
// builds, the head's replay, confirmations and confirmed checks, the base's
// runs, base required, the head build's part) and the ledger whole into
// <pv>-ledger, created once: one already there is this attempt's own,
// written before a status update that did not land.
func (r *Reconciler) storeLedger(ctx context.Context, pv *v1alpha1.PreviewVerification, out toolledger.Output,
	b builds) error {
	entries := out.Ledger.Entries
	data, err := toolledger.ConfigMapData(out.Ledger)
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: pv.Name + ledgerSuffix, Namespace: pv.Namespace,
			Labels: map[string]string{v1alpha1.LabelIntent: pv.Spec.IntentRef.Name,
				v1alpha1.LabelPreviewVerification: string(pv.UID)},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(),
				Kind: "PreviewVerification", Name: pv.Name, UID: pv.UID, Controller: new(true)}}},
		Immutable: new(true),
		Data:      data,
	}
	if err := r.Create(ctx, cm); err != nil && !kerrors.IsAlreadyExists(err) {
		return err
	}
	pv.Status.Ledger = &v1alpha1.LedgerSummary{Count: toolledger.ClampInt32(out.Ledger.Summary.Count),
		SHA256: out.Ledger.Summary.SHA256, Entries: toolledger.Index(entries), ConfigMap: cm.Name, Builds: b.list}
	pv.Status.Replay = toolledger.ReplayRuns(entries)
	pv.Status.Confirmations = toolledger.ConfirmationRuns(entries)
	pv.Status.Confirmed = toolledger.ConfirmedChecks(entries, pv.Spec.Outcomes)
	pv.Status.Base = &v1alpha1.BaseStatus{Required: true, Unbuildable: b.baseFailed,
		Runs: toolledger.BaseRuns(entries)}
	pv.Status.Environment = []v1alpha1.PartFact{b.head}
	r.log().LogAttrs(ctx, slog.LevelInfo, "agents verification collected", slog.String("verification", pv.Name),
		slog.Int("entries", len(entries)), slog.Bool("head_built", b.headBuilt),
		slog.Bool("base_unbuildable", b.baseFailed), slog.Int("base_runs", len(pv.Status.Base.Runs)))
	return nil
}

// toolsTerminated reports the tools container terminated: it ran, so its
// log and termination message are its ledger.
func toolsTerminated(pod *corev1.Pod) bool {
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name == toolsContainer && s.State.Terminated != nil {
			return true
		}
	}
	return false
}

// terminationMessage is the tools container's termination message.
func terminationMessage(pod *corev1.Pod) string {
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name == toolsContainer && s.State.Terminated != nil {
			return s.State.Terminated.Message
		}
	}
	return ""
}

// imageIDOf is the image ID the named container ran.
func imageIDOf(pod *corev1.Pod, container string) string {
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name == container {
			return s.ImageID
		}
	}
	return ""
}
