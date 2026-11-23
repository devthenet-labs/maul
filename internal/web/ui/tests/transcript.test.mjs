// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

import assert from "node:assert/strict";
import { test } from "node:test";
import { flattenTurns, nestTurns, transcriptLines } from "../src/transcript.ts";

let seq = 0;
const turn = (kind, extra = {}) => ({ seq: ++seq, role: kind === "tool_result" ? "user" : "assistant", kind, ...extra });
const notice = (text, parent) => ({ seq: ++seq, role: "system", kind: "notice", text, parent });

// shape is a nested tree as [seq, children] pairs, so a test reads the tree
// it expects.
const shape = (nodes) => nodes.map((n) => (n.children.length ? [n.turn.seq, shape(n.children)] : n.turn.seq));

test("a subagent's turns nest under the Agent call that ran them", () => {
  seq = 0;
  const turns = [
    turn("tool_use", { tool: "Bash", call: "p_bash" }), // 1
    turn("tool_result"), // 2
    turn("tool_use", { tool: "Agent", call: "t_agent" }), // 3
    notice("subagent general-purpose started", "t_agent"), // 4
    turn("tool_use", { tool: "Bash", call: "c_1", parent: "t_agent" }), // 5
    turn("tool_result", { parent: "t_agent" }), // 6
    notice("delegated work finished: completed, 1 tool use", "t_agent"), // 7
    turn("tool_result"), // 8: the hand-back, on the main thread
    turn("text", { text: "done" }), // 9
  ];
  assert.deepEqual(shape(nestTurns(turns)), [1, 2, [3, [4, 5, 6, 7]], 8, 9]);
});

test("two Agent calls in one message keep their own children, however interleaved", () => {
  seq = 0;
  const turns = [
    turn("tool_use", { tool: "Agent", call: "t_a" }), // 1
    turn("tool_use", { tool: "Agent", call: "t_b" }), // 2
    turn("tool_use", { tool: "Bash", call: "a_1", parent: "t_a" }), // 3
    turn("tool_use", { tool: "Bash", call: "b_1", parent: "t_b" }), // 4
    turn("tool_result", { parent: "t_b" }), // 5
    turn("tool_result", { parent: "t_a" }), // 6
    turn("tool_result"), // 7
    turn("tool_result"), // 8
  ];
  assert.deepEqual(shape(nestTurns(turns)), [[1, [3, 6]], [2, [4, 5]], 7, 8]);
});

test("a subagent's own subagent nests under its Agent call", () => {
  seq = 0;
  const turns = [
    turn("tool_use", { tool: "Agent", call: "t_agent" }), // 1
    turn("tool_use", { tool: "Agent", call: "c_nested", parent: "t_agent" }), // 2
    notice("subagent Explore started", "c_nested"), // 3
    notice("delegated work finished: completed", "c_nested"), // 4
    turn("tool_result", { parent: "t_agent" }), // 5
  ];
  assert.deepEqual(shape(nestTurns(turns)), [[1, [[2, [3, 4]], 5]]]);
});

test("a turn whose call is not in the record stays where it falls, marked a subagent's", () => {
  seq = 0;
  // The Agent call was before a live view joined, or past the recording cap.
  const turns = [turn("tool_use", { tool: "Bash", call: "c_1", parent: "t_gone" }), turn("text", { text: "x" })];
  const nodes = nestTurns(turns);
  assert.deepEqual(shape(nodes), [1, 2]);
  assert.equal(nodes[0].subagent, true);
  assert.equal(nodes[1].subagent, false);
});

test("the recording cap's notice mid-subagent closes the record at the top", () => {
  seq = 0;
  const turns = [
    turn("tool_use", { tool: "Agent", call: "t_agent" }), // 1
    turn("tool_use", { tool: "Bash", call: "c_1", parent: "t_agent" }), // 2
    { seq: ++seq, role: "system", kind: "notice", text: "transcript truncated: 500 turn cap reached", truncated: true }, // 3
  ];
  assert.deepEqual(shape(nestTurns(turns)), [[1, [2]], 3]);
});

test("a turn may only nest under an earlier call, never under itself or a later one", () => {
  seq = 0;
  const turns = [
    turn("tool_use", { tool: "Bash", call: "x", parent: "x" }), // 1: its own parent
    turn("tool_use", { tool: "Bash", call: "y", parent: "z" }), // 2: a later call
    turn("tool_use", { tool: "Agent", call: "z" }), // 3
  ];
  assert.deepEqual(shape(nestTurns(turns)), [1, 2, 3]);
});

test("flattenTurns walks the tree in order with each turn's depth", () => {
  seq = 0;
  const turns = [
    turn("tool_use", { tool: "Agent", call: "t_agent" }),
    turn("tool_use", { tool: "Agent", call: "c_nested", parent: "t_agent" }),
    notice("subagent started", "c_nested"),
    turn("tool_result"),
  ];
  assert.deepEqual(
    flattenTurns(nestTurns(turns)).map((r) => [r.turn.seq, r.depth]),
    [
      [1, 0],
      [2, 1],
      [3, 2],
      [4, 0],
    ],
  );
});

test("transcriptLines indents a subagent's turns as plain text", () => {
  seq = 0;
  const turns = [
    turn("tool_use", { tool: "Agent", call: "t_agent", text: "probe child" }),
    turn("tool_use", { tool: "Bash", call: "c_1", parent: "t_agent", text: "go test\n./..." }),
    turn("tool_result", { text: "ok" }),
  ];
  assert.equal(
    transcriptLines(turns),
    ["[tool_use Agent]", "probe child", "", "  | [tool_use Bash, subagent]", "  | go test", "  | ./...", "", "[tool_result]", "ok"].join(
      "\n",
    ),
  );
});

test("transcriptLines of a run without subagents is the plain text RunPanel always showed", () => {
  const turns = [
    { seq: 1, role: "system", kind: "notice", text: "session s started", at: "2026-11-23T07:09:29Z" },
    { seq: 2, role: "assistant", kind: "tool_use", tool: "Bash", text: "go test", call: "p_bash" },
    { seq: 3, role: "user", kind: "tool_result", text: "ok", truncated: true },
  ];
  // RunPanel's turnText before subagents existed.
  const old = (t) =>
    `[${t.kind}${t.tool ? ` ${t.tool}` : ""}${t.at ? ` ${t.at}` : ""}]\n${t.text ?? ""}${t.truncated ? "\n(truncated by the recorder)" : ""}`;
  assert.equal(transcriptLines(turns), turns.map(old).join("\n\n"));
});
