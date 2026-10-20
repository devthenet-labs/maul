Fixes #123

> [!WARNING]
> maul did not build or test this change. It ran on maul's default agent image, which carries none of this
> repository's toolchain, so whatever the report below says about verification, nothing was compiled or run. Build and
> test it (CI) before merging. maul builds and tests its fixes only in an [agent image](https://devthenet-labs.github.io/maul/integrations/agent-images/) the
> repository declares and maul can run.

> [!WARNING]
> maul could not reproduce this bug before fixing it. The remediation agent claims it reproduced the bug, but it ran
> on maul's default agent image, where none of this repository's tests can run, so maul does not accept that
> claim. The agent's account is quoted below; treat it as unverified and confirm the fix and its test before merging.

### Reproduction

Test command: `go test ./internal/render -run TestEscapesName`

Without the fix:

```text
--- FAIL: TestEscapesName (0.00s)
FAIL
```

With the fix:

```text
ok  	example.com/app/internal/render	0.012s
```

remediation report here

---

_This pull request was opened automatically by maul from the remediation report above. Review and merge it like any
other change; the tracked issue moves to remediated when this merges._
