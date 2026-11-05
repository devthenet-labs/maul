// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	stdimage "image"
	"image/png"
	"maps"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/toolledger"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// verifiedLedger parses a toolLedger as preview-controller does.
func verifiedLedger(t *testing.T, log []byte, msg string) toolledger.Output {
	t.Helper()
	out := toolledger.Parse(log, msg)
	if out.Reason != "" {
		t.Fatalf("fixture ledger refused: %s", out.Reason)
	}
	return out
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// The full ledger ConfigMap is toolledger's bounded data, immutable, named
// for and owned by its verification.
func TestLedgerConfigMap(t *testing.T) {
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
	l := verifiedLedger(t, out.Bytes(), sum.TerminationMessage()).Ledger
	pv := &v1alpha1.PreviewVerification{}
	pv.Name, pv.Namespace, pv.UID, pv.Spec.IntentRef.Name = "shop-1-v1-a1", "maul", "pv-1", "shop-1"
	cm, err := ledgerConfigMap(pv, l)
	if err != nil {
		t.Fatal(err)
	}
	want, err := toolledger.ConfigMapData(l)
	if err != nil || !maps.Equal(cm.Data, want) {
		t.Errorf("ledger ConfigMap data is not toolledger's: %v", err)
	}
	if !*cm.Immutable || cm.OwnerReferences[0].Kind != "PreviewVerification" || cm.Name != "shop-1-v1-a1-ledger" ||
		cm.Labels[v1alpha1.LabelPreviewVerification] != "pv-1" || cm.Labels[v1alpha1.LabelIntent] != "shop-1" {
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
