// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/untrusted"
)

const (
	notWhole   = "You have not seen the whole fix."
	noneProven = "Finding no special-casing in what is shown establishes nothing about the rest"
	fixSection = "### The fix since the last round"
)

// TestFixDiffViewSaysWhatItLeavesOutSeeded is a seeded property over the
// fix diff (any length up to past its bound, invisible characters and
// backticks included) and what the controller says it leaves out (code
// review 2026-11-18, agent-prompts 8): the evaluate prompt either quotes
// the whole diff, made visible, and says nothing of a cut, or states, in
// maul's words before the quoted diff, that the evaluator has not seen the
// whole fix and that finding no special-casing proves nothing. A diff that
// could not be read is said to be so, never a missing section.
func TestFixDiffViewSaysWhatItLeavesOutSeeded(t *testing.T) {
	for seed := range int64(200) {
		diff, cut := randomFixDiff(rand.New(rand.NewSource(seed)))
		p := testEvaluatePrompt()
		p.Round, p.PriorRound, p.FixDiff, p.FixDiffCut = 2, 1, diff, cut
		got, err := RenderEvaluatePrompt(p)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if why := fixDiffPromptFault(diff, cut, got); why != "" {
			t.Errorf("seed %d: a %d-byte diff, cut %+v: %s", seed, len(diff), cut, why)
		}
	}
}

// randomFixDiff is a fix diff of any length up to past its bound (empty
// one time in five), and what a controller says it leaves out.
func randomFixDiff(rng *rand.Rand) (string, FixDiffCut) {
	alphabet := []string{"a", "b", "\n", " ", "`", "\u200b", "\u202e", "é", "\U0001F600", "@@ ",
		"diff --git a/x b/x\n"}
	var b strings.Builder
	for n := rng.Intn(5 * FixDiffMaxBytes / 2); b.Len() < n; {
		b.WriteString(alphabet[rng.Intn(len(alphabet))])
	}
	if rng.Intn(5) == 0 {
		b.Reset()
	}
	diff := b.String()
	var cut FixDiffCut
	if rng.Intn(2) == 0 {
		cut.ShownBytes = len(untrusted.Escape(diff))
		cut.ReadBytes = cut.ShownBytes + rng.Intn(3)*1000
	}
	if rng.Intn(4) == 0 {
		cut.ForgeCut = []string{"acme/api"}
	}
	if rng.Intn(4) == 0 {
		cut.Files = []string{"a/x.go", strings.Repeat("p", 300)}
	}
	if rng.Intn(4) == 0 {
		cut.MoreFiles = 1 + rng.Intn(3)
	}
	if rng.Intn(4) == 0 {
		cut.Unavailable = []string{"acme/web"}
	}
	return diff, cut
}

// fixDiffPromptFault is why got, the evaluate prompt of diff and cut, does
// not hold the property; "" when it does.
func fixDiffPromptFault(diff string, cut FixDiffCut, got string) string {
	flat := strings.Join(strings.Fields(got), " ")
	blank := strings.TrimSpace(diff) == ""
	switch whole := cut.whole() && len(untrusted.Escape(diff)) <= FixDiffMaxBytes; {
	case blank && cut.whole():
		if strings.Contains(got, fixSection) {
			return "a fix section with no diff and nothing left out"
		}
	case whole:
		if !strings.Contains(got, strings.Trim(untrusted.Escape(diff), "\n")) || strings.Contains(flat, notWhole) {
			return "a whole diff not quoted whole, or said to be cut"
		}
	case !strings.Contains(flat, notWhole) || !strings.Contains(flat, noneProven):
		return "the prompt does not say the fix was not seen whole"
	default:
		// The note is maul's: in the section, before the quoted diff.
		section := strings.Index(got, fixSection)
		note := strings.Index(got, "**"+notWhole+"**")
		if section < 0 || note < section || !blank && note > section+strings.Index(got[section:], "```") {
			return "the note is not maul's own, before the quoted diff"
		}
	}
	return ""
}

// TestFixDiffViewBounds: the files and repositories a note names are
// bounded, counted and made visible.
func TestFixDiffViewBounds(t *testing.T) {
	files := make([]string, 0, 100)
	for range 100 {
		files = append(files, strings.Repeat("f", 400)+"\u202e")
	}
	repos := make([]string, 12)
	for i := range repos {
		repos[i] = "acme/r"
	}
	_, note, list := fixDiffView("d", FixDiffCut{ShownBytes: 1, ReadBytes: 2, Files: files, Unavailable: repos})
	if len(list) > FixDiffFilesMaxBytes || strings.ContainsRune(list, '\u202e') || list == "" {
		t.Errorf("files list of %d bytes, or not made visible", len(list))
	}
	shown := strings.Count(list, "\n") + 1
	if want := "and " + strconv.Itoa(len(files)-shown) + " more, are not shown in full"; !strings.Contains(note, want) {
		t.Errorf("note %q, want %q", note, want)
	}
	if !strings.Contains(note, ", 4 more ") || strings.Count(note, "`acme/r`") != fixDiffReposMax {
		t.Errorf("note %q does not bound the repositories", note)
	}
}
