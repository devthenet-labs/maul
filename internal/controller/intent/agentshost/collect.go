// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentshost

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/httpstep"
	"github.com/devthenet-labs/maul/internal/previewtools"
	"github.com/devthenet-labs/maul/internal/toolledger"
	"github.com/devthenet-labs/maul/internal/toolpod/podstate"
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
	// headBuilt: the head built. Whether the base built gates nothing (the
	// base is exploration only); its result is on list.
	headBuilt bool
	// ok is false for a ledger whose builds are not one of each tree.
	ok bool
}

// collect settles a finished tool pod's attempt from the tools container's
// log, verified against its termination message. A ledger that holds and
// does not carry the tools key is indexed onto status with its builds and
// the head build's part, and stored whole in <pv>-ledger. A head that did
// not build is Infra tools-build-failed; a round no evaluator called is
// Infra evaluator-not-run
// (or tools-not-ready); a ledger that does not hold is Infra, never a
// verdict (collectedAs).
func (r *Reconciler) collect(ctx context.Context, pv *v1alpha1.PreviewVerification, pod *corev1.Pod,
	log []byte) error {
	msg := podstate.TerminationMessage(pod, toolsContainer)
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
	pv.Status.ToolsImageID = podstate.ImageID(pod, toolsContainer)
	return r.settle(ctx, pv, phase, reason)
}

// collectedAs is the phase and reason a collected attempt settles at: Infra
// for a ledger that does not hold (out.Reason), a head that did not build,
// and a round no evaluator called (an earlier release's relay that finished
// kept-copy-failed among them), whatever its retired rung
// (toolledger.RoundReason); Observed ok otherwise.
func collectedAs(pv *v1alpha1.PreviewVerification, out toolledger.Output, b builds) (
	v1alpha1.PreviewVerificationPhase, v1alpha1.PreviewVerificationReason) {
	switch {
	case out.Reason != "":
		return v1alpha1.VerificationInfra, out.Reason
	case !b.headBuilt:
		return v1alpha1.VerificationInfra, v1alpha1.VerificationReasonToolsBuildFailed
	}
	if reason := toolledger.RoundReason(out.Ledger.Entries, pv.Status.Tools != nil); reason != "" {
		return v1alpha1.VerificationInfra, reason
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
		if tree == v1alpha1.TreeHead {
			b.head = partOf(previewtools.BuildPart(a, e.Exit))
			b.headBuilt = a.Result == v1alpha1.BuildBuilt && e.Exit == 0
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
func partOf(p httpstep.PartFact) v1alpha1.PartFact {
	f := v1alpha1.PartFact{Name: p.Name, Role: v1alpha1.PreviewPartRole(p.Role)}
	if t := p.LastTermination; t != nil {
		f.LastTermination = &v1alpha1.Termination{Reason: t.Reason, ExitCode: t.ExitCode}
	}
	return f
}

// storeLedger writes the verified ledger onto pv's status (its index and
// builds, the head build's part; no base status: the base is exploration
// only) and the ledger whole into
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
	sum := toolledger.ApplyStatus(out)
	sum.ConfigMap, sum.Builds = cm.Name, b.list
	pv.Status.Ledger = sum
	pv.Status.Environment = []v1alpha1.PartFact{b.head}
	r.log().LogAttrs(ctx, slog.LevelInfo, "agents verification collected", slog.String("verification", pv.Name),
		slog.Int("entries", len(entries)), slog.Bool("head_built", b.headBuilt))
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
