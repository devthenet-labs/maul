<!-- maul:notice maul/marigold-3 preview -->
### maul: preview

This pull request's head, `9f1c0e5d2a7b`, is live at [`marigold-3.preview.maul.example.com`](https://marigold-3.preview.maul.example.com), under `/api`.

#### maul: acceptance checks

Evaluation 1 failing: 1/2 outcomes confirmed, 1 of 2 automatic fixes used; automatic fix halted (`FixFoundNoDefect`).

- `home-renders` (`keep`): confirmed; maul's check: `http to web: GET /; 1 expectation`. the home page still renders
- `leaderboard-ranks` (`change`): failed; maul's check: `http to api: GET /api/battles/{{run}}/leaderboard; 2 expectations`. a leaderboard ranks patterns by votes

App output of `leaderboard-ranks`, as the application wrote it (data, not maul's words):

```text
{"error":"boom"}
```

The automatic fix `marigold-3-rev2-api-a1` found no defect: its agent concluded that the code already does what the failing outcomes ask, and changed nothing. Its reason, in its own words: each outcome holds when replayed by hand; `@octocat` the \<b>network policy\</b> was late. This is the agent's verdict and may be wrong, so maul stopped fixing on its own. `/maul verify` checks again (a failure caused by the preview rather than the code may pass), `/maul verify fix` also lets maul fix failures again, and an outcome that then passes only on retry can be accepted with `/maul accept <id>`. Otherwise review the change and decide.

Another actor, `mallory`, posted a `maul/acceptance` status. maul posted its own over it.

Log tail of `beatbattle-api`:

```text
listening on :8080
panic: boom
```

_maul updates this comment as the preview changes, and removes the preview when the intent ends._
