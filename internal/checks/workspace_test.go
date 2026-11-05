// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import (
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

// The surfaces a check is validated for: a preview slot, a cli workspace
// and a go library workspace.
var (
	slotCtx = Context{Preview: &Preview{}}
	cliCtx  = Context{Workspace: &Workspace{Binary: "/work/bin/bbrender"}}
	libCtx  = Context{Workspace: &Workspace{Language: LanguageGo}}
)

func runCheck(body string) string {
	return `{"id":"o.c","outcome":"o","form":"run","run":{` + body + `}}`
}

// Workspace checks of DESIGN 2.4 and 2.5.
const (
	// cliExec runs the built binary and asserts on its stdout.
	cliExec = `{"id":"onsets.exec","outcome":"kick-onsets","form":"exec","exec":{
	 "argv":["{{bin}}","--bpm","120","{{tree}}/testdata/kick.json"],"timeout":"30s",
	 "expect":{"exit":0,"stdoutContains":"wrote 4 beats"}}}`
	// cliRun is a program that invokes the binary and analyses its output.
	cliRun = `{"id":"onsets.wav","outcome":"kick-onsets","form":"run","run":{"language":"go",
	 "files":[{"path":"main.go","content":"package main\n\nimport \"os/exec\"\n\nfunc main() {\n` +
		`\t_ = exec.Command(\"{{bin}}\", \"--bpm\", \"120\").Run()\n\tprintln(\"onsets 500\")\n}\n"}],
	 "args":["{{run}}"],"expect":{"exit":0,"stdoutContains":"onsets 500"}}}`
	// libRun is a consumer program calling the library's public API.
	libRun = `{"id":"parse.too-long","outcome":"parse-rejects-long","form":"run","run":{"language":"go",
	 "files":[{"path":"main.go","content":"package main\n\nfunc main() { println(true) }\n"}],
	 "expect":{"exit":0,"stdoutJSON":{"":true}}}}`
	goMain  = `"files":[{"path":"main.go","content":"package main\nfunc main() { println(\"{{bin}}\") }\n"}]`
	libMain = `"files":[{"path":"main.go","content":"package main\nfunc main() { println(1) }\n"}]`
	okOut   = `"expect":{"stdoutContains":"done"}`
)

// TestS1DigestsUnchanged pins the S1 demo checks' digests as 0.13.13
// computed them: the run form and the surface context add nothing to an
// S1 check's canonical bytes.
func TestS1DigestsUnchanged(t *testing.T) {
	want := map[string]string{
		httpOrder:   "sha256:4f3526b57a135b841eda6f33f3c79d9118a7741936b6103b47c3aa0b4d57735d",
		httpRate:    "sha256:46a39f01a2c11797bd53a721b9e52f3397eafe46029c6437ac9c09ff2e699119",
		browserRow:  "sha256:b8c283d5ce6f737d406761853b46629102222218bea861004a781046eb40dfbf",
		browserGrid: "sha256:85d4e2b01331b452168dfdeb29e359e7c1806977e5f0d1a1ca980dfb2421d854",
		execWS:      "sha256:46da0cf48c19b72af924ab86b1943e0072ec60fbcb60793dc37c5a96479f13c8",
	}
	for s, d := range want {
		c := mustParse(t, s)
		if got := Digest(c); got != d {
			t.Errorf("%s: Digest = %s, want %s", c.ID, got, d)
		}
	}
}

func TestWorkspaceChecksAreValid(t *testing.T) {
	for _, tc := range []struct {
		name  string
		check string
		ctx   Context
	}{
		{"cli exec of the binary", cliExec, cliCtx},
		{"cli run invoking the binary", cliRun, cliCtx},
		{"library run", libRun, libCtx},
		{"library run reading the tree", runCheck(`"language":"go",` + libMain + `,"args":["{{tree}}/x.json"],` +
			okOut), libCtx},
		{"run with a timeout", runCheck(`"language":"go",` + libMain + `,"timeout":"2m",` + okOut), libCtx},
		{"run in a subdirectory", runCheck(`"language":"go","files":[{"path":"main.go","content":"package main"},
			{"path":"wav/wav.go","content":"package wav"}],` + okOut), libCtx},
	} {
		c := mustParse(t, tc.check)
		if err := Validate(c, tc.ctx); err != nil {
			t.Errorf("%s: Validate = %v", tc.name, err)
		}
		if again := mustParse(t, string(Canonical(c))); string(Canonical(again)) != string(Canonical(c)) {
			t.Errorf("%s: Canonical is not idempotent", tc.name)
		}
	}
}

// TestS1ChecksUnchangedInSlot: every S1 demo check still validates for a
// preview slot, and none for a workspace.
func TestS1ChecksUnchangedInSlot(t *testing.T) {
	for _, s := range []string{httpOrder, httpRate, browserRow, browserGrid, execWS} {
		c := mustParse(t, s)
		if err := Validate(c, slotCtx); err != nil {
			t.Errorf("%s: slot Validate = %v", c.ID, err)
		}
		for _, ctx := range []Context{cliCtx, libCtx} {
			if Validate(c, ctx) == nil {
				t.Errorf("%s: validated for workspace %+v", c.ID, *ctx.Workspace)
			}
		}
	}
}

func runWorkspaceRefusals(t *testing.T, rule Rule, cases []struct {
	name  string
	check string
	ctx   Context
}) {
	t.Helper()
	for _, tc := range cases {
		c, err := Parse([]byte(tc.check))
		if err != nil {
			t.Errorf("%s: Parse: %v", tc.name, err)
			continue
		}
		err = Validate(c, tc.ctx)
		var r *Refusal
		if !errors.As(err, &r) || r.Rule != rule {
			t.Errorf("%s: Validate = %v, want rule %s", tc.name, err, rule)
		}
	}
}

func TestRefuseOffSurface(t *testing.T) {
	runWorkspaceRefusals(t, RuleOffSurface, []struct {
		name  string
		check string
		ctx   Context
	}{
		{"http in a cli", httpOrder, cliCtx},
		{"browser in a cli", browserRow, cliCtx},
		{"http in a library", httpRate, libCtx},
		{"browser in a library", browserGrid, libCtx},
		{"exec in a library", execCheck(`"argv":["{{tree}}/run.sh"],` + okOut), libCtx},
		{"run in a slot", libRun, slotCtx},
	})
}

func TestRefuseWorkspaceGrammar(t *testing.T) {
	big := strings.Repeat("x", 300)
	runWorkspaceRefusals(t, RuleGrammar, []struct {
		name  string
		check string
		ctx   Context
	}{
		{"no context", libRun, Context{}},
		{"both surfaces", libRun, Context{Preview: &Preview{}, Workspace: &Workspace{Language: LanguageGo}}},
		{"empty workspace", libRun, Context{Workspace: &Workspace{}}},
		{"cli and library at once", cliRun, Context{Workspace: &Workspace{Binary: "/work/bin/x",
			Language: LanguageGo}}},
		{"workspace language not supported", runCheck(`"language":"node",` + libMain + `,` + okOut),
			Context{Workspace: &Workspace{Language: "node"}}},
		{"run language not supported", runCheck(`"language":"python",` + libMain + `,` + okOut), cliCtx},
		{"run language not the library's", runCheck(`"language":"python",` + libMain + `,` + okOut), libCtx},
		{"run without language", runCheck(libMain + `,` + okOut), libCtx},
		{"run without files", runCheck(`"language":"go",` + okOut), libCtx},
		{"run with a go.mod", runCheck(`"language":"go","files":[{"path":"main.go","content":"package main"},
			{"path":"go.mod","content":"module evil"}],` + okOut), libCtx},
		{"run with a go.sum", runCheck(`"language":"go","files":[{"path":"main.go","content":"package main"},
			{"path":"go.sum","content":""}],` + okOut), libCtx},
		{"run with a non-go file", runCheck(`"language":"go","files":[{"path":"main.go","content":"package main"},
			{"path":"x.sh","content":"echo"}],` + okOut), libCtx},
		{"run with nine files", runCheck(`"language":"go","files":[` + nineGoFiles() + `],` + okOut), libCtx},
		{"run with a bad path", runCheck(`"language":"go","files":[{"path":"../main.go","content":"package main"}],` +
			okOut), libCtx},
		{"run with seventeen args", runCheck(`"language":"go",` + libMain + `,"args":[` +
			strings.Repeat(`"a",`, 16) + `"a"],` + okOut), libCtx},
		{"run arg too long", runCheck(`"language":"go",` + libMain + `,"args":["` + big + `"],` + okOut), libCtx},
		{"run timeout too long", runCheck(`"language":"go",` + libMain + `,"timeout":"6m",` + okOut), libCtx},
		{"run unknown placeholder", runCheck(`"language":"go",` + libMain + `,"args":["{{home}}"],` + okOut), libCtx},
		{"bin in a library arg", runCheck(`"language":"go",` + libMain + `,"args":["{{bin}}"],` + okOut), libCtx},
		{"bin in a library program", runCheck(`"language":"go",` + goMain + `,` + okOut), libCtx},
		{"bin in a slot exec", execCheck(`"argv":["{{bin}}"],` + okOut), slotCtx},
		{"tree in a slot exec", execCheck(`"argv":["python3","{{tree}}/a.py"],` + okOut), slotCtx},
		{"run exit out of range", runCheck(`"language":"go",` + libMain + `,"expect":{"exit":256,
			"stdoutContains":"x"}`), libCtx},
	})
}

func nineGoFiles() string {
	fs := make([]string, 0, 9)
	for i := range 9 {
		fs = append(fs, fmt.Sprintf(`{"path":"f%d.go","content":"package main"}`, i))
	}
	return strings.Join(fs, ",")
}

func TestRefuseWorkspaceNotSubstantive(t *testing.T) {
	runWorkspaceRefusals(t, RuleNotSubstantive, []struct {
		name  string
		check string
		ctx   Context
	}{
		{"cli exec not of the binary", execCheck(`"argv":["python3","client.py"],` + okOut), cliCtx},
		{"cli exec of the binary's path", execCheck(`"argv":["/work/bin/bbrender"],` + okOut), cliCtx},
		{"cli exec binary not first", execCheck(`"argv":["env","{{bin}}"],` + okOut), cliCtx},
		{"cli exec exit 0 only", execCheck(`"argv":["{{bin}}","--bpm","120"],"expect":{"exit":0}`), cliCtx},
		{"cli exec no expectation", execCheck(`"argv":["{{bin}}"],"expect":{}`), cliCtx},
		{"cli exec whole stdout present", execCheck(`"argv":["{{bin}}"],"expect":{"stdoutJSON":{"":
			{"$present":true}}}`), cliCtx},
		{"cli run not invoking the binary", runCheck(`"language":"go",` + libMain + `,` + okOut), cliCtx},
		{"cli run binary only in args", runCheck(`"language":"go",` + libMain + `,"args":["{{bin}}"],` + okOut),
			cliCtx},
		{"cli run exit 0 only", runCheck(`"language":"go",` + goMain + `,"expect":{"exit":0}`), cliCtx},
		{"library run exit 0 only", runCheck(`"language":"go",` + libMain + `,"expect":{"exit":0}`), libCtx},
		{"library run no expectation", runCheck(`"language":"go",` + libMain + `,"expect":{}`), libCtx},
	})
}

// TestCliExecRefusalNamesTheBinary: a cli exec of anything but the built
// binary, a trivial program included, is told to run {{bin}}, never to run
// a client against a preview the workspace does not have.
func TestCliExecRefusalNamesTheBinary(t *testing.T) {
	for _, argv := range []string{`["echo","done"]`, `["sh","-c","true"]`, `["python3","client.py"]`} {
		err := Validate(mustParse(t, execCheck(`"argv":`+argv+`,`+okOut)), cliCtx)
		var r *Refusal
		if !errors.As(err, &r) || r.Rule != RuleNotSubstantive {
			t.Errorf("%s: Validate = %v, want rule %s", argv, err, RuleNotSubstantive)
			continue
		}
		if !strings.Contains(r.Reason, BinPlaceholder) || strings.Contains(r.Reason, "preview") {
			t.Errorf("%s: reason %q does not name %s, or names a preview", argv, r.Reason, BinPlaceholder)
		}
	}
}

func TestRefuseWorkspaceRunToken(t *testing.T) {
	runWorkspaceRefusals(t, RuleRunToken, []struct {
		name  string
		check string
		ctx   Context
	}{
		{"run stdout includes", runCheck(`"language":"go",` + libMain + `,"expect":{"stdoutJSON":{"/a":
			{"$includes":3}}}`), libCtx},
		{"cli exec stdout order", execCheck(`"argv":["{{bin}}"],"expect":{"stdoutJSON":{"/a":
			{"$order":["x","y"]}}}`), cliCtx},
	})
}

// TestRefuseRunTooLarge: a run check, its program included, keeps the
// saved-check bound of MaxCanonicalBytes.
func TestRefuseRunTooLarge(t *testing.T) {
	prog := "package main\n// " + strings.Repeat("x", MaxCanonicalBytes) + "\nfunc main() {}\n"
	runWorkspaceRefusals(t, RuleTooLarge, []struct {
		name  string
		check string
		ctx   Context
	}{
		{"run program over the bound", runCheck(fmt.Sprintf(`"language":"go","files":[{"path":"main.go",
			"content":%q}],`, prog) + okOut), libCtx},
	})
	// Just under the bound is admitted.
	small := mustParse(t, runCheck(`"language":"go","files":[{"path":"main.go","content":"package main"}],`+okOut))
	pad := MaxCanonicalBytes - len(Canonical(small))
	small.Run.Files[0].Content += strings.Repeat("x", pad)
	if n := len(Canonical(small)); n != MaxCanonicalBytes {
		t.Fatalf("padded check is %d bytes", n)
	}
	if err := Validate(small, libCtx); err != nil {
		t.Errorf("a run check of exactly %d bytes refused: %v", MaxCanonicalBytes, err)
	}
}

func TestWritesRunDataRun(t *testing.T) {
	for _, tc := range []struct {
		name  string
		check string
		want  bool
	}{
		{"run token in args", cliRun, true},
		{"run token in a file", runCheck(`"language":"go","files":[{"path":"main.go",
			"content":"package main\nconst run = \"{{run}}\"\n"}],` + okOut), true},
		{"bin and tree only", runCheck(`"language":"go",` + goMain + `,"args":["{{tree}}/x"],` + okOut), false},
		{"exec of the binary with a tree file", cliExec, false},
		{"exec of the binary with the run token", execCheck(`"argv":["{{bin}}","--name","{{run}}"],` + okOut), true},
	} {
		if got := WritesRunData(mustParse(t, tc.check)); got != tc.want {
			t.Errorf("%s: WritesRunData = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestParseRunIsStrict(t *testing.T) {
	for _, s := range []string{
		runCheck(`"language":"go",` + libMain + `,"side":"base",` + okOut),
		runCheck(`"language":"go",` + libMain + `,"expect":{"stdout":"x"}`),
	} {
		if _, err := Parse([]byte(s)); err == nil {
			t.Errorf("Parse(%s) accepted an unknown field", s)
		}
	}
}

// TestRunTwoBodies: a run check with a second body is refused.
func TestRunTwoBodies(t *testing.T) {
	s := `{"id":"o.c","outcome":"o","form":"run","run":{"language":"go",` + libMain + `,` + okOut +
		`},"exec":{"argv":["{{bin}}"],` + okOut + `}}`
	runWorkspaceRefusals(t, RuleGrammar, []struct {
		name  string
		check string
		ctx   Context
	}{{"run and exec", s, libCtx}, {"run and exec in a cli", s, cliCtx}})
}

// genWorkspace is a random check of any form for a random workspace:
// exec argv starting with {{bin}} or something else, run programs that
// name {{bin}} or not, http and browser bodies.
type genWorkspace struct {
	c   Check
	ctx Context
}

func (genWorkspace) Generate(r *rand.Rand, _ int) reflect.Value {
	ctx := cliCtx
	if r.Intn(2) == 0 {
		ctx = libCtx
	}
	c := Check{ID: "o." + word(r), Outcome: word(r)}
	argv0 := []string{BinPlaceholder, "python3", "/work/bin/bbrender", "{{tree}}/run", "go", "sh",
		"x" + BinPlaceholder}[r.Intn(7)]
	content := []string{"package main\nfunc main() { println(\"{{bin}}\") }\n",
		"package main\nfunc main() { println(1) }\n", "package main // {{run}}\n"}[r.Intn(3)]
	expect := []ExecExpect{{StdoutContains: word(r)}, {StdoutContains: "{{run}}"}, {Exit: new(int32)}, {}}[r.Intn(4)]
	switch r.Intn(4) {
	case 0:
		c.Form = FormExec
		c.Exec = &ExecCheck{Argv: []string{argv0, word(r)}, Expect: expect}
	case 1:
		c.Form = FormRun
		c.Run = &RunCheck{Language: LanguageGo, Files: []File{{Path: "main.go", Content: content}},
			Args: []string{[]string{word(r), BinPlaceholder, "{{run}}"}[r.Intn(3)]}, Expect: expect}
	case 2:
		c = mustParseNoT(httpOrder)
	default:
		c = mustParseNoT(browserRow)
	}
	return reflect.ValueOf(genWorkspace{c, ctx})
}

func mustParseNoT(s string) Check {
	c, err := Parse([]byte(s))
	if err != nil {
		panic(err)
	}
	return c
}

// TestWorkspaceCheckRunsTheSurface: a seeded property. A workspace check
// never validates unless it is a run form or, in a cli, an exec whose
// argv[0] is exactly {{bin}}; a cli run's program names {{bin}}; a library
// check is a run; every one asserts on content.
func TestWorkspaceCheckRunsTheSurface(t *testing.T) {
	accepted := map[Form]int{}
	prop := func(g genWorkspace) bool {
		if Validate(g.c, g.ctx) != nil {
			return true
		}
		accepted[g.c.Form]++
		cli := g.ctx.Workspace.Binary != ""
		var expect ExecExpect
		switch {
		case g.c.Form == FormExec && cli && g.c.Exec.Argv[0] == BinPlaceholder:
			expect = g.c.Exec.Expect
		case g.c.Form == FormRun && (!cli || filesName(g.c.Run.Files, BinPlaceholder)):
			expect = g.c.Run.Expect
		default:
			t.Logf("accepted %s for %+v", Canonical(g.c), *g.ctx.Workspace)
			return false
		}
		if expect.StdoutContains == "" && !contentJSON(expect.StdoutJSON) {
			t.Logf("accepted without a content assertion: %s", Canonical(g.c))
			return false
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(1000)); err != nil {
		t.Fatal(err)
	}
	// The generator must reach both admitted forms, or the property is
	// vacuous.
	if accepted[FormExec] == 0 || accepted[FormRun] == 0 {
		t.Fatalf("accepted %v: the generator never reached an admitted exec and run", accepted)
	}
}

func filesName(fs []File, s string) bool {
	for _, f := range fs {
		if strings.Contains(f.Content, s) {
			return true
		}
	}
	return false
}
