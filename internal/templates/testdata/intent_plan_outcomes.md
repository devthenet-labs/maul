<!-- maul:plan maul/target-1 r2 sha256:fa2aaa568050 -->
## Plan r2

**Summary:** Add GET /version returning {sha, built} as JSON

### Acceptance outcomes

maul's evaluator checks each outcome below in the running preview; a manual one is left to an approver.

- `rows-scroll`: the two rows scroll together, 40 px a step. It answers the issue's line: Rows scroll together
- `still-loads` (must keep holding): the leaderboard still loads
- `sounds-good` (manual: only a human hears it): it sounds good

No outcome answers these lines of the issue, so nothing checks them. If one should be checked, ask for a new plan:

- Show the pattern's author

### Before you approve

The plan has 1 line longer than 100 columns. GitHub does not wrap a code block, so on most screens such a line runs past the right edge of the block below: scroll the block sideways to read each to its end, since at the edge a line can look finished when it is not.

### To approve

Add the `maul:approved` label to this issue, or comment `/maul approve`. maul then builds exactly the plan below: r2, `sha256:fa2aaa568050`. If this comment or the issue description is edited after maul posted the plan, the approval is refused and maul asks for a new plan.

For a different plan, say what should change in a comment, then re-apply the `maul:target` label or comment `/maul replan`. `/maul cancel` stops work on this intent.

### The plan

This is the planner's report exactly as the build agent reads it, byte for byte, with nothing in it rendered or left out. GitHub does not wrap it, so scroll it sideways to read a long line to its end. Its digest is `sha256:fa2aaa568050158cd1f3fafe7853e0e38e0d2011515785634610e92b84a43982`.

```markdown
---
summary: Add GET /version returning {sha, built} as JSON
repositories: [maul-target]
new_dependencies: []
questions: []
confidence: high
estimated_max_turns: 60
estimated_token_budget: 300000
---

## Approach

Add a `GET /version` handler in `internal/server` that returns the commit SHA and build time, set with `-ldflags` at build time.

## Steps

1. Add `version.go` with `Commit` and `Built` variables.
2. Register the handler next to `/healthz`.

## Test plan

- a handler test asserting the JSON shape
- `go test ./...`

## Risks

A new public endpoint; it reveals the deployed commit, which is public anyway.
```
