// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"testing/quick"
)

// Real `go build -o maulcheck .` outputs (go1.26.10, the relay's compile
// environment: GOPROXY=off, GOFLAGS=-mod=readonly, GOENV=off, a workspace
// of the program's directory and the tree), with the shapes the go command
// prints when the compiler or the linker is killed or the disk is full.
const (
	goOutUndefined = "# maulcheck\n./main.go:3:26: undefined: pkg.New\n"
	goOutTypeError = "# maulcheck\n./main.go:3:29: cannot use pkg.Old() (value of type int) as string value in " +
		"variable declaration\n"
	goOutSyntax    = "# maulcheck\n./main.go:3:1: syntax error: unexpected EOF, expected }\n"
	goOutInterface = "# maulcheck\n./main.go:6:16: cannot use T{} (value of struct type T) as I value in argument " +
		"to f: T does not implement I (missing method M)\n\t\thave m()\n\t\twant M()\n"
	goOutTooMany = "# maulcheck\n./main.go:3:14: undefined: a\n./main.go:3:19: undefined: b\n" +
		"./main.go:3:24: undefined: c\n./main.go:3:29: undefined: d\n./main.go:3:34: undefined: e\n" +
		"./main.go:3:39: undefined: f\n./main.go:3:44: undefined: g\n./main.go:3:49: undefined: h\n" +
		"./main.go:3:54: undefined: i\n./main.go:3:59: undefined: j\n./main.go:3:59: too many errors\n"
	goOutTwoFiles = "# maulcheck\n./main.go:7:2: undefined: bb.Parse\n./helper.go:4:9: undefined: bb.Count\n"
	// The go command could not load the program's imports: no compiler
	// ran, so nothing about the tree.
	goOutNewPackage = "main.go:2:8: cannot find module providing package example.com/tree/newpkg: import lookup " +
		"disabled by -mod=readonly\n"
	goOutNoCache = "main.go:2:8: github.com/google/uuid@v1.6.0: module lookup disabled by GOPROXY=off\n"
	goOutNotStd  = "main.go:2:8: package fmtx is not in std (/usr/local/go/src/fmtx)\n"
	goOutSumMiss = "main.go:4:2: missing go.sum entry for module providing package golang.org/x/mod/modfile " +
		"(imported by maulcheck); to add:\n\tgo get maulcheck\n"
	// The compiler refused a file of the tree, not the program's.
	goOutTreeFile = "# example.com/tree/pkg\n../tree/pkg/pkg.go:3:25: cannot use \"x\" (untyped string constant) " +
		"as int value in return statement\n"
	goOutKilled     = "# maulcheck\ngo build maulcheck: /usr/local/go/pkg/tool/linux_amd64/compile: signal: killed\n"
	goOutLinkKilled = "# maulcheck\n/usr/local/go/pkg/tool/linux_amd64/link: signal: killed\n"
	goOutDiskFull   = "# maulcheck\n./main.go:1:1: open /work/home/.cache/go-build/ab/abcd-d: no space left on device\n"
	goOutToolchain  = "go: example.com/tree@v0.0.0 requires go >= 1.99 (running go 1.26.10; GOTOOLCHAIN=local)\n"
	goOutNoImport   = "# maulcheck\n./main.go:4:2: could not import example.com/tree/pkg (open " +
		"/work/home/.cache/go-build/12/1234-d: no such file or directory)\n"
	goOutICE = "# maulcheck\n./main.go:5:2: internal compiler error: panic: runtime error\n"
	goOutOOM = "# maulcheck\nfatal error: runtime: out of memory\n"
)

// TestCompilerRefused: a compile is the compiler's refusal of the program
// (CauseCompile, no base comparison) only when every line the go command
// printed is the program package's header, a diagnostic located in one of
// the program's own files, or its continuation, with at least one
// diagnostic and no environmental marker. A load failure (a module, a
// package, a cache maul did not have), a diagnostic in the tree's own
// files, a killed compiler or linker, a full disk, a toolchain refusal and
// output that was cut are no evidence about the tree (SPEC-S2 S2-20b).
func TestCompilerRefused(t *testing.T) {
	main := []string{"main.go"}
	tests := []struct {
		name   string
		output string
		files  []string
		want   bool
	}{
		{"an undefined name", goOutUndefined, main, true},
		{"a type error", goOutTypeError, main, true},
		{"a syntax error", goOutSyntax, main, true},
		{"a diagnostic with continuation lines", goOutInterface, main, true},
		{"too many errors", goOutTooMany, main, true},
		{"diagnostics in two of the program's files", goOutTwoFiles, []string{"main.go", "helper.go"}, true},
		{"a diagnostic in a file not the program's", goOutTwoFiles, main, false},
		{"without the package header", "./main.go:3:26: undefined: pkg.New\n", main, false},
		{"a header and no diagnostic", "# maulcheck\n", main, false},
		{"empty output", "", main, false},
		{"a package not found in the modules", goOutNewPackage, main, false},
		{"a module not in the cache", goOutNoCache, main, false},
		{"a package not in std", goOutNotStd, main, false},
		{"a missing go.sum entry", goOutSumMiss, main, false},
		{"a diagnostic in the tree's own file", goOutTreeFile, main, false},
		{"the compiler killed", goOutKilled, main, false},
		{"the linker killed", goOutLinkKilled, main, false},
		{"the disk full", goOutDiskFull, main, false},
		{"a toolchain refusal", goOutToolchain, main, false},
		{"an import the cache lost", goOutNoImport, main, false},
		{"an internal compiler error", goOutICE, main, false},
		{"out of memory", goOutOOM, main, false},
		{"another package's header", strings.Replace(goOutUndefined, "# maulcheck", "# other", 1), main, false},
		{"a diagnostic then a go line", goOutUndefined + "go: warning: something\n", main, false},
		{"a continuation before any diagnostic", "# maulcheck\n\thave m()\n" + goOutUndefined[12:], main, false},
		{"a path in a subdirectory", "# maulcheck\n./sub/main.go:3:26: undefined: x\n", main, false},
		{"a file name only sharing a suffix", "# maulcheck\n./xmain.go:3:26: undefined: x\n", main, false},
		{"a diagnostic without a column", "# maulcheck\n./main.go:3: undefined: x\n", main, false},
		{"no program files", goOutUndefined, nil, false},
		{"a carriage return in a line", "# maulcheck\r\n./main.go:3:26: undefined: pkg.New\r\n", main, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := compilerRefused(tt.output, tt.files); got != tt.want {
				t.Errorf("compilerRefused(%q, %q) = %v, want %v", tt.output, tt.files, got, tt.want)
			}
		})
	}
}

// environmentalMarkers are text the go command prints when a compile ended
// for a reason outside the program: any of them, anywhere, leaves a
// compile without a cause.
var environmentalMarkers = []string{
	"signal: killed", "signal: segmentation fault", "out of memory", "cannot allocate memory",
	"no space left on device", "disk quota exceeded", "too many open files", "resource temporarily unavailable",
	"internal compiler error", "module lookup disabled", "import lookup disabled", "missing go.sum entry",
	"GOPROXY", "toolchain", "requires go >=", "go: downloading", "dial tcp", "could not import",
	"no such file or directory", "permission denied", "read-only file system", "input/output error",
	"fatal error:", "checksum mismatch", "verifying ",
}

// TestCompilerRefusedProperty: whatever lines a compile printed, output
// carrying an environmental marker never classifies as the compiler's
// refusal, and neither does output with a diagnostic located in a file not
// the program's; output of only the header and the program's own
// diagnostics always does.
func TestCompilerRefusedProperty(t *testing.T) {
	files := []string{"main.go", "helper.go"}
	own := []string{"./main.go:3:26: undefined: pkg.New", "./helper.go:12:4: x declared and not used",
		"main.go:7:1: syntax error: non-declaration statement outside function body",
		"./main.go:6:16: cannot use T{} as I value", "\t\thave m()", "\t\twant M()"}
	foreign := []string{"../tree/pkg/pkg.go:3:25: cannot use \"x\" as int value", "./other.go:1:1: undefined: y",
		"/work/tree/bb.go:9:2: undefined: z", "./sub/main.go:2:2: undefined: w", "vendor/x/x.go:1:1: undefined: v"}
	prop := func(seed uint64) bool {
		rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
		n := rng.IntN(8)
		lines := make([]string, 0, n+2)
		lines = append(lines, "# maulcheck", own[rng.IntN(2)])
		for range n {
			lines = append(lines, own[rng.IntN(len(own))])
		}
		clean := strings.Join(lines, "\n") + "\n"
		if !compilerRefused(clean, files) {
			t.Logf("own diagnostics not classified: %q", clean)
			return false
		}
		// An environmental marker, in a line of its own or inside a
		// diagnostic's message.
		marker := environmentalMarkers[rng.IntN(len(environmentalMarkers))]
		withMarker := slices.Insert(slices.Clone(lines), 1+rng.IntN(len(lines)), "./main.go:1:1: open x: "+marker)
		if rng.IntN(2) == 0 {
			withMarker = slices.Insert(slices.Clone(lines), 1+rng.IntN(len(lines)), marker)
		}
		if out := strings.Join(withMarker, "\n") + "\n"; compilerRefused(out, files) {
			t.Logf("an environmental marker classified as compile: %q", out)
			return false
		}
		withForeign := slices.Insert(slices.Clone(lines), 1+rng.IntN(len(lines)), foreign[rng.IntN(len(foreign))])
		if out := strings.Join(withForeign, "\n") + "\n"; compilerRefused(out, files) {
			t.Logf("a foreign diagnostic classified as compile: %q", out)
			return false
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatal(err)
	}
}
