Fixes #123

> [!WARNING]
> maul could not reproduce this bug before fixing it. The remediation agent reports that its regression test did not
> fail on the unfixed code, or could not be run there, so nothing here shows that this change fixes the reported
> problem. The report below says why. Confirm the fix and its test before merging.

### Reproduction

Test command: `go test ./internal/render -run TestEscapesName`

Without the fix:

```text
ok  	example.com/app/internal/render	0.010s
```

remediation report here

---

_This pull request was opened automatically by maul from the remediation report above. Review and merge it like any
other change; the tracked issue moves to remediated when this merges._
