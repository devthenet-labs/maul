// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/agentrun"
	"github.com/devthenet-labs/maul/internal/ghclient"
	"github.com/devthenet-labs/maul/internal/templates"
	"github.com/devthenet-labs/maul/internal/untrusted"
)

// fileDiff is one file's part of a patch, of about size bytes.
func fileDiff(path string, size int) string {
	head := fmt.Sprintf("diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n", path, path, path, path)
	var b strings.Builder
	b.WriteString(head)
	for i := 0; b.Len() < size; i++ {
		fmt.Fprintf(&b, "@@ -%d +%d @@\n+line %d of %s\n", i, i, i, path)
	}
	return b.String()
}

// specialCase is the line a fix that recognises a check's data writes.
const specialCase = `+if name == "maul-run-1" { return 200 }`

// TestEvaluateHandoffSaysWhenTheFixDiffIsCut (code review 2026-11-18,
// agent-prompts 8): a round after a fix whose diff is past the bound hands
// the evaluator a diff cut at a file or hunk boundary within it, and says
// so: how much it shows of how much, and the files not shown in full,
// those cut and those never reached (where a fix may special-case a
// check); a diff GitHub had already cut is said to be cut; a compare that
// failed is said to be unavailable, never a missing section. A whole diff
// says nothing.
func TestEvaluateHandoffSaysWhenTheFixDiffIsCut(t *testing.T) {
	big := fileDiff("vendor/gen/big.go", 40<<10) + "diff --git a/api/battle.go b/api/battle.go\n" + specialCase + "\n"
	for _, tc := range []struct {
		name    string
		patch   string
		err     error
		want    *agentrun.FixDiffCut
		holds   []string
		lacks   []string
		ends    string
		partial bool
	}{
		{name: "whole", patch: fileDiff("api/battle.go", 200) + specialCase + "\n",
			holds: []string{specialCase}},
		{name: "cut", patch: big, partial: true, holds: []string{"diff --git a/vendor/gen/big.go"},
			lacks: []string{specialCase},
			want:  &agentrun.FixDiffCut{Files: []string{"vendor/gen/big.go", "api/battle.go"}}},
		{name: "cut by the forge", patch: fileDiff("api/battle.go", 200) + strings.TrimLeft(ghclient.PatchTruncated, "\n"),
			want:  &agentrun.FixDiffCut{ForgeCut: []string{"acme/app"}, Files: []string{"api/battle.go"}},
			lacks: []string{"compare patch truncated"}},
		{name: "unavailable", err: errors.New("compare: 502"),
			want: &agentrun.FixDiffCut{Unavailable: []string{"acme/app"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := fixRoundHandoff(t, tc.patch, tc.err)
			if len(h.FixDiff) > templates.FixDiffMaxBytes {
				t.Fatalf("fix diff of %d bytes, over %d", len(h.FixDiff), templates.FixDiffMaxBytes)
			}
			for _, s := range tc.holds {
				if !strings.Contains(h.FixDiff, s) {
					t.Errorf("fix diff lacks %q", s)
				}
			}
			for _, s := range tc.lacks {
				if strings.Contains(h.FixDiff, s) {
					t.Errorf("fix diff holds %q", s)
				}
			}
			c := h.FixDiffCut
			if tc.want == nil {
				if c != nil {
					t.Errorf("fix diff cut = %+v for a whole diff", c)
				}
				return
			}
			if c == nil {
				t.Fatalf("no fix diff cut, want %+v", tc.want)
			}
			if !slices.Equal(c.Files, tc.want.Files) || !slices.Equal(c.ForgeCut, tc.want.ForgeCut) ||
				!slices.Equal(c.Unavailable, tc.want.Unavailable) || (c.ShownBytes < c.ReadBytes) != tc.partial ||
				c.ShownBytes != len(h.FixDiff) {
				t.Errorf("fix diff cut = %+v (diff %d bytes), want %+v, partial %v", c, len(h.FixDiff), tc.want,
					tc.partial)
			}
			if tc.partial && !strings.HasSuffix(h.FixDiff, "of vendor/gen/big.go\n") {
				t.Errorf("not cut at a hunk boundary: ...%q", h.FixDiff[max(0, len(h.FixDiff)-80):])
			}
		})
	}
}

// fixRoundHandoff is the evaluate handoff of a round after a judged one
// whose head moved, the compare answering patch or err.
func fixRoundHandoff(t *testing.T, patch string, err error) agentrun.EvaluateHandoff {
	t.Helper()
	return fixRoundHandoffWith(t, patch, err, func(*v1alpha1.PreviewVerification) {})
}

// fixRoundHandoffWith is fixRoundHandoff with the round's verification
// changed by next before it is rendered.
func fixRoundHandoffWith(t *testing.T, patch string, err error,
	next func(*v1alpha1.PreviewVerification)) agentrun.EvaluateHandoff {
	t.Helper()
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	pv1 := e.onlyVerification(name)
	pv1.Status.Phase = v1alpha1.VerificationObserved
	if err := e.c.Update(context.Background(), &pv1); err != nil {
		t.Fatal(err)
	}
	e.gh.mu.Lock()
	e.gh.patch = patch
	e.gh.mu.Unlock()
	if err != nil {
		e.gh.failNext("ComparePatch", err)
	}
	pv2 := pv1.DeepCopy()
	pv2.Name, pv2.UID = pv1.Name+"-next", types.UID("pv2-uid")
	pv2.Spec.Round, pv2.Status = 2, v1alpha1.PreviewVerificationStatus{}
	pv2.Spec.Heads[0].Revision = strings.Repeat("b", 40)
	next(pv2)
	return decodeEvaluateHandoff(t, mustHandoff(t, e.prVerifyPass(name), pv2))
}

// TestBoundFixDiffNamesAQuotedPath: git quotes a path with a non-ASCII
// character, a quote or a control character in a file's header
// ("a/caf\303\251.go"). A cut through such a file names it, as the diff
// spells it, and never the file before it, which is shown whole.
func TestBoundFixDiffNamesAQuotedPath(t *testing.T) {
	quoted := strings.Replace(fileDiff("Q", 40<<10), "diff --git a/Q b/Q",
		`diff --git "a/caf\303\251 b/x.go" "b/caf\303\251 b/x.go"`, 1)
	patch := fileDiff("api/small.go", 300) + quoted + fileDiff("z/last.go", 300)
	got, cut := boundFixDiff([]fixDiffPart{{repo: "acme/app", base: "aaaaaaa", head: "bbbbbbb", patch: patch}})
	if cut == nil || cut.ShownBytes != len(got) || cut.ShownBytes >= cut.ReadBytes {
		t.Fatalf("cut = %+v over a %d-byte diff, want it cut", cut, len(got))
	}
	if want := []string{`caf\303\251 b/x.go`, "z/last.go"}; !slices.Equal(cut.Files, want) {
		t.Errorf("files not shown in full = %q, want %q", cut.Files, want)
	}
}

// TestEvaluateHandoffNamesAComponentNewToTheRound: a head of a component
// the last round did not evaluate has no heads to compare from, so none of
// its change is shown; the handoff says so, never leaves it out silently.
func TestEvaluateHandoffNamesAComponentNewToTheRound(t *testing.T) {
	h := fixRoundHandoffWith(t, fileDiff("api/battle.go", 200), nil, func(pv *v1alpha1.PreviewVerification) {
		pv.Spec.Heads = append(pv.Spec.Heads, v1alpha1.VerificationHead{Component: "worker",
			Repository: "https://github.com/acme/worker", Revision: strings.Repeat("c", 40)})
	})
	if c := h.FixDiffCut; c == nil || !slices.Equal(c.Unavailable, []string{"acme/worker"}) ||
		c.ShownBytes != c.ReadBytes || !strings.Contains(h.FixDiff, "api/battle.go") {
		t.Errorf("fix diff cut = %+v, want acme/worker said unavailable beside the app's whole diff", c)
	}
}

// TestBoundFixDiffSeeded is a seeded property over the moved heads'
// compares (several repositories, files of any size, a commit message line
// that looks like a file's header, invisible characters, compares GitHub
// cut or that failed): the diff handed over is a prefix of the whole diff
// made visible, within FixDiffMaxBytes with the files it names; it is the
// whole diff, with no cut, only when nothing is left out; otherwise the cut
// counts the bytes shown of those read, and every file the diff does not
// show in full is named or counted.
func TestBoundFixDiffSeeded(t *testing.T) {
	for seed := range int64(300) {
		rng := rand.New(rand.NewSource(seed))
		var parts []fixDiffPart
		for r := range 1 + rng.Intn(3) {
			part := fixDiffPart{repo: fmt.Sprintf("acme/r%d", r), base: "aaaaaaa", head: "bbbbbbb"}
			switch rng.Intn(6) {
			case 0:
				part.unavailable = true
			default:
				var b strings.Builder
				b.WriteString("From 1234 Mon Sep 17 00:00:00 2001\nSubject: fix\n\ndiff --git in a message\n")
				for f := range rng.Intn(6) {
					b.WriteString(fileDiff(fmt.Sprintf("d%d/f%d\u200b.go", r, f), rng.Intn(16<<10)))
				}
				part.patch = b.String()
				if rng.Intn(4) == 0 {
					part.patch += ghclient.PatchTruncated
				}
			}
			parts = append(parts, part)
		}
		got, cut := boundFixDiff(parts)
		full, files := wholeFixDiff(parts)
		var listed int
		if cut != nil {
			for _, f := range cut.Files {
				listed += len(f) + 1
			}
		}
		if len(got)+listed > templates.FixDiffMaxBytes || !strings.HasPrefix(full, got) {
			t.Fatalf("seed %d: %d-byte diff and %d bytes of files, or not a prefix of the whole", seed, len(got),
				listed)
		}
		if cut == nil {
			if got != full || slices.ContainsFunc(parts, func(p fixDiffPart) bool {
				return p.unavailable || strings.HasSuffix(p.patch, ghclient.PatchTruncated)
			}) {
				t.Errorf("seed %d: no cut, but %d of %d bytes shown, or a compare cut or failed", seed, len(got),
					len(full))
			}
			continue
		}
		if cut.ShownBytes != len(got) || cut.ReadBytes != len(full) {
			t.Errorf("seed %d: cut %+v counts %d of %d", seed, cut, len(got), len(full))
		}
		var notShown int
		for _, f := range files {
			if f.end > len(got) && !slices.Contains(cut.Files, f.path) {
				notShown++
			}
		}
		if notShown > cut.MoreFiles {
			t.Errorf("seed %d: %d files not shown in full are neither named nor counted (%d counted)", seed,
				notShown, cut.MoreFiles)
		}
	}
}

// wholeFile is a file of the whole diff: its name and where its part ends.
type wholeFile struct {
	path string
	end  int
}

// wholeFixDiff is parts' whole diff as boundFixDiff renders it before any
// cut, read independently: each repository's header and its patch made
// visible, the forge's notice off; and every file in it, by where its part
// ends (the next file, commit or repository, or the end; a forge-cut
// repository's last file never ends within it).
func wholeFixDiff(parts []fixDiffPart) (string, []wholeFile) {
	var b strings.Builder
	var files []wholeFile
	for _, p := range parts {
		if p.unavailable {
			continue
		}
		patch, forge := strings.CutSuffix(p.patch, ghclient.PatchTruncated)
		start := b.Len()
		fmt.Fprintf(&b, "# %s %s...%s\n%s\n", p.repo, p.base, p.head, untrusted.Escape(strings.TrimRight(patch,
			"\n")))
		text := b.String()[start:]
		var mine []wholeFile
		at := 0
		for line := range strings.SplitAfterSeq(text, "\n") {
			if strings.HasPrefix(line, "From ") && len(mine) > 0 && mine[len(mine)-1].end < 0 {
				mine[len(mine)-1].end = start + at
			}
			if rest, ok := strings.CutPrefix(line, "diff --git a/"); ok && strings.Contains(rest, " b/") {
				if len(mine) > 0 && mine[len(mine)-1].end < 0 {
					mine[len(mine)-1].end = start + at
				}
				name := strings.TrimSuffix(rest[strings.LastIndex(rest, " b/")+3:], "\n")
				if len(parts) > 1 {
					name = p.repo + ": " + name
				}
				mine = append(mine, wholeFile{path: name, end: -1})
			}
			at += len(line)
		}
		for i := range mine {
			if mine[i].end < 0 {
				mine[i].end = b.Len()
				if forge {
					mine[i].end = 1 << 30
				}
			}
		}
		files = append(files, mine...)
	}
	return b.String(), files
}
