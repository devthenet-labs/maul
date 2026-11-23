// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/intentview"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/templates"
	"github.com/devthenet-labs/maul/internal/toolledger"
	"github.com/devthenet-labs/maul/internal/web/auth"
	"github.com/devthenet-labs/maul/internal/web/authz"
)

// The evaluation evidence view (agent evaluator, SPEC S1-23): one evaluate
// run's round as maul recorded it. Its PreviewVerification is read through
// the uncached reader (get only) and held to the run: the name and UID the
// run's spec.evaluation pins, and an intentRef naming this very Intent. Its
// ledger and evidence ConfigMaps are owned by that verification and read
// through guardedConfigMap, so a ConfigMap of the same name another object
// controls is never served. The outcomes, maul's decision on each with the
// calls it counted ("evaluator observed": the verdict's cites in the
// round's verified ledger index) and the ledger index are tier 1
// (projects/intents); the ledger's arguments and outputs, which calls ran
// the evaluator's own page script or program, the evidence images and the
// evaluator's own claims are tier 2 (projects/transcripts), like every
// other agent and application text.

// Bounds of what the view sends.
const (
	// maxLedgerShown bounds one ledger output or argument shown.
	maxLedgerShown = 64 << 10
	// maxEvaluationText bounds an evaluator-written line (a note, a reason,
	// an anomaly's summary): report.NoteMaxChars, in bytes with room for
	// multibyte characters.
	maxEvaluationText = 4 * report.NoteMaxChars
)

// evidenceImageKey is an evidence image's key in <pv>-evidence, as
// preview-controller writes it: <seq>-<sha256>.png (internal/controller/
// preview evidenceKey). Anything else in a path is refused before a read.
var evidenceImageKey = regexp.MustCompile(`^[1-9][0-9]{0,2}-[0-9a-f]{64}\.png$`)

// pngSignature starts every PNG; an evidence image is served only when its
// bytes do.
var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// evaluationRequest answers the run's Intent and tier, the run, and its
// verification, answering the request itself on failure; ok is false when
// it did. A run that is not an evaluate run, or whose verification is
// missing or not the one it pins, is not found.
func (s *Server) evaluationRequest(w http.ResponseWriter, r *http.Request) (auth.Identity, *v1alpha1.Intent,
	authz.Tier, *v1alpha1.IntentRun, *v1alpha1.PreviewVerification, bool) {
	id, in, tier, ok := s.intentRequest(w, r)
	if !ok {
		return id, nil, tier, nil, nil, false
	}
	ctx := r.Context()
	run, err := s.intentRunFor(ctx, in, r.PathValue("run"))
	if err == nil {
		var pv *v1alpha1.PreviewVerification
		if pv, err = s.runVerification(ctx, in, run); err == nil {
			return id, in, tier, run, pv, true
		}
	}
	if errors.Is(err, errNotVisible) {
		notFound(w)
		return id, nil, tier, nil, nil, false
	}
	s.log.LogAttrs(ctx, slog.LevelError, "load evaluation", slog.String("intent", in.Name), slog.Any("error", err))
	http.Error(w, "failed to load the evaluation", http.StatusInternalServerError)
	return id, nil, tier, nil, nil, false
}

// runVerification reads an evaluate run's PreviewVerification and holds it
// to the run and the Intent by name and UID.
func (s *Server) runVerification(ctx context.Context, in *v1alpha1.Intent,
	run *v1alpha1.IntentRun) (*v1alpha1.PreviewVerification, error) {
	ev := run.Spec.Evaluation
	if run.Spec.Stage != v1alpha1.IntentStageEvaluate || ev == nil || ev.Verification == "" || ev.UID == "" {
		return nil, errNotVisible
	}
	var pv v1alpha1.PreviewVerification
	if err := s.reader.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: ev.Verification}, &pv); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errNotVisible
		}
		return nil, err
	}
	if pv.UID != ev.UID || pv.Spec.IntentRef.Name != in.Name || pv.Spec.IntentRef.UID != in.UID {
		s.log.LogAttrs(ctx, slog.LevelWarn, "verification refused: not the run's own",
			slog.String("verification", pv.Name), slog.String("intent", in.Name), slog.String("run", run.Name))
		return nil, errNotVisible
	}
	return &pv, nil
}

// handleRunEvaluation serves one evaluate run's evidence view.
func (s *Server) handleRunEvaluation(w http.ResponseWriter, r *http.Request) {
	id, in, tier, run, pv, ok := s.evaluationRequest(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	out := buildRunEvaluation(in, run, pv, tier)
	if tier == authz.TierTranscripts {
		s.addLedgerOutputs(ctx, in, pv, out.Ledger)
		s.auditRead(ctx, id, in, run.Name, "evaluation")
	}
	writeJSONGzip(w, r, out)
}

// buildRunEvaluation is the view from the verification, the Intent's status
// and (tier 2) the run's report, without the ConfigMaps.
func buildRunEvaluation(in *v1alpha1.Intent, run *v1alpha1.IntentRun, pv *v1alpha1.PreviewVerification,
	tier authz.Tier) RunEvaluation {
	out := RunEvaluation{
		Intent: in.Name, Run: run.Name, Tier: tierName(tier), Verification: pv.Name,
		Round: pv.Spec.Round, Attempt: pv.Spec.Attempt,
		Phase: string(pv.Status.Phase), Reason: string(pv.Status.Reason),
		Outcomes: []EvaluationOutcome{},
	}
	ver := in.Status.Verification
	out.Current = ver != nil && ver.Ref != nil && ver.Ref.UID == pv.UID
	if meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionVerificationSuperseded) {
		out.Withdrawn = templates.WithdrawnSentence
	}
	// The Intent's results and anomalies are this round's only when its
	// judgement wrote them: a current round that settled unjudged shows
	// none of an earlier round's (S2-B3).
	judged := out.Current && ver.OutcomesJudgedAt(pv.Spec.Round, pv.Spec.HeadSetDigest)
	// Only a judgement under the observed rules is worded as one: an
	// earlier release's, which the upgrade has not yet withdrawn, shows its
	// results alone.
	observed := judged && ver.Judge == v1alpha1.JudgeObserved
	decided := map[string]v1alpha1.OutcomeResult{}
	if judged {
		decided, out.Anomalies = judgedRound(ver)
		if observed && len(decided) > 0 {
			out.Lead = templates.ObservedLead
		}
	}
	if out.Current && ver.Handoff != nil {
		h := ver.Handoff
		out.Handoff = &EvaluationHandoff{Mode: string(h.Mode), Released: h.Released,
			ReleasedAt: stampPtr(h.ReleasedAt), Reason: string(h.Reason)}
	}
	out.Outcomes = evaluationOutcomes(pv, decided, observed, evaluationClaims(run, tier))
	agentsView(&out, pv)
	if l := pv.Status.Ledger; l != nil {
		out.Ledger = &EvaluationLedger{Count: l.Count, SHA256: l.SHA256, Entries: []EvaluationLedgerEntry{}}
		for _, e := range l.Entries {
			le := EvaluationLedgerEntry{Seq: e.Seq, Kind: string(e.Kind), Verb: string(e.Verb), Side: string(e.Side),
				Exit: e.Exit, Refused: e.Refused, ResultSHA256: e.ResultSHA256}
			for _, a := range e.Artifacts {
				le.Artifacts = append(le.Artifacts, EvaluationArtifact{SHA256: a})
			}
			out.Ledger.Entries = append(out.Ledger.Entries, le)
		}
	}
	if c := pv.Status.Capture; c != nil {
		out.Capture = &EvaluationCapture{Events: c.Events}
		for _, cc := range c.Components {
			out.Capture.Components = append(out.Capture.Components,
				EvaluationComponentCapture{Name: cc.Name, Bytes: cc.Bytes, Truncated: cc.Truncated})
		}
	}
	return out
}

// evaluationOutcomes are pv's approved outcomes, each with maul's decided
// result (worded, with the calls it counted, only for an observed
// judgement) and, with claims (tier 2), the evaluator's first verdict of
// it, the one maul judged.
func evaluationOutcomes(pv *v1alpha1.PreviewVerification, decided map[string]v1alpha1.OutcomeResult, observed bool,
	claims *report.Evaluation) []EvaluationOutcome {
	out := make([]EvaluationOutcome, 0, len(pv.Spec.Outcomes))
	for _, o := range pv.Spec.Outcomes {
		eo := EvaluationOutcome{ID: o.ID, Outcome: intentview.Text(o.Outcome, 0),
			Kind: string(o.EffectiveKind()), Manual: o.Manual}
		if d, ok := decided[o.ID]; ok {
			eo.Result, eo.Reason = string(d.Result), intentview.Text(d.Reason, maxEvaluationText)
			if observed {
				eo.Words, eo.Cites = templates.ObservedOutcomeWords(string(d.Result), d.Reason), d.Cites
			}
		}
		if claims != nil {
			if i := slices.IndexFunc(claims.Verdicts, func(v report.OutcomeVerdict) bool { return v.ID == o.ID }); i >= 0 {
				v := claims.Verdicts[i]
				eo.Verdict, eo.Cited = string(v.Verdict), v.Ledger
				eo.Note = intentview.Text(v.Note, maxEvaluationText)
			}
		}
		out = append(out, eo)
	}
	return out
}

// evaluationClaims is the run's report as the evaluator wrote it, tier 2
// only; nil for a report that does not parse, which was never acted on (its
// text is the run panel's report), and for one writing a key the observed
// evaluator retired, which claims nothing (report.CarriesRetiredKeys, as
// the controller judges it).
func evaluationClaims(run *v1alpha1.IntentRun, tier authz.Tier) *report.Evaluation {
	if tier != authz.TierTranscripts || run.Status.Report == "" {
		return nil
	}
	claims, err := report.ParseEvaluation([]byte(run.Status.Report))
	if err != nil || claims.CarriesRetiredKeys() {
		return nil
	}
	return claims
}

// agentsView fills what only an agents round has: its placement and the
// tree builds.
func agentsView(out *RunEvaluation, pv *v1alpha1.PreviewVerification) {
	if !pv.Spec.IsAgents() {
		return
	}
	out.Placement = string(v1alpha1.PlacementAgents)
	if l := pv.Status.Ledger; l != nil {
		for _, b := range l.Builds {
			out.Builds = append(out.Builds, EvaluationBuild{Tree: string(b.Side), Result: string(b.Result),
				Seq: b.Seq, ExitCode: b.ExitCode, DurationMs: b.DurationMs})
		}
	}
}

// judgedRound is what ver's judgement decided: each outcome's result by id,
// and its anomalies as the view shows them.
func judgedRound(ver *v1alpha1.IntentVerification) (map[string]v1alpha1.OutcomeResult, []EvaluationAnomaly) {
	decided := map[string]v1alpha1.OutcomeResult{}
	anomalies := make([]EvaluationAnomaly, 0, len(ver.Anomalies))
	for _, o := range ver.Outcomes {
		decided[o.ID] = o
	}
	for _, a := range ver.Anomalies {
		ea := EvaluationAnomaly{Summary: intentview.Text(a.Summary, maxEvaluationText),
			Route: intentview.Text(a.Route, maxEvaluationText), Ledger: a.Ledger}
		if a.Round != 0 && a.Round != ver.OutcomesRound {
			// Carried from an earlier round at these heads: its seqs are
			// that round's ledger's, not this one's.
			ea.FoundInRound, ea.Ledger = a.Round, nil
		}
		anomalies = append(anomalies, ea)
	}
	return decided, anomalies
}

// addLedgerOutputs fills each ledger entry's arguments and output from the
// verification's <pv>-ledger, labels the calls that ran the evaluator's own
// page script or program, and names each artifact's stored image from its
// <pv>-evidence: tier 2 only. A line is shown only as the call the index
// records, and its output only when it hashes to the index's sha256; a
// ConfigMap that is missing or not the verification's own leaves the index
// as it is.
func (s *Server) addLedgerOutputs(ctx context.Context, in *v1alpha1.Intent, pv *v1alpha1.PreviewVerification,
	l *EvaluationLedger) {
	sum := pv.Status.Ledger
	if l == nil || sum == nil {
		return
	}
	if sum.ConfigMap != "" {
		if cm, err := s.guardedConfigMap(ctx, sum.ConfigMap, in.Name, pv.UID); err == nil {
			lines := toolledger.ReadStored(cm.Data[toolledger.Key])
			l.Outputs = true
			agents, binary := pv.Spec.IsAgents(), cliBinary(pv)
			for i := range l.Entries {
				if i < len(sum.Entries) {
					fillLedgerEntry(&l.Entries[i], sum.Entries[i], lines, agents, binary)
				}
			}
		} else if !errors.Is(err, errNotVisible) {
			s.log.LogAttrs(ctx, slog.LevelError, "read ledger", slog.String("verification", pv.Name),
				slog.Any("error", err))
		}
	}
	if sum.Evidence != "" {
		if cm, err := s.guardedConfigMap(ctx, sum.Evidence, in.Name, pv.UID); err == nil {
			for i := range l.Entries {
				e := &l.Entries[i]
				for j := range e.Artifacts {
					key := evidenceName(e.Seq, e.Artifacts[j].SHA256)
					if bytes.HasPrefix(cm.BinaryData[key], pngSignature) {
						e.Artifacts[j].Image = key
					}
				}
			}
		} else if !errors.Is(err, errNotVisible) {
			s.log.LogAttrs(ctx, slog.LevelError, "read evidence", slog.String("verification", pv.Name),
				slog.Any("error", err))
		}
	}
}

// cliBinary is an agents round's built cli binary, "" for a library or a
// slot round.
func cliBinary(pv *v1alpha1.PreviewVerification) string {
	if ws := pv.Spec.Workspace; ws != nil && ws.Surface.CLI != nil {
		return ws.Surface.CLI.Binary
	}
	return ""
}

// fillLedgerEntry shows entry e (index entry ix) from its stored line: what
// it sent when the line is the index's call, and its output only when that
// hashes to the index's sha256. Own is toolledger.OwnCall over the index's
// verb and, where it is the call, the line's request, as the comment labels
// a cited call.
func fillLedgerEntry(e *EvaluationLedgerEntry, ix v1alpha1.LedgerIndexEntry, lines map[int64]toolledger.StoredLine,
	agents bool, binary string) {
	line, ok := lines[int64(e.Seq)]
	if !ok || !line.Of(ix) {
		e.OutputDropped = true
		e.Own = toolledger.OwnCall(ix.Verb, nil, agents, binary)
		return
	}
	e.Own = toolledger.OwnCall(ix.Verb, line.Args, agents, binary)
	if len(line.Args) > 0 {
		e.Args = intentview.Text(string(line.Args), maxLedgerShown)
	}
	if !line.OutputKept() {
		e.OutputDropped = true
		return
	}
	e.Output = intentview.Text(line.Output, maxLedgerShown)
}

// evidenceName is the key of entry seq's image of sha256 in <pv>-evidence.
func evidenceName(seq int32, sha string) string { return fmt.Sprintf("%d-%s.png", seq, sha) }

// handleEvidenceImage serves one evidence image, tier 2 only: read from the
// run's verification's own <pv>-evidence and served only when its bytes
// are a PNG's, as image/png with nosniff under the envelope's strict CSP,
// never cached.
func (s *Server) handleEvidenceImage(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("image")
	if !evidenceImageKey.MatchString(key) {
		notFound(w)
		return
	}
	id, in, tier, run, pv, ok := s.evaluationRequest(w, r)
	if !ok {
		return
	}
	if tier != authz.TierTranscripts {
		s.denyContent(w, id, in)
		return
	}
	ctx := r.Context()
	if pv.Status.Ledger == nil || pv.Status.Ledger.Evidence == "" {
		notFound(w)
		return
	}
	cm, err := s.guardedConfigMap(ctx, pv.Status.Ledger.Evidence, in.Name, pv.UID)
	switch {
	case errors.Is(err, errNotVisible):
		notFound(w)
		return
	case err != nil:
		s.log.LogAttrs(ctx, slog.LevelError, "read evidence", slog.String("verification", pv.Name),
			slog.Any("error", err))
		http.Error(w, "failed to load the image", http.StatusInternalServerError)
		return
	}
	img := cm.BinaryData[key]
	if !bytes.HasPrefix(img, pngSignature) {
		if len(img) > 0 {
			s.log.LogAttrs(ctx, slog.LevelWarn, "evidence refused: not a PNG",
				slog.String("verification", pv.Name), slog.String("image", key))
		}
		notFound(w)
		return
	}
	s.auditRead(ctx, id, in, run.Name, "evidence "+key)
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", "inline")
	h.Set("Cache-Control", "private, no-store")
	_, _ = w.Write(img)
}
