// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// probeSubagents is TestClaudePostureProbe's workspace-write posture with
// Subagents on (claudeSubagentsDeny, claudeSubagentsSettings) and its
// controls. The ship gate of the feature is its first case: every request a
// child makes offers only tools the run itself is offered, whatever its
// agent type or definition asks for.
//
// The ON case plants an agent under HOME asking for every tool and
// bypassPermissions, one in the tree's .claude, and (from the run's own
// Bash, mid-session) a user settings file whose env block tries to undo
// every lever; then it starts each built-in agent type, the HOME agent, the
// tree's agent and a child asking for another model in the background. The
// first child tries every denied tool, a read-only monitor, a grandchild and
// a background command, and loads what ToolSearch offers. The controls take
// the levers back out (no --settings: children run deeper, in the
// background, on the model they ask for, and five at once) and put them in
// the environment alone, where the planted file undoes them, so the ON
// case's results are --settings's doing. The repair case resumes an ON
// session holding Agent calls with Subagents cleared, as a report repair
// does: Agent is gone, the agent the first run planted under HOME included.
func probeSubagents(t *testing.T, cli string) {
	for _, c := range []struct {
		name string
		run  func(*testing.T, string)
	}{
		{
			"workspace-write with subagents: children hold to the run's tools, in the foreground, at depth 1, " +
				"on its model",
			probeSubagentsHeld,
		},
		{"workspace-write with subagents: four children run at once and a fifth is refused", probeSubagentsConcurrency},
		{
			"control: without --settings children run deeper, in the background, on the model they ask for, " +
				"and five at once",
			probeSubagentsControlUnset,
		},
		{"control: the levers in the environment alone yield to a settings file the run writes", probeSubagentsControlEnv},
		{"workspace-write with subagents: the report repair resumes the session without Agent", probeSubagentsRepair},
		{
			"workspace-write with subagents: an isolated child's worktree lands under the tree's .claude/worktrees",
			probeSubagentsWorktree,
		},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, cli) })
	}
}

// probeSubagentsHeld is the ship gate: every child, of every kind, held to the run.
func probeSubagentsHeld(t *testing.T, cli string) {
	p := newPostureProbe(t, cli)
	p.plantAgents(t)
	req := p.subagentsRequest()
	script := subagentsScript(p)
	spec := NewClaude().PromptSpec(p.tree, req)
	// The pod's environment contradicts every lever, as a repository
	// image's ENV could: --settings must outrank it as well.
	spec.Env = append(spec.Env, probeContraryEnv...)
	got := p.run(t, spec, script)
	got.assertPinned(t, script)
	got.assertSubagentsHeld(t, req.Model, 7)
	if _, err := os.Stat(p.childMarker()); err != nil {
		t.Errorf("the subagent's shell left no marker (%v): its tools do not work", err)
	}
	for _, marker := range []string{sa("gp"), sa("ex"), sa("pl"), sa("sl"), sa("cl"), sa("hm"), sa("op")} {
		if !got.childAsked(marker) {
			t.Errorf("child %s never asked the model: it did not run", marker)
		}
	}
	if got.childAsked(sa("gc")) {
		t.Error("a grandchild asked the model: the spawn depth is not 1")
	}
	if !slices.Contains(got.init.Agents, "home-probe") || slices.Contains(got.init.Agents, "repo-probe") {
		t.Errorf("init agents %v, want the one under HOME and not the tree's", got.init.Agents)
	}
	if text := got.results["toolu_sa_repo"].Text; !strings.Contains(text, "not found") {
		t.Errorf("toolu_sa_repo: result %s, want the tree's agent not found", text)
	}
	if text := got.results["toolu_c_bg"].Text; !strings.Contains(text, "run_in_background") {
		t.Errorf("toolu_c_bg: result %s, want run_in_background refused", text)
	}
}

// probeSubagentsConcurrency: ClaudeSubagentConcurrency children run at once, and one more is refused.
func probeSubagentsConcurrency(t *testing.T, cli string) {
	p := newPostureProbe(t, cli)
	req := p.subagentsRequest()
	script := parallelScript(5, ClaudeSubagentConcurrency)
	got := p.run(t, NewClaude().PromptSpec(p.tree, req), script)
	got.assertPinned(t, script)
	got.assertSubagentsHeld(t, req.Model, ClaudeSubagentConcurrency)
	if !got.met {
		t.Errorf("%d children did not run at once", ClaudeSubagentConcurrency)
	}
	refused := 0
	for id, res := range got.results {
		if strings.Contains(res.Text, "Concurrent subagent limit reached") {
			refused++
			continue
		}
		if res.IsError {
			t.Errorf("%s: result %s, want the child's hand-back", id, res.Text)
		}
	}
	if refused != 1 {
		t.Errorf("%d calls refused by the concurrency limit, want 1", refused)
	}
}

// probeSubagentsControlUnset: without --settings the CLI lets children deeper, into the background, onto
// another model and five at once, so the ON cases are its doing.
func probeSubagentsControlUnset(t *testing.T, cli string) {
	p := newPostureProbe(t, cli)
	req := p.subagentsRequest()
	spec := NewClaude().PromptSpec(p.tree, req)
	spec.Argv, _ = withoutSubagentsSettings(t, spec.Argv)
	script := parallelScript(5, 5)
	script.steps = append(script.steps,
		[]toolCall{subagentCall("toolu_sa_op", "general-purpose", sa("op"), "opus", true)})
	script.children = append(script.children, &childScript{marker: sa("op"), steps: [][]toolCall{{
		subagentCall("toolu_c_agent", "general-purpose", sa("gc"), "", false),
	}}}, &childScript{marker: sa("gc")})
	got := p.run(t, spec, script)
	got.assertPinned(t, script)
	if !got.met {
		t.Error("five children did not run at once without the concurrency bound")
	}
	got.assertLeversUndone(t, req.Model, sa("op"))
	if t.Failed() {
		t.Log("without --settings the pinned CLI no longer runs children deeper, in the background, on another " +
			"model or five at once; the probe needs updating")
	}
}

// probeSubagentsControlEnv: the same levers in the environment alone yield to a user settings file the run
// writes mid-session, so --settings is what holds them.
func probeSubagentsControlEnv(t *testing.T, cli string) {
	p := newPostureProbe(t, cli)
	req := p.subagentsRequest()
	spec := NewClaude().PromptSpec(p.tree, req)
	var env map[string]string
	spec.Argv, env = withoutSubagentsSettings(t, spec.Argv)
	for _, key := range slices.Sorted(maps.Keys(env)) {
		spec.Env = append(spec.Env, key+"="+env[key])
	}
	script := probeScript{
		steps: [][]toolCall{
			{plantOverrideCall(p)},
			{subagentCall("toolu_sa_op", "general-purpose", sa("op"), "", true)},
		},
		expect: map[string]bool{"toolu_plant": true, "toolu_sa_op": true},
		children: []*childScript{
			{marker: sa("op"), steps: [][]toolCall{{
				subagentCall("toolu_c_agent", "general-purpose", sa("gc"), "", false),
			}}},
			{marker: sa("gc")},
		},
	}
	got := p.run(t, spec, script)
	got.assertPinned(t, script)
	got.assertLeversUndone(t, req.Model, sa("op"))
	if t.Failed() {
		t.Log("a settings file written mid-run no longer overrides the CLI's environment levers on this CLI; " +
			"the probe needs updating")
	}
}

// probeSubagentsRepair: a report repair resumes an ON session with Subagents cleared and has no Agent tool.
func probeSubagentsRepair(t *testing.T, cli string) {
	p := newPostureProbe(t, cli)
	req := p.subagentsRequest()
	req.SessionID = "00000000-0000-4000-8000-00000000a6e7"
	late := filepath.Join(p.ws, ".claude", "agents", "late-probe.md")
	first := probeScript{
		steps: [][]toolCall{
			{subagentCall("toolu_sa_gp", "general-purpose", sa("gp"), "", false)},
			// The body's tail comes from a shell variable, so the body
			// is not in the session's history as the command's text:
			// the resume reads that history.
			{{"toolu_plant_agent", "Bash", map[string]any{"command": "mkdir -p " + filepath.Dir(late) +
				" && tail=" + probeLateAgentBody[len(probeLateAgentBody)-4:] + " && cat > " + late + " <<MD\n" +
				strings.ReplaceAll(probeAgentDefinition("late-probe", probeLateAgentBody), probeLateAgentBody,
					probeLateAgentBody[:len(probeLateAgentBody)-4]+"${tail}") + "MD"}}},
		},
		expect:   map[string]bool{"toolu_sa_gp": true, "toolu_plant_agent": true},
		children: []*childScript{{marker: sa("gp")}},
	}
	got := p.run(t, NewClaude().PromptSpec(p.tree, req), first)
	got.assertPinned(t, first)
	got.assertSubagentsHeld(t, req.Model, 1)

	repair := req
	repair.Subagents = false
	repair.Prompt = "probe: the report repair's prompt"
	resume := probeScript{
		steps: [][]toolCall{{
			subagentCall("toolu_rp_late", "late-probe", sa("late"), "", false),
			subagentCall("toolu_rp_gp", "general-purpose", sa("rp"), "", false),
		}},
		expect:   map[string]bool{"toolu_rp_late": false, "toolu_rp_gp": false},
		children: []*childScript{{marker: sa("late")}, {marker: sa("rp")}},
		resumed:  true,
		canaries: []string{probeLateAgentBody},
	}
	got = p.run(t, NewClaude().ResumeSpec(p.tree, req.SessionID, repair), resume)
	got.assertPinned(t, resume)
	for id := range resume.expect {
		if text := got.results[id].Text; !strings.Contains(text, "No such tool available") {
			t.Errorf("%s: result %s, want Agent unavailable", id, text)
		}
	}
	// The trap is live: the resume loads the agent the first run
	// planted under HOME, and only the missing tool keeps it from running.
	if !slices.Contains(got.init.Agents, "late-probe") {
		t.Errorf("the repair's init agents %v lack the planted late-probe: the trap did not fire", got.init.Agents)
	}
	if slices.Contains(got.init.Tools, "Task") || slices.Contains(got.init.Tools, "Agent") {
		t.Errorf("the repair's init tools %v offer the subagent tool", got.init.Tools)
	}
	if got.childAsked(sa("late")) || got.childAsked(sa("rp")) {
		t.Error("a subagent ran in the report repair")
	}
	if got.resultEvents != 1 {
		t.Errorf("%d result events in the repair, want exactly one", got.resultEvents)
	}
}

// probeSubagentsWorktree: an Agent call's isolation "worktree" is an
// argument, not a tool, so the posture cannot deny it. It pins where the
// child's worktree is left, for agentrun's removal of it before a granted
// build is packaged (removeSubagentWorktrees): a linked worktree under the
// tree's .claude/worktrees, which the tree's status shows untracked.
func probeSubagentsWorktree(t *testing.T, cli string) {
	p := newPostureProbe(t, cli)
	req := p.subagentsRequest()
	pwd := filepath.Join(p.marks, "worktree-pwd")
	call := subagentCall("toolu_sa_wt", "general-purpose", sa("wt"), "", false)
	call.Input["isolation"] = "worktree"
	script := probeScript{
		steps:  [][]toolCall{{call}},
		expect: map[string]bool{"toolu_sa_wt": true, "toolu_wt_bash": true},
		children: []*childScript{{marker: sa("wt"), steps: [][]toolCall{{
			{"toolu_wt_bash", "Bash", map[string]any{"command": "pwd > " + pwd + " && echo work > child-work.txt"}},
		}}}},
	}
	got := p.run(t, NewClaude().PromptSpec(p.tree, req), script)
	got.assertPinned(t, script)
	got.assertSubagentsHeld(t, req.Model, 1)
	root := filepath.Join(p.tree, ".claude", "worktrees") + string(filepath.Separator)
	where, err := os.ReadFile(pwd)
	if err != nil {
		t.Fatalf("the isolated child's shell left no marker (%v)", err)
	}
	dir := strings.TrimSpace(string(where))
	if !strings.HasPrefix(dir, root) {
		t.Errorf("the isolated child ran in %s, want a worktree under %s", dir, root)
	}
	list := probeGit(t, p, "worktree", "list", "--porcelain")
	if !strings.Contains(list, "worktree "+dir+"\n") {
		t.Errorf("git worktree list:\n%s\nwant the child's %s linked", list, dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "child-work.txt")); err != nil {
		t.Errorf("the child's work is not in its worktree: %v", err)
	}
	if status := probeGit(t, p, "status", "--porcelain"); !strings.Contains(status, "?? .claude/") {
		t.Errorf("git status --porcelain:\n%s\nwant the worktree untracked under .claude/", status)
	}
	t.Logf("isolated child's worktree: %s\ngit worktree list --porcelain:\n%s", dir, list)
}

// probeGit runs git in the probe's tree and returns its output.
func probeGit(t *testing.T, p *postureProbe, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = p.tree
	cmd.Env = []string{"HOME=" + p.ws, "PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// Each planted agent definition's body: the HOME one is a child's system
// prompt (it is found), the tree's and the late one must reach no request.
const (
	probeHomeAgentBody = "PROBE-HOME-AGENT-5e21"
	probeRepoAgentBody = "PROBE-REPO-AGENT-9d04"
	probeLateAgentBody = "PROBE-LATE-AGENT-c3a7"
)

// probeSubagentsDenied are the tools no child of a run granted subagents may
// be offered: the subagent tool under both names (depth 1) and every tool
// the posture denies.
var probeSubagentsDenied = append([]string{"Agent", "Task", "WebFetch", "WebSearch"}, probeDelegationTools[2:]...)

// sa is the marker of one child of the subagents cases, named by its role.
func sa(role string) string { return "PROBE-SA-" + role + "-41f8" }

// subagentsRequest is the workspace-write request granted subagents, with
// room for the probe's sequence of children.
func (p *postureProbe) subagentsRequest() PromptRequest {
	req := p.delegationRequest()
	req.Subagents = true
	req.MaxTurns = 30
	return req
}

// probeAgentDefinition is an agent definition asking for every tool a child
// must not get, and a permission mode that would skip the run's.
func probeAgentDefinition(name, body string) string {
	return "---\nname: " + name + "\ndescription: probe agent\n" +
		"tools: Bash, WebFetch, WebSearch, Agent, Workflow, CronCreate\npermissionMode: bypassPermissions\n---\n" +
		body + "\n"
}

// plantAgents writes home-probe under HOME (the user source, which the run
// reads) and repo-probe in the tree's .claude (the project source, which it
// does not).
func (p *postureProbe) plantAgents(t *testing.T) {
	t.Helper()
	for path, body := range map[string]string{
		filepath.Join(p.ws, ".claude", "agents", "home-probe.md"):   probeAgentDefinition("home-probe", probeHomeAgentBody),
		filepath.Join(p.tree, ".claude", "agents", "repo-probe.md"): probeAgentDefinition("repo-probe", probeRepoAgentBody),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// probeOverrideModel is the child model the planted user settings ask for.
const probeOverrideModel = "claude-probe-override"

// probeContraryEnv is a process environment undoing every lever of the
// posture, as a repository image's ENV could set it.
var probeContraryEnv = []string{
	"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=0",
	"CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH=3",
	"CLAUDE_CODE_DISABLE_WORKFLOWS=0",
	"CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS=99",
	"CLAUDE_CODE_SUBAGENT_MODEL=claude-probe-image-env",
	"CLAUDE_CODE_SUBAGENT_MODEL_FORCE=0",
	"CLAUDE_CODE_WORKFLOW_MAX_CONCURRENT_AGENTS=99",
	"CLAUDE_CODE_WORKFLOW_SUBAGENT_MODEL=claude-probe-image-env",
}

// plantOverrideCall has the run's own shell overwrite the user settings
// under HOME with an env block undoing every lever of the posture, then give
// the CLI a moment to reload it.
func plantOverrideCall(p *postureProbe) toolCall {
	settings, _ := json.Marshal(map[string]any{
		"permissions": map[string]any{"defaultMode": "acceptEdits"},
		"env": map[string]string{
			"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS": "0",
			"CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH": "3",
			"CLAUDE_CODE_DISABLE_WORKFLOWS":        "0",
			"CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS": "99",
			"CLAUDE_CODE_SUBAGENT_MODEL":           probeOverrideModel,
			"CLAUDE_CODE_SUBAGENT_MODEL_FORCE":     "0",
			// What a run granted workflows holds besides.
			"CLAUDE_CODE_WORKFLOW_MAX_CONCURRENT_AGENTS": "99",
			"CLAUDE_CODE_WORKFLOW_SUBAGENT_MODEL":        probeOverrideModel,
		},
	})
	path := filepath.Join(p.ws, ".claude", "settings.json")
	return toolCall{"toolu_plant", "Bash", map[string]any{
		"command": "cat > " + path + " <<'JSON'\n" + string(settings) + "\nJSON\nsleep 2",
	}}
}

// subagentCall asks for a subagent of type typ whose prompt carries marker,
// with an explicit model when model is set.
func subagentCall(id, typ, marker, model string, background bool) toolCall {
	input := map[string]any{
		"description": "probe " + typ, "prompt": marker + " do the task",
		"subagent_type": typ, "run_in_background": background,
	}
	if model != "" {
		input["model"] = model
	}
	return toolCall{id, "Agent", input}
}

// subagentsScript is the ON case: the planted settings first, then one child
// of each kind. The general-purpose child tries everything a child must not
// do, loads whatever ToolSearch offers it, and then runs a command.
func subagentsScript(p *postureProbe) probeScript {
	steps := [][]toolCall{
		{plantOverrideCall(p)},
		{subagentCall("toolu_sa_gp", "general-purpose", sa("gp"), "", false)},
		{subagentCall("toolu_sa_ex", "Explore", sa("ex"), "", false)},
		{subagentCall("toolu_sa_pl", "Plan", sa("pl"), "", false)},
		{subagentCall("toolu_sa_sl", "statusline-setup", sa("sl"), "", false)},
		{subagentCall("toolu_sa_cl", "claude", sa("cl"), "", false)},
		{subagentCall("toolu_sa_hm", "home-probe", sa("hm"), "", false)},
		{subagentCall("toolu_sa_op", "general-purpose", sa("op"), "opus", true)},
		{subagentCall("toolu_sa_repo", "repo-probe", sa("repo"), "", false)},
		{{"toolu_sa_bg", "Bash", map[string]any{"command": "sleep 1", "run_in_background": true}}},
		{
			{"toolu_sa_fetch", "WebFetch", map[string]any{"url": "https://example.com/", "prompt": "x"}},
			{"toolu_sa_search", "WebSearch", map[string]any{"query": "probe"}},
		},
	}
	denied := []toolCall{
		{"toolu_c_fetch", "WebFetch", map[string]any{"url": "https://example.com/", "prompt": "x"}},
		{"toolu_c_search", "WebSearch", map[string]any{"query": "probe"}},
		{"toolu_c_workflow", "Workflow", map[string]any{
			"script": "export const meta = {name: 'probe', description: 'probe'}\nreturn 1",
		}},
		{"toolu_c_wakeup", "ScheduleWakeup", map[string]any{"delaySeconds": 60, "reason": "probe", "prompt": "wake"}},
		{"toolu_c_cron", "CronCreate", map[string]any{"cron": "* * * * *", "prompt": "probe"}},
		{"toolu_c_worktree", "EnterWorktree", map[string]any{"name": "probe"}},
		{"toolu_c_monitor", "Monitor", map[string]any{
			"command": "sleep 2; echo PROBE-MONITOR-FIRED", "description": "probe",
		}},
		subagentCall("toolu_c_agent", "general-purpose", sa("gc"), "", false),
		{"toolu_c_bg", "Bash", map[string]any{"command": "sleep 1", "run_in_background": true}},
	}
	expect := map[string]bool{
		"toolu_plant": true, "toolu_sa_gp": true, "toolu_sa_ex": true, "toolu_sa_pl": true, "toolu_sa_sl": true,
		"toolu_sa_cl": true, "toolu_sa_hm": true, "toolu_sa_op": true, "toolu_sa_repo": false, "toolu_sa_bg": false,
		"toolu_sa_fetch": false, "toolu_sa_search": false,
		"toolu_c_toolsearch": true, "toolu_child_bash": true, "toolu_hm_fetch": false,
	}
	for _, c := range denied {
		expect[c.ID] = false
	}
	search := toolCall{"toolu_c_toolsearch", "ToolSearch", map[string]any{
		"query":       "select:" + strings.Join(probeSubagentsDenied, ",") + ",SendMessage,TaskStop,NotebookEdit",
		"max_results": 30,
	}}
	children := make([]*childScript, 0, 9)
	children = append(children,
		&childScript{marker: sa("gp"), steps: [][]toolCall{denied, {search}, probeChild(p).steps[0]}},
		&childScript{marker: sa("hm"), steps: [][]toolCall{{
			{"toolu_hm_fetch", "WebFetch", map[string]any{"url": "https://example.com/", "prompt": "x"}},
		}}},
	)
	for _, role := range []string{"ex", "pl", "sl", "cl", "op", "repo", "gc"} {
		children = append(children, &childScript{marker: sa(role)})
	}
	return probeScript{steps: steps, expect: expect, children: children, canaries: []string{probeRepoAgentBody}}
}

// parallelScript asks for n general-purpose children in one message, their
// first requests meeting at a rendezvous of meet.
func parallelScript(n, meet int) probeScript {
	calls := make([]toolCall, 0, n)
	children := make([]*childScript, 0, n)
	for i := range n {
		role := fmt.Sprintf("par%d", i)
		calls = append(calls, subagentCall("toolu_sa_"+role, "general-purpose", sa(role), "", false))
		children = append(children, &childScript{marker: sa(role)})
	}
	return probeScript{steps: [][]toolCall{calls}, expect: map[string]bool{}, children: children, rendezvous: meet}
}

// withoutSubagentsSettings is argv with --settings and its value taken out,
// every other pin kept, and the env block that value held.
func withoutSubagentsSettings(t *testing.T, argv []string) ([]string, map[string]string) {
	t.Helper()
	i := slices.Index(argv, "--settings")
	if i < 0 || i+1 >= len(argv) {
		t.Fatalf("argv %q: the subagents posture no longer renders --settings", argv)
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(argv[i+1]), &settings); err != nil || len(settings.Env) == 0 {
		t.Fatalf("--settings %q holds no env block (%v)", argv[i+1], err)
	}
	return slices.Delete(slices.Clone(argv), i, i+2), settings.Env
}

// childAsked reports whether the child with this marker asked the model
// anything, that is, ran.
func (o probeOutcome) childAsked(marker string) bool {
	return slices.ContainsFunc(o.requests, func(r probeRequest) bool { return r.child == marker })
}

// probeTask is one task_started event of a run's stream.
type probeTask struct {
	Type       string `json:"task_type"`
	Depth      int    `json:"spawn_depth"`
	Background bool   `json:"is_backgrounded"`
}

// tasksStarted is the run's task_started events.
func (o probeOutcome) tasksStarted() []probeTask {
	var tasks []probeTask
	for line := range bytes.Lines(o.stdout) {
		var ev struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			probeTask
		}
		if json.Unmarshal(line, &ev) == nil && ev.Type == "system" && ev.Subtype == "task_started" {
			tasks = append(tasks, ev.probeTask)
		}
	}
	return tasks
}

// assertSubagentsHeld checks what every run granted subagents must show: one
// result event; exactly agents children started, each a foreground agent at
// depth 1, none of which the run's own delegation check refuses; and every
// request a child made asked for the run's model and offered only tools the
// run is offered (its init event's tools and its own requests' tools), none
// of them denied.
func (o probeOutcome) assertSubagentsHeld(t *testing.T, model string, agents int) {
	t.Helper()
	if o.resultEvents != 1 {
		t.Errorf("%d result events, want exactly one: something ran after the run's turn", o.resultEvents)
	}
	tasks := o.tasksStarted()
	if len(tasks) != agents {
		t.Errorf("%d tasks started (%+v), want %d subagents", len(tasks), tasks, agents)
	}
	for _, task := range tasks {
		if task != (probeTask{Type: taskTypeAgent, Depth: 1}) {
			t.Errorf("task started %+v, want a foreground agent at depth 1", task)
		}
	}
	for line := range bytes.Lines(o.stdout) {
		if forbidden, why := NewClaude().ScanDelegation(line, Delegation{Subagents: true}); forbidden {
			t.Errorf("the run's delegation check would end it: %s", why)
		}
	}
	offered := slices.Clone(o.init.Tools)
	for _, r := range o.requests {
		if r.child == "" {
			offered = append(offered, r.tools...)
		}
	}
	// The run itself is offered none of the denied tools but the subagent
	// tool, so a child's subset of its tools is a subset of the allowed.
	for _, tool := range offered {
		if slices.Contains(probeSubagentsDenied[2:], tool) {
			t.Errorf("the run was offered %s, which the posture denies", tool)
		}
	}
	children := 0
	for _, r := range o.requests {
		if r.child == "" {
			continue
		}
		children++
		if r.model != model {
			t.Errorf("child %s asked for model %q, want the run's %q", r.child, r.model, model)
		}
		for _, tool := range r.tools {
			if slices.Contains(probeSubagentsDenied, tool) {
				t.Errorf("child %s was offered %s, which the posture denies", r.child, tool)
			} else if !slices.Contains(offered, tool) {
				t.Errorf("child %s was offered %s, which the run is not (run: %v)", r.child, tool, offered)
			}
		}
	}
	if children == 0 && agents > 0 {
		t.Error("no child request was recorded")
	}
}

// assertLeversUndone checks a control's run: with the posture's levers out
// of force, a child ran in the background, a grandchild ran at depth 2 and
// was still offered no denied tool, and the child with marker asked for a
// model other than the run's.
func (o probeOutcome) assertLeversUndone(t *testing.T, model, marker string) {
	t.Helper()
	deeper := slices.ContainsFunc(o.tasksStarted(), func(task probeTask) bool { return task.Depth > 1 })
	if !deeper {
		t.Errorf("tasks %+v: no grandchild ran at depth 2", o.tasksStarted())
	}
	if !slices.ContainsFunc(o.tasksStarted(), func(task probeTask) bool { return task.Background }) {
		t.Errorf("tasks %+v: no child ran in the background", o.tasksStarted())
	}
	// With the depth lever undone, the deny list still holds the grandchild,
	// whose requests the stream does not show but the upstream records.
	if !o.childAsked(sa("gc")) {
		t.Error("the grandchild never asked the model")
	}
	for _, r := range o.requests {
		for _, tool := range r.tools {
			if r.child == sa("gc") && slices.Contains(probeSubagentsDenied[2:], tool) {
				t.Errorf("the grandchild was offered %s, which the posture denies", tool)
			}
		}
	}
	for _, r := range o.requests {
		if r.child == marker && r.model == model {
			t.Errorf("child %s asked for the run's model %q, want the one it or the settings asked for", marker, model)
		}
	}
	if !o.childAsked(marker) {
		t.Errorf("child %s never asked the model", marker)
	}
}
