// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/checks/playwright"
	"github.com/devthenet-labs/maul/internal/intentview"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/toolwire"
	"github.com/devthenet-labs/maul/internal/web/auth"
	"github.com/devthenet-labs/maul/internal/web/authz"
)

// The evaluation evidence view (agent evaluator, SPEC S1-23): one evaluate
// run's round as maul recorded it. Its PreviewVerification is read through
// the uncached reader (get only) and held to the run: the name and UID the
// run's spec.evaluation pins, and an intentRef naming this very Intent. Its
// ledger and evidence ConfigMaps are owned by that verification and read
// through guardedConfigMap, so a ConfigMap of the same name another object
// controls is never served. The outcomes, the checks and the ledger index
// are tier 1 (projects/intents); the ledger's arguments and outputs, the
// evidence images and the evaluator's own claims are tier 2
// (projects/transcripts), like every other agent and application text.

// Bounds of what the view sends.
const (
	// maxLedgerShown bounds one ledger output or argument shown.
	maxLedgerShown = 64 << 10
	// maxEvaluationText bounds an evaluator-written line (a note, a reason,
	// an anomaly's summary): report.NoteMaxChars, in bytes with room for
	// multibyte characters.
	maxEvaluationText = 4 * report.NoteMaxChars
	// compileToken is the run token a browser check is compiled with for
	// display when the round recorded no run of it.
	compileToken = "run-token"
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
		Round: pv.Spec.Round, Attempt: pv.Spec.Attempt, Rung: string(pv.Spec.Rung),
		Phase: string(pv.Status.Phase), Reason: string(pv.Status.Reason),
		Outcomes: []EvaluationOutcome{}, Checks: []EvaluationCheck{},
	}
	ver := in.Status.Verification
	out.Current = ver != nil && ver.Ref != nil && ver.Ref.UID == pv.UID
	decided := map[string]v1alpha1.OutcomeResult{}
	disputed := map[string]bool{}
	if out.Current {
		for _, o := range ver.Outcomes {
			decided[o.ID] = o
		}
		for _, d := range ver.Disputed {
			disputed[d] = true
		}
		for _, a := range ver.Anomalies {
			out.Anomalies = append(out.Anomalies, EvaluationAnomaly{Summary: intentview.Text(a.Summary,
				maxEvaluationText), Route: intentview.Text(a.Route, maxEvaluationText), Ledger: a.Ledger})
		}
		if h := ver.Handoff; h != nil {
			out.Handoff = &EvaluationHandoff{Mode: string(h.Mode), Released: h.Released,
				ReleasedAt: stampPtr(h.ReleasedAt), Reason: string(h.Reason)}
		}
	}
	var claims *report.Evaluation
	if tier == authz.TierTranscripts && run.Status.Report != "" {
		// A report that does not parse was never acted on: its claims are
		// left out (its text is the run panel's report).
		claims, _ = report.ParseEvaluation([]byte(run.Status.Report))
	}
	for _, o := range pv.Spec.Outcomes {
		eo := EvaluationOutcome{ID: o.ID, Outcome: intentview.Text(o.Outcome, 0),
			Kind: string(o.EffectiveKind()), Manual: o.Manual}
		if d, ok := decided[o.ID]; ok {
			eo.Result, eo.Basis = string(d.Result), d.Basis
			eo.Line = intentview.Text(d.Line, 0)
			eo.Reason = intentview.Text(d.Reason, maxEvaluationText)
		}
		if claims != nil {
			for _, v := range claims.Verdicts {
				if v.ID == o.ID {
					eo.Verdict, eo.Cited = string(v.Verdict), v.Ledger
					eo.Note = intentview.Text(v.Note, maxEvaluationText)
				}
			}
		}
		out.Outcomes = append(out.Outcomes, eo)
	}
	out.Checks = evaluationChecks(pv, disputed)
	out.Disputes = evaluationDisputes(ver, out.Current, claims)
	if l := pv.Status.Ledger; l != nil {
		out.Ledger = &EvaluationLedger{Count: l.Count, SHA256: l.SHA256, Entries: []EvaluationLedgerEntry{}}
		for _, e := range l.Entries {
			le := EvaluationLedgerEntry{Seq: e.Seq, Kind: string(e.Kind), Verb: string(e.Verb), Exit: e.Exit,
				Refused: e.Refused, ResultSHA256: e.ResultSHA256}
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

// evaluationChecks are the round's checks: the saved checks it replayed, in
// spec order, then the proposed checks it confirmed, each once by digest.
func evaluationChecks(pv *v1alpha1.PreviewVerification, disputed map[string]bool) []EvaluationCheck {
	replay := map[string]v1alpha1.CheckRun{}
	for _, r := range pv.Status.Replay {
		replay[r.CheckDigest] = r
	}
	confirm := map[string]v1alpha1.CheckRun{}
	for _, r := range pv.Status.Confirmations {
		confirm[r.CheckDigest] = r
	}
	out := []EvaluationCheck{}
	seen := map[string]bool{}
	add := func(c v1alpha1.SavedCheck, source string) {
		if seen[c.Digest] {
			return
		}
		seen[c.Digest] = true
		ec := EvaluationCheck{ID: c.ID, Outcome: c.Outcome, Form: string(c.Form), Digest: c.Digest,
			Source: source, Steps: indentedBody(c.Body), Disputed: disputed[c.Digest]}
		token := ""
		if r, ok := replay[c.Digest]; ok {
			ec.Replay = wireCheckRun(r)
			token = r.Token
		}
		if r, ok := confirm[c.Digest]; ok {
			ec.Confirmation = wireCheckRun(r)
			token = r.Token
		}
		if c.Form == v1alpha1.CheckFormBrowser {
			ec.Compiled, ec.CompiledToken = compiledSpec(c.Body, token)
		}
		out = append(out, ec)
	}
	for _, c := range pv.Spec.Checks {
		add(c, "saved")
	}
	for _, c := range pv.Status.Confirmed {
		add(c, "proposed")
	}
	return out
}

func wireCheckRun(r v1alpha1.CheckRun) *EvaluationCheckRun {
	return &EvaluationCheckRun{Result: string(r.Result), Token: r.Token, Seq: r.Seq, DryRunSeq: r.DryRunSeq}
}

// indentedBody is a check's canonical JSON indented for reading, as plain
// text; a body that is not JSON is shown as it is.
func indentedBody(body string) string {
	var b bytes.Buffer
	if err := json.Indent(&b, []byte(body), "", "  "); err != nil {
		return intentview.Text(body, 0)
	}
	return intentview.Text(b.String(), 0)
}

// compiledSpec is the Playwright spec maul compiles from a browser check's
// body, compiled with the round's own run token where it recorded one, and
// the token used; empty when the body does not compile.
func compiledSpec(body, token string) (string, string) {
	c, err := checks.Parse([]byte(body))
	if err != nil || c.Browser == nil {
		return "", ""
	}
	for _, t := range []string{token, compileToken} {
		if t == "" {
			continue
		}
		if spec, err := playwright.Compile(*c.Browser, playwright.Options{RunToken: t}); err == nil {
			return intentview.Text(string(spec), 0), t
		}
	}
	return "", ""
}

// evaluationDisputes are the round's disputed checks: the Intent's disputed
// digests while the round is current, and (tier 2) the evaluator's own
// disputes with their reasons.
func evaluationDisputes(ver *v1alpha1.IntentVerification, current bool,
	claims *report.Evaluation) []EvaluationDispute {
	var out []EvaluationDispute
	index := map[string]int{}
	if current {
		for _, d := range ver.Disputed {
			index[d] = len(out)
			out = append(out, EvaluationDispute{CheckDigest: d})
		}
	}
	if claims != nil {
		for _, d := range claims.Disputes {
			ed := EvaluationDispute{CheckDigest: d.CheckDigest, Reason: intentview.Text(d.Reason,
				maxEvaluationText), Replacement: intentview.Text(d.Replacement, 80), Ledger: d.Ledger}
			if i, ok := index[d.CheckDigest]; ok {
				out[i] = ed
				continue
			}
			index[d.CheckDigest] = len(out)
			out = append(out, ed)
		}
	}
	return out
}

// addLedgerOutputs fills each ledger entry's arguments and output from the
// verification's <pv>-ledger, and names each artifact's stored image from
// its <pv>-evidence: tier 2 only. An output is shown only when it hashes to
// the index's sha256; a ConfigMap that is missing or not the
// verification's own leaves the index as it is.
func (s *Server) addLedgerOutputs(ctx context.Context, in *v1alpha1.Intent, pv *v1alpha1.PreviewVerification,
	l *EvaluationLedger) {
	sum := pv.Status.Ledger
	if l == nil || sum == nil {
		return
	}
	if sum.ConfigMap != "" {
		if cm, err := s.guardedConfigMap(ctx, sum.ConfigMap, in.Name, pv.UID); err == nil {
			lines := ledgerLines(cm.Data[ledgerDataKey])
			l.Outputs = true
			for i := range l.Entries {
				fillLedgerEntry(&l.Entries[i], lines)
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

// ledgerDataKey is the key <pv>-ledger holds its entries under, one JSON
// line each (internal/controller/preview ledgerKey).
const ledgerDataKey = "ledger.jsonl"

// ledgerLines decodes the ledger ConfigMap's lines by seq; a line that does
// not decode is skipped.
func ledgerLines(data string) map[int32]toolwire.LedgerEntry {
	out := map[int32]toolwire.LedgerEntry{}
	for line := range strings.SplitSeq(data, "\n") {
		if line == "" {
			continue
		}
		var e toolwire.LedgerEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil || e.Seq < 1 || e.Seq > toolwire.MaxEntries {
			continue
		}
		out[int32(e.Seq)] = e
	}
	return out
}

// fillLedgerEntry shows an entry's arguments and output from its line, the
// output only when it hashes to the index's sha256.
func fillLedgerEntry(e *EvaluationLedgerEntry, lines map[int32]toolwire.LedgerEntry) {
	line, ok := lines[e.Seq]
	if !ok {
		e.OutputDropped = true
		return
	}
	if len(line.Args) > 0 {
		e.Args = intentview.Text(string(line.Args), maxLedgerShown)
	}
	sum := sha256.Sum256([]byte(line.Output))
	if hex.EncodeToString(sum[:]) != e.ResultSHA256 {
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
