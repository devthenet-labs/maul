// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// How a transcript's turns are laid out: a subagent's turns nest under the
// Agent call that ran them. Each tool call turn names its call and each turn
// a subagent printed names the call it ran under (TranscriptTurn's call and
// parent; internal/transcript.Turn), so the nesting is by id, never by where
// a turn falls: two subagents running at once keep their own turns. Pure, so
// both transcript views (the finding's Conversation, the intent run's panel)
// lay it out alike and a test can hold them to it.

import type { TranscriptTurn } from "./types";

export interface TurnNode {
  turn: TranscriptTurn;
  // children are the turns that ran under this one's call, in order.
  children: TurnNode[];
  // subagent is set on a turn a subagent printed, nested or not: one whose
  // call is not in the record (before a live view joined, past the recording
  // cap, or an id the recorder dropped) stays where it falls, marked.
  subagent: boolean;
}

// nestTurns arranges turns, in order, under the earlier call each names as
// its parent; a turn whose parent is not an earlier call stays at the level
// it would otherwise be. A call id seen twice nests what follows under its
// latest turn.
export function nestTurns(turns: TranscriptTurn[]): TurnNode[] {
  const roots: TurnNode[] = [];
  const byCall = new Map<string, TurnNode>();
  for (const turn of turns) {
    const node: TurnNode = { turn, children: [], subagent: !!turn.parent };
    const under = turn.parent ? byCall.get(turn.parent) : undefined;
    (under ? under.children : roots).push(node);
    if (turn.call) byCall.set(turn.call, node);
  }
  return roots;
}

export interface TurnRow {
  turn: TranscriptTurn;
  depth: number;
  subagent: boolean;
}

// flattenTurns walks a nested transcript in order, each turn with its depth.
export function flattenTurns(nodes: TurnNode[], depth = 0): TurnRow[] {
  const out: TurnRow[] = [];
  for (const n of nodes) {
    out.push({ turn: n.turn, depth, subagent: n.subagent });
    out.push(...flattenTurns(n.children, depth + 1));
  }
  return out;
}

// SUBAGENT_INDENT marks each line of a nested turn in plain text, once per
// level.
export const SUBAGENT_INDENT = "  | ";

function turnText(t: TranscriptTurn, subagent: boolean): string {
  const head = `[${t.kind}${t.tool ? ` ${t.tool}` : ""}${t.at ? ` ${t.at}` : ""}${subagent ? ", subagent" : ""}]`;
  return `${head}\n${t.text ?? ""}${t.truncated ? "\n(truncated by the recorder)" : ""}`;
}

// transcriptLines is a transcript as plain text: each turn a bracketed head
// and its text, a blank line between turns, a subagent's turns indented
// under their call. A run without subagents reads as it always did.
export function transcriptLines(turns: TranscriptTurn[]): string {
  return flattenTurns(nestTurns(turns))
    .map(({ turn, depth, subagent }) => {
      const indent = SUBAGENT_INDENT.repeat(depth);
      return turnText(turn, subagent)
        .split("\n")
        .map((line) => indent + line)
        .join("\n");
    })
    .join("\n\n");
}
