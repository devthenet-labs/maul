// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// A run program (SPEC-S2 Q25, the run verb and the run check form) is a
// small Go consumer the agent writes, compiled and run against one side's
// tree. maul writes its module itself, so the program can only use the real
// tree: beside the agent's .go files, a go.mod naming module maulcheck at
// the pristine tree's go version; then `go work init . <WorkTree>` makes a
// workspace of the program's directory and the side's writable tree, whose
// go.mod, go.sum and replaces are the module graph's exactly as the tree's
// own build left them (its go line included, which a build may raise), so
// the go command writes go.work's go line itself.
//
// This is SP-8's amended Q25 by another route: SP-8 rendered the tree's
// go.mod with its module line replaced and copied the tree's go.sum beside
// it, but a real tree's go.sum is past the 64 KiB a request's files may
// hold (toolwire.MaxProgramBytes); a workspace reads the tree's go.mod and
// go.sum where the build left them, and gives the same graph (the go-spew
// case included: the tree is a main module, as in its own build).
//
// The compile is steps of its own (go work init, then go build, both under
// compileEnv: no module network, the module files read-only, no user go env
// file), then the binary runs in the same scratch directory under the
// run's token. A compile that fails, whatever
// the compiler said (an undefined name at base, a package not in std, no
// module providing an import), is an error: n/a at the base, never a fail.
// A tree with a vendor directory, a replace leaving the tree, or a go.mod
// maul cannot read refuses the run by name (ReasonVendoredTree,
// ReasonReplaceOutsideTree, ReasonNoModule), never widening the network.

// consumerBinary is the compiled program, in its scratch directory.
const consumerBinary = "maulcheck"

// consumerModule is the program's module path.
const consumerModule = "maulcheck"

// maxGoModBytes bounds the tree's go.mod maul reads.
const maxGoModBytes = 1 << 20

// consumer is the module maul writes beside a run program.
type consumer struct {
	goMod []byte
}

// renderConsumer reads the pristine tree at treeDir (never following a link
// out of it) and renders the program's go.mod; workTree is where the side's
// sandbox holds its writable copy, which every directory replace must stay
// under.
func renderConsumer(treeDir, workTree string) (consumer, error) {
	root, err := os.OpenRoot(treeDir)
	if err != nil {
		return consumer{}, fmt.Errorf("%s: the tree cannot be read", ReasonNoModule)
	}
	defer func() { _ = root.Close() }()
	if _, err := root.Lstat("vendor"); err == nil {
		return consumer{}, fmt.Errorf("%s: the tree vendors its modules, which a run program cannot build against",
			ReasonVendoredTree)
	}
	data, err := readBounded(root, "go.mod", maxGoModBytes)
	if err != nil {
		return consumer{}, fmt.Errorf("%s: the tree has no go.mod maul can read", ReasonNoModule)
	}
	f, err := modfile.Parse("go.mod", data, nil)
	if err != nil || f.Module == nil || f.Module.Mod.Path == "" || f.Module.Mod.Path == consumerModule {
		return consumer{}, fmt.Errorf("%s: the tree's go.mod does not name a module maul can build against",
			ReasonNoModule)
	}
	for _, rep := range f.Replace {
		if rep.New.Version != "" {
			continue // a module replaced by a module: the network's, as in the build
		}
		p := rep.New.Path
		if filepath.IsAbs(p) || path.IsAbs(p) {
			return consumer{}, fmt.Errorf("%s: %s is replaced by an absolute path", ReasonReplaceOutsideTree,
				rep.Old.Path)
		}
		joined := path.Join(workTree, filepath.ToSlash(p))
		if joined != workTree && !strings.HasPrefix(joined, workTree+"/") {
			return consumer{}, fmt.Errorf("%s: %s is replaced by a directory outside the tree",
				ReasonReplaceOutsideTree, rep.Old.Path)
		}
	}
	mod := "module " + consumerModule + "\n"
	if f.Go != nil {
		mod += "\ngo " + f.Go.Version + "\n"
	}
	return consumer{goMod: []byte(mod)}, nil
}

// readBounded reads name under root, a regular file of at most limit bytes.
func readBounded(root *os.Root, name string, limit int64) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil, fs.ErrInvalid
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("too large")
	}
	return b, nil
}

// errSubdirFile is a run program file below the program's directory (U6).
var errSubdirFile = errors.New("a run program's files sit at its top, never in a subdirectory")

// program is a run program ready to compile: its files with maul's module.
type program struct {
	files []toolwire.File
	args  []string
}

// prepareProgram renders side's consumer and holds files to the top of
// the program's directory (U6).
func (r *Relay) prepareProgram(side toolwire.Side, files []toolwire.File, args []string) (program, error) {
	for _, f := range files {
		if strings.Contains(f.Name, "/") {
			return program{}, fmt.Errorf("%w: %q", errSubdirFile, f.Name)
		}
	}
	c, err := renderConsumer(filepath.Join(r.trees, string(sideName(side))), r.workTree)
	if err != nil {
		return program{}, err
	}
	out := program{files: append(files[:len(files):len(files)], toolwire.File{Name: "go.mod", Content: c.goMod}),
		args: args}
	return out, nil
}

// sideName is side's directory name: head or base.
func sideName(side toolwire.Side) toolwire.Side {
	if side.Recorded() == toolwire.SideBase {
		return toolwire.SideBase
	}
	return toolwire.SideHead
}

// compiled is a program's compile outcome.
type compiled struct {
	res toolexec.Result
	err error
	ok  bool
}

// compileProgram compiles p at side in run's scratch directory: its files
// written by the workspace's init, then the build.
func (r *Relay) compileProgram(ctx context.Context, side toolwire.Side, run string, p program) compiled {
	steps := []*toolexec.Request{
		{Argv: []string{"go", "work", "init", ".", r.workTree}, Files: p.files, Side: side, Run: run,
			Env: r.compileEnv()},
		{Argv: []string{"go", "build", "-o", consumerBinary, "."}, Side: side, Run: run, Env: r.compileEnv()},
	}
	var c compiled
	for _, req := range steps {
		if err := req.Check(); err != nil {
			return compiled{err: err}
		}
		res, err := r.sandbox(side).Do(ctx, req)
		c = compiled{res: res, err: err,
			ok: err == nil && res.Refused == "" && !res.Unclean && !res.TimedOut && res.Exit == 0}
		if !c.ok {
			return c
		}
	}
	return c
}

// runProgram runs a compiled program at side in run's scratch directory.
func (r *Relay) runProgram(ctx context.Context, side toolwire.Side, run string, args []string) (toolexec.Result,
	error) {
	return r.sandbox(side).Do(ctx, &toolexec.Request{Argv: append([]string{"./" + consumerBinary}, args...),
		Side: side, Run: run, Env: r.programEnv()})
}

// runVerb answers `maul-tool run`: the agent's program compiled and run at
// the side asked for, in a fresh scratch directory; {{bin}} and {{tree}}
// substituted as in an exec. A program that does not compile answers the
// compiler's output and exit, its reason saying so.
func (r *Relay) runVerb(ctx context.Context, req *toolwire.RunRequest) result {
	side := req.Side.Recorded()
	if !r.cfg.IsWorkspace() {
		return refusal(toolwire.RefusedUnavailable, "run needs a workspace; this tool pod has a preview")
	}
	if why := r.runnable(side); why != "" {
		return sideRefusal(side, toolwire.RefusedUnavailable, why)
	}
	files := make([]toolwire.File, 0, len(req.Files))
	for _, f := range req.Files {
		files = append(files, toolwire.File{Name: f.Name, Content: []byte(r.explore(string(f.Content)))})
	}
	args := make([]string, 0, len(req.Args))
	for _, a := range req.Args {
		args = append(args, r.explore(a))
	}
	p, err := r.prepareProgram(side, files, args)
	if err != nil {
		code := toolwire.RefusedUnavailable
		if errors.Is(err, errSubdirFile) {
			code = toolwire.RefusedInvalid
		}
		return sideRefusal(side, code, err.Error())
	}
	run := r.token()
	c := r.compileProgram(ctx, side, run, p)
	if !c.ok {
		res := programResult(c.res, c.err, "the sandbox refused the compile")
		if res.refused == "" {
			res.reason = strings.TrimSuffix("the program did not compile; "+res.reason, "; ")
		}
		return withSide(side, res)
	}
	res, err := r.runProgram(ctx, side, run, p.args)
	return withSide(side, programResult(res, err, "the sandbox refused the program"))
}

// workspaceRunAttempt runs a run check once at side: its files and args
// substituted (the marker in the files only), compiled, then run and
// judged as an exec is. A program that cannot be built against the tree,
// or does not compile, is an error.
func (r *Relay) workspaceRunAttempt(ctx context.Context, rc checks.RunCheck, run string,
	side toolwire.Side) []toolwire.StepRecord {
	marker := checks.ExecMarker(randomMarkerNonce())
	pl := r.places()
	files := make([]toolwire.File, 0, len(rc.Files))
	for _, f := range rc.Files {
		s, err := substitute(f.Content, run, marker, pl)
		if err != nil {
			return errStep("the program could not be built: " + err.Error())
		}
		files = append(files, toolwire.File{Name: f.Path, Content: []byte(s)})
	}
	args := make([]string, 0, len(rc.Args))
	for _, a := range rc.Args {
		s, err := substitute(a, run, "", pl)
		if err != nil {
			return errStep("the program could not be built: " + err.Error())
		}
		args = append(args, s)
	}
	p, err := r.prepareProgram(side, files, args)
	if err != nil {
		return errStep("the program cannot be built against the tree: " + err.Error())
	}
	if c := r.compileProgram(ctx, side, run, p); !c.ok {
		if c.res.Unclean || c.res.TimedOut {
			return errStep("the program's compile did not end cleanly, so maul cannot judge the run")
		}
		return errStep(fmt.Sprintf("the program did not compile (exit %d): a compile failure is no evidence about "+
			"the outcome", c.res.Exit))
	}
	ctx, cancel := withCheckTimeout(ctx, rc.Timeout)
	defer cancel()
	res, err := r.runProgram(ctx, side, run, p.args)
	return judgeWorkspace(res, err, rc.Expect, marker, run, pl)
}
