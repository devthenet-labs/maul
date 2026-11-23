// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// probeWorkflows is TestClaudePostureProbe's workspace-write posture with
// Subagents and Workflows on (claudeWorkflowsAllow, claudeSubagentsSettings)
// and its controls.
//
// The first case is the ship gate: an inline workflow, started after the
// run's own shell has written user settings trying to undo every lever and
// under a process environment that contradicts them, runs agents each
// offered only the run's tools (StructuredOutput aside), none of them a
// subagent, a workflow or a denied tool, each refused the web tools and the
// rest of the denied ones it tries, each on the run's model whatever it asks
// for; the run's own delegation check passes the stream, and the result
// counts the agents' tokens though the stream shows none of their turns.
//
// The sources case pins which workflows a run can reach by name, from the
// Workflow tool or from a script's own workflow(): the CLI's built-in ones
// alone, since the tree's .claude is not read and HOME, read once at the
// start, holds none; a workflow the run's shell writes under HOME, one
// shadowing a built-in name included, is not found. It also pins the hole
// the CLI leaves, which nothing on this pin closes: a script path reads any
// file the run may read, the tree's included, whether the Workflow call
// names it or an inline script's own workflow() does. The run's delegation
// check ends a run whose call names one, after the fact; a nested one it
// does not see.
//
// The concurrency case and its control show the bound queues a workflow's
// agents at ClaudeWorkflowConcurrency, and that without the levers five run
// at once on the model one asks for. The ship gate's planted settings set a
// workflow agent model of their own as well, which the child model's FORCE
// outranks, so the posture sets none. The repair case resumes a session that
// ran a workflow with the grant cleared, as a report repair does: Workflow
// is gone, the one the first run planted under HOME included.
func probeWorkflows(t *testing.T, cli string) {
	for _, c := range []struct {
		name string
		run  func(*testing.T, string)
	}{
		{
			"workspace-write with workflows: an inline workflow's agents hold to the run's tools and model, " +
				"and are refused the web",
			probeWorkflowsHeld,
		},
		{
			"workspace-write with workflows: only built-in workflows are found by name; a script path, direct or " +
				"nested, still runs",
			probeWorkflowsSources,
		},
		{"workspace-write with workflows: four agents run at once and the fifth waits", probeWorkflowsConcurrency},
		{
			"control: without the workflow levers five agents run at once on the model they ask for",
			probeWorkflowsControl,
		},
		{"workspace-write with workflows: the report repair resumes the session without Workflow", probeWorkflowsRepair},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, cli) })
	}
}

// probeWorkflowsHeld is the ship gate: every agent of an inline workflow held to the run.
func probeWorkflowsHeld(t *testing.T, cli string) {
	p := newPostureProbe(t, cli)
	req := p.workflowsRequest()
	agents := []string{wf("w0"), wf("w1"), wf("w2")}
	body := "const r = await parallel(" + jsList(agents) + ".map((m, i) => () => agent(m + ' do the task', " +
		"{ label: 'a' + i, phase: 'One' })))\n" +
		"const o = await agent('" + wf("op") + " on opus', { label: 'op', phase: 'One', model: 'opus' })\n" +
		"return { r, o }\n"
	script := probeScript{
		steps: [][]toolCall{
			{plantOverrideCall(p)},
			{workflowInline("toolu_wf", "probe-held", body)},
		},
		expect: map[string]bool{"toolu_plant": true, "toolu_wf": true},
		children: []*childScript{
			{marker: wf("w0"), steps: [][]toolCall{probeWorkflowAgentDenied(), {probeWorkflowSearch()},
				probeChild(p).steps[0]}},
			{marker: wf("w1")}, {marker: wf("w2")}, {marker: wf("op")}, {marker: wf("gc")},
		},
	}
	spec := NewClaude().PromptSpec(p.tree, req)
	spec.Env = append(spec.Env, probeContraryEnv...)
	got := p.run(t, spec, script)
	got.assertPinned(t, script)
	got.assertWorkflowsHeld(t, req.Model)
	for _, marker := range append(agents, wf("op")) {
		if !got.childAsked(marker) {
			t.Errorf("workflow agent %s never asked the model: it did not run", marker)
		}
	}
	if got.childAsked(wf("gc")) {
		t.Error("a workflow agent's subagent asked the model")
	}
	if _, err := os.Stat(p.childMarker()); err != nil {
		t.Errorf("the workflow agent's shell left no marker (%v): its tools do not work", err)
	}
	for _, c := range probeWorkflowAgentDenied() {
		switch res, ok := got.upstreamResults[c.ID]; {
		case !ok:
			t.Errorf("%s: the workflow agent's call got no result", c.ID)
		case !res.IsError:
			t.Errorf("%s: the workflow agent's %s was not refused: %s", c.ID, c.Name, res.Text)
		}
	}
	for _, id := range []string{"toolu_w_fetch", "toolu_w_search"} {
		if text := got.upstreamResults[id].Text; !strings.Contains(text, "No such tool available") {
			t.Errorf("%s: result %s, want the web tool unavailable", id, text)
		}
	}
	if got.resultEvents != 2 {
		t.Errorf("%d result events, want two: the run's turn, then the wake the workflow's end makes",
			got.resultEvents)
	}
	// The result counts the workflow's agents, whose calls the stream never
	// shows: each of the upstream's replies is one output token.
	res, ok := NewClaude().ParseResult(got.stdout)
	if !ok || res.Usage == nil || res.Usage.OutputTokens == nil {
		t.Fatalf("ParseResult = %+v, %v: no usage", res, ok)
	}
	children := 0
	for _, r := range got.requests {
		if r.child != "" {
			children++
		}
	}
	if out := *res.Usage.OutputTokens; out < got.mainThreadOutput()+children {
		t.Errorf("output tokens %d, want at least the main thread's %d and the workflow agents' %d calls",
			out, got.mainThreadOutput(), children)
	}
	if res.SubagentTurns != 0 {
		t.Errorf("SubagentTurns = %d, want none: a workflow's agents print no turns", res.SubagentTurns)
	}
}

// probeWorkflowsSources: a name finds only a built-in workflow; a script path finds any file the run reads.
func probeWorkflowsSources(t *testing.T, cli string) {
	p := newPostureProbe(t, cli)
	req := p.workflowsRequest()
	treeWF := filepath.Join(p.tree, ".claude", "workflows", "repo-wf.js")
	writeProbeFile(t, treeWF, workflowScript("repo-wf", agentOnce(wf("repo"))))
	nestedWF := filepath.Join(p.tree, "scripts", "nested-wf.js")
	writeProbeFile(t, nestedWF, workflowScript("nested-wf", agentOnce(wf("nested"))))
	homeWF := filepath.Join(p.ws, ".claude", "workflows")
	plant := "mkdir -p " + homeWF + " && cat > " + homeWF + "/late-wf.js <<'JS'\n" +
		workflowScript("late-wf", agentOnce(wf("late"))) + "JS\ncat > " + homeWF + "/shadow.js <<'JS'\n" +
		workflowScript(claudeBuiltinWorkflows[0], agentOnce(wf("shadow"))) + "JS\nsleep 2"
	script := probeScript{
		steps: [][]toolCall{
			{workflowCallNamed("toolu_wf_repo", "repo-wf", "")},
			{workflowCallNamed("toolu_wf_none", "probe-none", "")},
			{workflowInline("toolu_wf_nested_path", "probe-nested-path",
				"const r = await workflow({ scriptPath: '"+nestedWF+"' })\nreturn { r }\n")},
			{{"toolu_plant_wf", "Bash", map[string]any{"command": plant}}},
			{workflowCallNamed("toolu_wf_late", "late-wf", "")},
			{workflowInline("toolu_wf_nested_name", "probe-nested-name",
				"const r = await workflow('late-wf')\nreturn { r }\n")},
			{workflowCallNamed("toolu_wf_builtin", claudeBuiltinWorkflows[0], probeResearchArgs+" what is x")},
			{{"toolu_wf_path", "Workflow", map[string]any{"scriptPath": treeWF}}},
		},
		expect: map[string]bool{
			"toolu_wf_repo": false, "toolu_wf_none": false, "toolu_plant_wf": true, "toolu_wf_late": false,
			"toolu_wf_builtin": true, "toolu_wf_path": true, "toolu_wf_nested_path": true, "toolu_wf_nested_name": true,
		},
		children: []*childScript{
			{marker: probeResearchArgs}, {marker: wf("repo")}, {marker: wf("late")}, {marker: wf("shadow")},
			{marker: wf("nested")},
		},
	}
	got := p.run(t, NewClaude().PromptSpec(p.tree, req), script)
	got.assertPinned(t, script)
	for _, id := range []string{"toolu_wf_repo", "toolu_wf_late"} {
		if text := got.results[id].Text; !strings.Contains(text, "not found") {
			t.Errorf("%s: result %s, want the workflow not found", id, text)
		}
	}
	// With HOME as the pod starts it, a name reaches the built-in workflows
	// alone, which are the ones the delegation check admits by name.
	if text, want := got.results["toolu_wf_none"].Text,
		"Available: "+strings.Join(claudeBuiltinWorkflows, ", ")+"<"; !strings.Contains(text, want) {
		t.Errorf("toolu_wf_none: result %s, want the built-in workflows alone (%q)", text, want)
	}
	if got.childAsked(wf("late")) || got.childAsked(wf("shadow")) {
		t.Error("a workflow the run planted under HOME ran")
	}
	if !got.childAsked(probeResearchArgs) {
		t.Errorf("the built-in %s workflow's agents never asked the model", claudeBuiltinWorkflows[0])
	}
	for _, r := range got.requests {
		for _, tool := range r.tools {
			if r.child != "" && slices.Contains(probeWorkflowsDenied, tool) {
				t.Errorf("workflow agent %s was offered %s, which the posture denies", r.child, tool)
			}
		}
	}
	// The hole: the CLI runs a tree's script by its path, named by the call
	// or by an inline script's own workflow(). The run's delegation check
	// refuses the call that names one, as it refuses every call for a
	// workflow by a name not built in, and passes the rest, the nested path
	// among them: it cannot be told from any inline script.
	if !got.childAsked(wf("repo")) {
		t.Error("the tree's workflow did not run by its path: the CLI now refuses it, and the probe needs updating")
	}
	if !got.childAsked(wf("nested")) {
		t.Error("the tree's workflow did not run by a script's own workflow({scriptPath}): the CLI now refuses it, " +
			"and the probe and the docs need updating")
	}
	var refused []string
	for line := range bytes.Lines(got.stdout) {
		forbidden, why := NewClaude().ScanDelegation(line, probeWorkflowsGrant)
		if !forbidden {
			continue
		}
		id := "?"
		for _, call := range []string{"toolu_wf_repo", "toolu_wf_none", "toolu_wf_late", "toolu_wf_path"} {
			if bytes.Contains(line, []byte(`"`+call+`"`)) {
				id = call
			}
		}
		if id == "?" {
			t.Errorf("the delegation check refuses another line (%s): %.200s", why, line)
		}
		refused = append(refused, id)
	}
	if want := []string{"toolu_wf_repo", "toolu_wf_none", "toolu_wf_late", "toolu_wf_path"}; !slices.Equal(
		refused, want) {
		t.Errorf("the delegation check refuses the calls %v, want %v", refused, want)
	}
}

// probeWorkflowsConcurrency: ClaudeWorkflowConcurrency agents run at once,
// and one more waits for a slot: a rendezvous of all of them is never met,
// as it is without the bound (probeWorkflowsControl).
func probeWorkflowsConcurrency(t *testing.T, cli string) {
	p := newPostureProbe(t, cli)
	req := p.workflowsRequest()
	n := ClaudeWorkflowConcurrency + 1
	script := parallelWorkflowScript(n, n)
	got := p.run(t, NewClaude().PromptSpec(p.tree, req), script)
	got.assertPinned(t, script)
	got.assertWorkflowsHeld(t, req.Model)
	if got.met {
		t.Errorf("%d workflow agents ran at once past the bound of %d", n, ClaudeWorkflowConcurrency)
	}
	if got.peak != ClaudeWorkflowConcurrency {
		t.Errorf("%d workflow agents ran at once, want %d", got.peak, ClaudeWorkflowConcurrency)
	}
	for _, c := range script.children {
		if !got.childAsked(c.marker) {
			t.Errorf("workflow agent %s never ran: the bound refused it rather than queueing it", c.marker)
		}
	}
}

// probeWorkflowsControl: without the workflow levers in --settings the
// pinned CLI runs five agents at once and one on the model it asks for, so
// the gate's results are the levers' doing.
func probeWorkflowsControl(t *testing.T, cli string) {
	p := newPostureProbe(t, cli)
	req := p.workflowsRequest()
	spec := NewClaude().PromptSpec(p.tree, req)
	spec.Argv = withSettingsEnv(t, spec.Argv, func(env map[string]string) {
		delete(env, "CLAUDE_CODE_WORKFLOW_MAX_CONCURRENT_AGENTS")
		delete(env, "CLAUDE_CODE_SUBAGENT_MODEL_FORCE")
	})
	script := parallelWorkflowScript(5, 5)
	script.steps[0][0].Input["script"] = workflowScript("probe-par",
		strings.TrimSuffix(parallelBody(5), "return { r }\n")+
			"const o = await agent('"+wf("op")+" on opus', { label: 'op', phase: 'One', model: 'opus' })\n"+
			"return { r, o }\n")
	script.children = append(script.children, &childScript{marker: wf("op")})
	got := p.run(t, spec, script)
	got.assertPinned(t, script)
	if !got.met || got.peak != 5 {
		t.Errorf("met %v, peak %d: five workflow agents did not run at once without the bound", got.met, got.peak)
	}
	for _, r := range got.requests {
		if r.child == wf("op") && r.model == req.Model {
			t.Errorf("the agent asking for opus asked for the run's model %q without the levers", r.model)
		}
	}
	if !got.childAsked(wf("op")) {
		t.Error("the agent asking for opus never ran")
	}
	if t.Failed() {
		t.Log("without the workflow levers the pinned CLI no longer runs five agents at once or on another " +
			"model; the probe needs updating")
	}
}

// probeWorkflowsRepair: a report repair resumes a session that ran a
// workflow with the grant cleared and has no Workflow tool.
func probeWorkflowsRepair(t *testing.T, cli string) {
	p := newPostureProbe(t, cli)
	req := p.workflowsRequest()
	req.SessionID = "00000000-0000-4000-8000-00000000a7f1"
	homeWF := filepath.Join(p.ws, ".claude", "workflows")
	first := probeScript{
		steps: [][]toolCall{
			{workflowInline("toolu_wf", "probe-first", agentOnce(wf("first")))},
			{{"toolu_plant_wf", "Bash", map[string]any{"command": "mkdir -p " + homeWF + " && cat > " + homeWF +
				"/late-wf.js <<'JS'\n" + workflowScript("late-wf", agentOnce(wf("late"))) + "JS"}}},
		},
		expect:   map[string]bool{"toolu_wf": true, "toolu_plant_wf": true},
		children: []*childScript{{marker: wf("first")}},
	}
	got := p.run(t, NewClaude().PromptSpec(p.tree, req), first)
	got.assertPinned(t, first)
	if !got.childAsked(wf("first")) {
		t.Fatal("the first run's workflow never ran")
	}

	repair := req
	repair.Subagents, repair.Workflows = false, false
	repair.Prompt = "probe: the report repair's prompt"
	resume := probeScript{
		steps: [][]toolCall{{
			workflowCallNamed("toolu_rp_late", "late-wf", ""),
			workflowInline("toolu_rp_inline", "probe-repair", agentOnce(wf("rp"))),
		}},
		expect:   map[string]bool{"toolu_rp_late": false, "toolu_rp_inline": false},
		children: []*childScript{{marker: wf("late")}, {marker: wf("rp")}},
		resumed:  true,
	}
	got = p.run(t, NewClaude().ResumeSpec(p.tree, req.SessionID, repair), resume)
	got.assertPinned(t, resume)
	for id := range resume.expect {
		if text := got.results[id].Text; !strings.Contains(text, "No such tool available") {
			t.Errorf("%s: result %s, want Workflow unavailable", id, text)
		}
	}
	if slices.Contains(got.init.Tools, "Workflow") {
		t.Errorf("the repair's init tools %v offer Workflow", got.init.Tools)
	}
	if got.childAsked(wf("late")) || got.childAsked(wf("rp")) {
		t.Error("a workflow ran in the report repair")
	}
	if got.resultEvents != 1 {
		t.Errorf("%d result events in the repair, want exactly one", got.resultEvents)
	}
}

// probeWorkflowsGrant is what a run granted workflows is granted.
var probeWorkflowsGrant = Delegation{Subagents: true, Workflows: true}

// probeWorkflowsDenied are the tools the posture still denies with
// workflows granted: every one it denies, the subagent tool and Workflow
// aside, which a workflow's agents get neither of (probeWorkflowAgentBarred).
var probeWorkflowsDenied = slices.DeleteFunc(slices.Clone(probeSubagentsDenied), func(tool string) bool {
	return tool == "Agent" || tool == "Task" || tool == claudeWorkflowTool
})

// probeWorkflowAgentBarred are the tools a workflow's agent is never
// offered: the subagent tool and Workflow itself.
var probeWorkflowAgentBarred = []string{"Agent", "Task", claudeWorkflowTool}

// probeWorkflowAgentExtra is the one tool a workflow's agent may be offered
// that the run is not: the one a built-in workflow's agent hands its
// structured result back with.
const probeWorkflowAgentExtra = "StructuredOutput"

// probeResearchArgs marks the built-in research workflow's question, so its
// agents get a script of their own.
const probeResearchArgs = "PROBE-WF-research-62d0"

// wf is the marker of one workflow agent of the workflows cases, named by its role.
func wf(role string) string { return "PROBE-WF-" + role + "-62d0" }

// workflowsRequest is the workspace-write request granted subagents and workflows.
func (p *postureProbe) workflowsRequest() PromptRequest {
	req := p.subagentsRequest()
	req.Workflows = true
	return req
}

// workflowScript is a workflow's script: its meta, then body.
func workflowScript(name, body string) string {
	return "export const meta = { name: '" + name + "', description: 'probe " + name +
		"', phases: [{ title: 'One' }] }\n" + body
}

// agentOnce is a workflow body running one agent whose prompt carries marker.
func agentOnce(marker string) string {
	return "const r = await agent('" + marker + " do the task', { label: 'a', phase: 'One' })\nreturn { r }\n"
}

// jsList is markers as a JavaScript array literal.
func jsList(markers []string) string {
	quoted := make([]string, 0, len(markers))
	for _, m := range markers {
		quoted = append(quoted, "'"+m+"'")
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// parallelBody is a workflow body running n agents in parallel.
func parallelBody(n int) string {
	markers := make([]string, 0, n)
	for i := range n {
		markers = append(markers, wf(fmt.Sprintf("par%d", i)))
	}
	return "const r = await parallel(" + jsList(markers) + ".map((m, i) => () => agent(m + ' do the task', " +
		"{ label: 'a' + i, phase: 'One' })))\nreturn { r }\n"
}

// parallelWorkflowScript asks for one inline workflow of n agents in
// parallel, their first requests meeting at a rendezvous of meet.
func parallelWorkflowScript(n, meet int) probeScript {
	children := make([]*childScript, 0, n)
	for i := range n {
		children = append(children, &childScript{marker: wf(fmt.Sprintf("par%d", i))})
	}
	return probeScript{
		steps:      [][]toolCall{{workflowInline("toolu_wf", "probe-par", parallelBody(n))}},
		expect:     map[string]bool{"toolu_wf": true},
		children:   children,
		rendezvous: meet,
	}
}

// workflowInline asks for an inline workflow.
func workflowInline(id, name, body string) toolCall {
	return toolCall{id, claudeWorkflowTool, map[string]any{"script": workflowScript(name, body)}}
}

// workflowCallNamed asks for a workflow by name, with args when set.
func workflowCallNamed(id, name, args string) toolCall {
	input := map[string]any{"name": name}
	if args != "" {
		input["args"] = args
	}
	return toolCall{id, claudeWorkflowTool, input}
}

// probeWorkflowAgentDenied is what a workflow's agent tries that it must be
// refused: the web tools, a workflow, a subagent, a wake-up, cron, a
// worktree, a read-only monitor and a background command.
func probeWorkflowAgentDenied() []toolCall {
	return []toolCall{
		{"toolu_w_fetch", "WebFetch", map[string]any{"url": "https://example.com/", "prompt": "x"}},
		{"toolu_w_search", "WebSearch", map[string]any{"query": "probe"}},
		workflowInline("toolu_w_workflow", "probe-nested", "return 1\n"),
		subagentCall("toolu_w_agent", "general-purpose", wf("gc"), "", false),
		{"toolu_w_wakeup", "ScheduleWakeup", map[string]any{"delaySeconds": 60, "reason": "probe", "prompt": "wake"}},
		{"toolu_w_cron", "CronCreate", map[string]any{"cron": "* * * * *", "prompt": "probe"}},
		{"toolu_w_worktree", "EnterWorktree", map[string]any{"name": "probe"}},
		{"toolu_w_monitor", "Monitor", map[string]any{
			"command": "sleep 2; echo PROBE-MONITOR-FIRED", "description": "probe",
		}},
		{"toolu_w_bg", "Bash", map[string]any{"command": "sleep 1", "run_in_background": true}},
	}
}

// probeWorkflowSearch has a workflow's agent load whatever ToolSearch offers
// it of the denied and barred tools.
func probeWorkflowSearch() toolCall {
	return toolCall{"toolu_w_toolsearch", "ToolSearch", map[string]any{
		"query": "select:" + strings.Join(append(slices.Clone(probeWorkflowsDenied), probeWorkflowAgentBarred...),
			",") + ",SendMessage,TaskStop,NotebookEdit",
		"max_results": 30,
	}}
}

// withSettingsEnv is argv with the --settings env block edited by edit.
func withSettingsEnv(t *testing.T, argv []string, edit func(map[string]string)) []string {
	t.Helper()
	out, env := withoutSubagentsSettings(t, argv)
	edit(env)
	b, err := json.Marshal(map[string]map[string]string{"env": env})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(argv, "--settings")
	return slices.Insert(out, i, "--settings", string(b))
}

// writeProbeFile writes body at path, its directory made first.
func writeProbeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mainThreadOutput is the output tokens the run's result events report for
// its main thread (their top-level usage).
func (o probeOutcome) mainThreadOutput() int {
	total := 0
	for line := range bytes.Lines(o.stdout) {
		var ev struct {
			Type  string `json:"type"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(line, &ev) == nil && ev.Type == "result" {
			total += ev.Usage.OutputTokens
		}
	}
	return total
}

// assertWorkflowsHeld checks what every run granted workflows must show: a
// workflow started and no subagent did; the run's own delegation check
// passes every line; the run is offered none of the denied tools; and every
// request a workflow's agent made asked for the run's model and offered only
// tools the run is offered (StructuredOutput aside), none of them denied or
// barred to it.
func (o probeOutcome) assertWorkflowsHeld(t *testing.T, model string) {
	t.Helper()
	tasks := o.tasksStarted()
	if !slices.ContainsFunc(tasks, func(task probeTask) bool { return task.Type == taskTypeWorkflow }) {
		t.Errorf("tasks started %+v: no workflow", tasks)
	}
	if slices.ContainsFunc(tasks, func(task probeTask) bool { return task.Type == taskTypeAgent }) {
		t.Errorf("tasks started %+v: a subagent started", tasks)
	}
	for line := range bytes.Lines(o.stdout) {
		if forbidden, why := NewClaude().ScanDelegation(line, probeWorkflowsGrant); forbidden {
			t.Errorf("the run's delegation check would end it: %s", why)
		}
	}
	offered := slices.Clone(o.init.Tools)
	for _, r := range o.requests {
		if r.child == "" {
			offered = append(offered, r.tools...)
		}
	}
	for _, tool := range offered {
		if slices.Contains(probeWorkflowsDenied, tool) {
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
			t.Errorf("workflow agent %s asked for model %q, want the run's %q", r.child, r.model, model)
		}
		for _, tool := range r.tools {
			switch {
			case slices.Contains(probeWorkflowsDenied, tool) || slices.Contains(probeWorkflowAgentBarred, tool):
				t.Errorf("workflow agent %s was offered %s", r.child, tool)
			case tool != probeWorkflowAgentExtra && !slices.Contains(offered, tool):
				t.Errorf("workflow agent %s was offered %s, which the run is not (run: %v)", r.child, tool, offered)
			}
		}
	}
	if children == 0 {
		t.Error("no workflow agent's request was recorded")
	}
}
