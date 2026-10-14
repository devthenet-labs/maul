Fixes #123

The remediation agent reports that it reproduced the bug before fixing it: it wrote the regression test first, saw it
fail on the unfixed code, then saw it pass with the fix. This is the agent's own account, quoted below; patchy did not
run the test itself.

### Reproduction

Test command: `go test ./internal/render -run TestEscapesName`

Without the fix:

```text
--- FAIL: TestEscapesName (0.00s)
    render_test.go:14: got "<b>", want "&lt;b&gt;"
FAIL
```

With the fix:

```text
ok  	example.com/app/internal/render	0.012s
```

remediation report here

---

_This pull request was opened automatically by patchy from the remediation report above. Review and merge it like any
other change; the tracked issue moves to remediated when this merges._
