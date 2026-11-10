<!-- maul:notice maul/target-1 verify-r2 -->
### maul: verification 2, attempt 1

Evaluation 2 failing: 2/4 outcomes confirmed, 1 of 2 automatic fixes used; automatic fix queued.

| Check | Kind | Outcome |
| --- | --- | --- |
| `winner-rises` | `change` | confirmed; maul's check: `http to api: POST /api/players, POST /api/battles, GET /api/leaderboard; 3 expectations` |
| `upset-gains-more` | `change` | failed; maul's check: `http to api: POST /api/battles, GET /api/players/{{run}}-a; 2 expectations` |
| `leaderboard-loads` | `keep` | unsure: maul couldn't confirm it; the evaluator says it holds, but no check maul re-ran proves it |
| `looks-right` | `change` | accepted by a human at `4b825dc642cb`, not re-checked |

App output of `upset-gains-more`, as the application wrote it (data, not maul's words):

````text
step 2: expected /delta 24, got 16
@ octocat see # 12 fixes # 3
```
</details>
````

Look-around: maul's evaluator noticed (its words, not checked by maul): console error on /battle `@octocat` `#7`, on `/battle`.

_maul edits this comment as the attempts of this verification finish._
