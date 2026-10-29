// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"bytes"
	"errors"
	"fmt"
	stdimage "image"
	"image/color"
	"image/png"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// The evidence (SPEC Q7): the images an evaluator's report cites reach
// preview-controller only from the tools container, as evidence lines after
// the ledger's entries. Each is kept only when it is an artifact its ledger
// entry recorded (by sha256, on an unrefused entry), a PNG by its magic
// bytes that decodes within bounds, and fits: it is re-encoded with Go's
// image/png, downscaled to at most evidenceWidth pixels wide, and stored in
// <pv>-evidence, which the verification owns, at most maxLedgerConfigMap
// in all. Anything else is dropped and counted, never stored.

const (
	// evidenceWidth is the widest an image is stored.
	evidenceWidth = 1280
	// maxEvidenceSide and maxEvidencePixels bound an image decoded at all.
	maxEvidenceSide   = 16384
	maxEvidencePixels = 40 << 20
)

// Why an evidence image was dropped.
const (
	dropMalformed = "malformed"
	dropUncited   = "uncited"
	dropNotPNG    = "not-png"
	dropTooLarge  = "too-large"
)

// pngMagic starts every PNG.
var pngMagic = []byte("\x89PNG\r\n\x1a\n")

// evidenceKey keys image sha256 of entry seq in <pv>-evidence.
func evidenceKey(seq int64, sha string) string { return fmt.Sprintf("%d-%s.png", seq, sha) }

// keepEvidence holds evs to the verified entries and re-encodes those it
// keeps; dropped counts the rest by reason.
func keepEvidence(entries []toolwire.LedgerEntry, evs []toolwire.Evidence) (map[string][]byte, map[string]int) {
	cited := map[int64]map[string]bool{}
	for _, e := range entries {
		if e.Refused != "" {
			continue
		}
		for _, a := range e.Artifacts {
			if cited[e.Seq] == nil {
				cited[e.Seq] = map[string]bool{}
			}
			cited[e.Seq][a.SHA256] = true
		}
	}
	kept := map[string][]byte{}
	dropped := map[string]int{}
	size := 0
	for _, ev := range evs {
		key := evidenceKey(ev.Seq, ev.SHA256)
		switch {
		case !cited[ev.Seq][ev.SHA256] || kept[key] != nil:
			dropped[dropUncited]++
			continue
		case len(kept) == toolwire.MaxEvidence:
			dropped[dropTooLarge]++
			continue
		case !bytes.HasPrefix(ev.Data, pngMagic):
			dropped[dropNotPNG]++
			continue
		}
		img, err := reencodePNG(ev.Data)
		if err != nil {
			dropped[dropNotPNG]++
			continue
		}
		if size+len(key)+len(img) > maxLedgerConfigMap {
			dropped[dropTooLarge]++
			continue
		}
		size += len(key) + len(img)
		kept[key] = img
	}
	return kept, dropped
}

// reencodePNG decodes a PNG within bounds and encodes it again, scaled
// down to evidenceWidth wide when wider: only pixels survive, no chunk of
// the original.
func reencodePNG(data []byte) ([]byte, error) {
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxEvidenceSide || cfg.Height > maxEvidenceSide ||
		cfg.Width*cfg.Height > maxEvidencePixels {
		return nil, errors.New("image dimensions out of bounds")
	}
	src, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&out, downscale(src, evidenceWidth)); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// downscale is src at most width wide, its aspect kept, each pixel the
// average of the source box it covers.
func downscale(src stdimage.Image, width int) stdimage.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= width {
		dst := stdimage.NewNRGBA(stdimage.Rect(0, 0, w, h))
		for y := range h {
			for x := range w {
				dst.Set(x, y, src.At(b.Min.X+x, b.Min.Y+y))
			}
		}
		return dst
	}
	nh := max(h*width/w, 1)
	dst := stdimage.NewNRGBA(stdimage.Rect(0, 0, width, nh))
	for y := range nh {
		y0, y1 := y*h/nh, max((y+1)*h/nh, y*h/nh+1)
		for x := range width {
			x0, x1 := x*w/width, max((x+1)*w/width, x*w/width+1)
			var r, g, bl, a, n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					c := color.NRGBA64Model.Convert(src.At(b.Min.X+sx, b.Min.Y+sy)).(color.NRGBA64)
					r, g, bl, a, n = r+uint64(c.R), g+uint64(c.G), bl+uint64(c.B), a+uint64(c.A), n+1
				}
			}
			dst.Set(x, y, color.NRGBA64{R: uint16(r / n), G: uint16(g / n), B: uint16(bl / n), A: uint16(a / n)})
		}
	}
	return dst
}

// evidenceConfigMap is <pv>-evidence holding images, nil with none.
func evidenceConfigMap(pv *v1alpha1.PreviewVerification, images map[string][]byte) *corev1.ConfigMap {
	if len(images) == 0 {
		return nil
	}
	data := make(map[string][]byte, len(images))
	for _, k := range slices.Sorted(maps.Keys(images)) {
		data[k] = images[k]
	}
	return &corev1.ConfigMap{ObjectMeta: ledgerMeta(pv, pv.Name+evidenceSuffix), Immutable: ptr(true),
		BinaryData: data}
}
