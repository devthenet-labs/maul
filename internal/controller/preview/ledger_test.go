// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	stdimage "image"
	"image/png"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// verifiedLedger parses a toolLedger as preview-controller does.
func verifiedLedger(t *testing.T, log []byte, msg string) toolOutput {
	t.Helper()
	out := parseToolOutput(log, msg)
	if out.reason != "" {
		t.Fatalf("fixture ledger refused: %s", out.reason)
	}
	return out
}

func TestParseToolOutput(t *testing.T) {
	shot := testPNG(t, 8, 8)
	log, msg := toolLedger(t, shot)
	out := verifiedLedger(t, log, msg)
	if len(out.ledger.Entries) != 7 || len(out.evidence) != 1 || out.dropped != 0 {
		t.Fatalf("parsed %d entries, %d evidence, %d dropped", len(out.ledger.Entries), len(out.evidence), out.dropped)
	}
	evLine := func(seq int64, sha string, data []byte) []byte {
		b, _ := json.Marshal(evidenceLine{V: toolwire.WireVersion, Seq: seq, SHA256: sha, Data: data})
		return append([]byte(toolwire.EvidencePrefix), append(b, '\n')...)
	}
	summaryAt := bytes.LastIndex(log, []byte(toolwire.EntryPrefix))
	for _, tc := range []struct {
		name    string
		log     []byte
		msg     string
		reason  v1alpha1.PreviewVerificationReason
		dropped int
	}{
		{"an extra evidence line whose bytes are not its sha256 drops alone",
			append(bytes.Clone(log), evLine(4, sha256Hex(shot), []byte("other"))...), msg, "", 1},
		{"malformed evidence drops alone",
			append(bytes.Clone(log), []byte(toolwire.EvidencePrefix+"{nope\n")...), msg, "", 1},
		{"a changed entry", bytes.Replace(log, []byte(`"seq":2`), []byte(`"seq":9`), 1), msg,
			v1alpha1.VerificationReasonLedgerMismatch, 0},
		{"another termination message", log, `{"count":7,"sha256":"` + strings.Repeat("0", 64) + `"}`,
			v1alpha1.VerificationReasonLedgerMismatch, 0},
		{"no summary", log[:summaryAt], msg, v1alpha1.VerificationReasonDeadlineNoSummary, 0},
		{"over the bound", make([]byte, toolwire.MaxLogBytes+1), msg, v1alpha1.VerificationReasonLedgerMismatch, 0},
	} {
		got := parseToolOutput(tc.log, tc.msg)
		if got.reason != tc.reason || got.dropped != tc.dropped {
			t.Errorf("%s: reason %q dropped %d, want %q %d", tc.name, got.reason, got.dropped, tc.reason, tc.dropped)
		}
	}
}

// What status holds of the ledger is read from the ledger alone: a
// confirmation whose check is over the saved bound, bound to no outcome
// under evaluation, or that failed, confirms nothing.
func TestConfirmedChecks(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var entries []toolwire.LedgerEntry
	add := func(e toolwire.LedgerEntry) {
		e.Seq, e.TS = int64(len(entries)+1), at
		entries = append(entries, e)
	}
	pass := []toolwire.Attempt{{Run: "0123456789abcdef", Result: toolwire.ResultPass}}
	propose := func(body string, confirmed toolwire.RunResult) string {
		c, err := checks.Parse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		d := strings.TrimPrefix(checks.Digest(c), "sha256:")
		args, _ := json.Marshal(toolwire.CheckRunRequest{Spec: checks.Canonical(c)})
		add(toolwire.LedgerEntry{Kind: toolwire.KindAgent, Verb: toolwire.VerbCheckRun, Args: args,
			CheckRun: &toolwire.CheckRunRecord{Digest: d, Mode: toolwire.ModeDryRun, Result: toolwire.RunPass,
				Attempts: pass}})
		attempts := pass
		if confirmed == toolwire.RunFail {
			attempts = []toolwire.Attempt{{Run: "a", Result: toolwire.ResultFail}, {Run: "b", Result: toolwire.ResultFail}}
		}
		add(toolwire.LedgerEntry{Kind: toolwire.KindRelay, Verb: toolwire.VerbConfirm, Args: json.RawMessage(`{}`),
			CheckRun: &toolwire.CheckRunRecord{Digest: d, Mode: toolwire.ModeConfirm, Result: confirmed,
				Attempts: attempts}})
		return d
	}
	good := propose(testCheckBody, toolwire.RunPass)
	propose(strings.ReplaceAll(testCheckBody, "items", "other"), toolwire.RunPass)
	propose(strings.Replace(testCheckBody, "items.body", "items.fails", 1), toolwire.RunFail)
	big := strings.Replace(testCheckBody, "/api/echo", "/api/"+strings.Repeat("a", v1alpha1.MaxSavedCheckBytes), 1)
	if _, err := checks.Parse([]byte(big)); err == nil {
		propose(strings.Replace(big, "items.body", "items.big", 1), toolwire.RunPass)
	}
	got := confirmedChecks(entries, testOutcomes())
	if len(got) != 1 || got[0].Digest != "sha256:"+good || got[0].ID != "items.body" {
		t.Errorf("confirmed %+v", got)
	}
	runs := confirmationRuns(entries)
	if len(runs) < 3 || runs[0].DryRunSeq != 1 || runs[0].Seq != 2 || runs[2].Result != v1alpha1.CheckRunFail {
		t.Errorf("confirmations %+v", runs)
	}
}

// The full ledger ConfigMap holds every entry's line within its bound:
// the oldest outputs go first, every seq and output digest stays.
func TestLedgerConfigMapBound(t *testing.T) {
	var out bytes.Buffer
	w := toolwire.NewWriter(&out)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for i := range 40 {
		if _, err := w.Append(toolwire.LedgerEntry{TS: at, Kind: toolwire.KindAgent, Verb: toolwire.VerbHTTP,
			Args: json.RawMessage(`{}`), Output: strings.Repeat(fmt.Sprint(i%10), toolwire.MaxOutputBytes)}); err != nil {
			t.Fatal(err)
		}
	}
	sum, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	l := verifiedLedger(t, out.Bytes(), sum.TerminationMessage()).ledger
	pv := &v1alpha1.PreviewVerification{}
	pv.Name, pv.Namespace, pv.UID, pv.Spec.IntentRef.Name = "shop-1-v1-a1", "maul", "pv-1", "shop-1"
	cm, err := ledgerConfigMap(pv, l)
	if err != nil {
		t.Fatal(err)
	}
	size := 0
	for k, v := range cm.Data {
		size += len(k) + len(v)
	}
	if size > maxLedgerConfigMap {
		t.Errorf("ledger ConfigMap holds %d bytes, over %d", size, maxLedgerConfigMap)
	}
	lines := strings.Split(strings.TrimSuffix(cm.Data[ledgerKey], "\n"), "\n")
	if len(lines) != 40 {
		t.Fatalf("%d lines, want every entry's", len(lines))
	}
	var first, last toolwire.LedgerEntry
	_ = json.Unmarshal([]byte(lines[0]), &first)
	_ = json.Unmarshal([]byte(lines[39]), &last)
	if first.Output != droppedOutput || first.OutputSHA256 != l.Entries[0].OutputSHA256 || first.Seq != 1 {
		t.Errorf("oldest entry %d kept its output or lost its digest", first.Seq)
	}
	if last.Output != l.Entries[39].Output {
		t.Error("the newest entry's output was dropped")
	}
	if !*cm.Immutable || cm.OwnerReferences[0].Kind != "PreviewVerification" || cm.Name != "shop-1-v1-a1-ledger" {
		t.Errorf("ledger ConfigMap meta %+v", cm.ObjectMeta)
	}
}

func TestKeepEvidence(t *testing.T) {
	small := testPNG(t, 8, 8)
	noisy := func() []byte {
		img := stdimage.NewNRGBA(stdimage.Rect(0, 0, 700, 700))
		_, _ = rand.Read(img.Pix)
		var b bytes.Buffer
		if err := png.Encode(&b, img); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}()
	notPNG := []byte("GIF89a not a png")
	entries := []toolwire.LedgerEntry{
		{Seq: 1, Artifacts: []toolwire.Artifact{{SHA256: sha256Hex(small)}, {SHA256: sha256Hex(notPNG)}}},
		{Seq: 2, Refused: "verb-not-allowed"},
		{Seq: 3, Artifacts: []toolwire.Artifact{{SHA256: sha256Hex(noisy)}}},
	}
	kept, dropped := keepEvidence(entries, []toolwire.Evidence{
		{Seq: 1, SHA256: sha256Hex(small), Data: small},
		{Seq: 1, SHA256: sha256Hex(small), Data: small},   // twice
		{Seq: 2, SHA256: sha256Hex(small), Data: small},   // a refused entry's
		{Seq: 1, SHA256: sha256Hex(notPNG), Data: notPNG}, // not a PNG
		{Seq: 3, SHA256: sha256Hex(noisy), Data: noisy},   // too large once stored
	})
	if len(kept) != 1 || kept[evidenceKey(1, sha256Hex(small))] == nil {
		t.Errorf("kept %d images", len(kept))
	}
	want := map[string]int{dropUncited: 2, dropNotPNG: 1, dropTooLarge: 1}
	if fmt.Sprint(dropped) != fmt.Sprint(want) {
		t.Errorf("dropped %v, want %v", dropped, want)
	}
}

func TestReencodePNG(t *testing.T) {
	out, err := reencodePNG(testPNG(t, 3000, 300))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(out))
	if err != nil || cfg.Width != evidenceWidth || cfg.Height != 128 {
		t.Errorf("re-encoded %dx%d: %v", cfg.Width, cfg.Height, err)
	}
	small, err := reencodePNG(testPNG(t, 20, 10))
	if cfg, _ := png.DecodeConfig(bytes.NewReader(small)); err != nil || cfg.Width != 20 || cfg.Height != 10 {
		t.Errorf("a narrow image was resized: %dx%d, %v", cfg.Width, cfg.Height, err)
	}
	// A header claiming a huge image is refused before any pixel is
	// decoded.
	huge := testPNG(t, 1, 1)
	copy(huge[16:24], []byte{0, 0, 0x4e, 0x20, 0, 0, 0, 1}) // IHDR width 20000
	binary.BigEndian.PutUint32(huge[29:33], crc32.ChecksumIEEE(huge[12:29]))
	if cfg, err := png.DecodeConfig(bytes.NewReader(huge)); err != nil || cfg.Width != 20000 {
		t.Fatalf("fixture header %+v: %v", cfg, err)
	}
	if _, err := reencodePNG(huge); err == nil || !strings.Contains(err.Error(), "dimensions") {
		t.Errorf("an image 20000 px wide: %v", err)
	}
}
