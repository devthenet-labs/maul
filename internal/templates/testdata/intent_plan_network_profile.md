<!-- maul:plan maul/target-1 r2 sha256:05126c8813d2 -->
## Plan r2

**Summary:** Add GET /version returning {sha, built} as JSON

### Before you approve

The plan has 1 line longer than 100 columns. GitHub does not wrap a code block, so on most screens such a line runs past the right edge of the block below: scroll the block sideways to read each to its end, since at the edge a line can look finished when it is not.

The build runs in the image this repository declares, with the operator's network profile `go`: it fetches new Go modules through maul's egress proxy, but only tagged releases of module paths the operator allows. This plan adds dependencies: each one the build cannot fetch that way (a module outside those paths, a branch or commit version, a dependency of any other kind) must be in that image on the default branch before you approve, or the build will fail.

- `github.com/google/uuid v1.6.0`
- `libvips 8.15`

### To approve

Add the `maul:approved` label to this issue, or comment `/maul approve`. maul then builds exactly the plan below: r2, `sha256:05126c8813d2`. If this comment or the issue description is edited after maul posted the plan, the approval is refused and maul asks for a new plan.

For a different plan, say what should change in a comment, then re-apply the `maul:target` label or comment `/maul replan`. `/maul cancel` stops work on this intent.

### The plan

This is the planner's report exactly as the build agent reads it, byte for byte, with nothing in it rendered or left out. GitHub does not wrap it, so scroll it sideways to read a long line to its end. Its digest is `sha256:05126c8813d281315916c9f1f41d09f98dd4ddc8a8f258a3ca8a5af18d593fcf`.

```markdown
---
summary: Add GET /version returning {sha, built} as JSON
repositories: [patchy-target]
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
