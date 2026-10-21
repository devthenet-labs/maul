<!-- maul:plan maul/target-1 r2 sha256:5f051fa7769d -->
## Plan r2

**Summary:** Show the API's greeting on the home page

**Repositories:** maul opens one pull request in each of: `devthenet-labs/marigold-web`, `devthenet-labs/Acme.Web_App`.

### Before you approve

Each build runs in the image its own repository declares. The build of each repository listed here fetches new dependencies through maul's egress proxy, as its network profile allows: `devthenet-labs/Acme.Web_App` (network profile `web`: any npm package `ca-npm` serves; the PyPI projects the operator allows from `ca-pypi`). Every other build runs offline, so it cannot fetch anything new. This plan adds dependencies: each one a build cannot fetch that way must be in the image of the repository that needs it, on that repository's default branch, before you approve, or its build will fail.

- `github.com/acme/jsonx v1.2.0 (devthenet-labs/Acme.Web_App)`

### To approve

Add the `maul:approved` label to this issue, or comment `/maul approve`. maul then builds exactly the plan below: r2, `sha256:5f051fa7769d`. If this comment or the issue description is edited after maul posted the plan, the approval is refused and maul asks for a new plan.

For a different plan, say what should change in a comment, then re-apply the `maul:target` label or comment `/maul replan`. `/maul cancel` stops work on this intent.

### The plan

This is the planner's report exactly as the build agent reads it, byte for byte, with nothing in it rendered or left out. GitHub does not wrap it, so scroll it sideways to read a long line to its end. Its digest is `sha256:5f051fa7769d869ea3c54312cc94f750c7f5b67ff9b871fae9f89999e4da4342`.

```markdown
---
summary: "Show the API's greeting on the home page"
repositories:
  - "https://github.com/devthenet-labs/marigold-web"
  - "https://github.com/devthenet-labs/Acme.Web_App"
new_dependencies: []
questions: []
confidence: 0.8
estimated_max_turns: 40
estimated_token_budget: 200000
---

## Approach

The API serves `GET /api/greeting` as `{"message": string}`; the page fetches it same-origin.

## Steps

### https://github.com/devthenet-labs/Acme.Web_App

1. Add the handler.

### https://github.com/devthenet-labs/marigold-web

1. Fetch `/api/greeting` and fill `#greeting`.
```
