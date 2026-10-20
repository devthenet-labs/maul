Fixes #123

> [!WARNING]
> maul could not reproduce this bug before fixing it. The remediation report records no reproduction, so nothing here
> shows that a test failed on the unfixed code and passes with this change. Confirm the fix and its test before
> merging.

The agent ran with the operator's network profile `go`, so it could fetch Go modules through maul's egress proxy: review every dependency this change adds or updates in `go.mod` and `go.sum`.

remediation report here

---

_This pull request was opened automatically by maul from the remediation report above. Review and merge it like any
other change; the tracked issue moves to remediated when this merges._
