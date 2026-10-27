<!-- maul:notice maul/marigold-3 verify-r1 -->
### maul: verification 1, attempt 2

Verification 1 failing: 2/4 checks passing, 0 of 2 automatic fixes used, 1 quarantined, 1 for a human; automatic fix queued.

| Check | Kind | Outcome |
| --- | --- | --- |
| `render` | `http` | passed |
| `battle-leaderboard` | `http` | failed |
| `flaky` | `http` | quarantined: passed only on retry, not counted as passed |
| `kick-onsets` | `command` | not run yet |
| `sounds-good` | `manual` | needs a human |

**`battle-leaderboard` failed.** Expected `GET /api/battles/b1/leaderboard: status 200, /0/id = p2`. Observed `status 500`.

```text
{"error":"boom"}
```

_maul edits this comment as the attempts of this verification finish._
