<!-- maul:notice maul/marigold-3 preview -->
### maul: preview

This pull request's head, `9f1c0e5d2a7b`, is live at [`marigold-3.preview.maul.example.com`](https://marigold-3.preview.maul.example.com), under `/api`.

#### maul: acceptance checks

Verification failing: 2/4 checks passing, round 1 of 2, 1 quarantined, 1 for a human; automatic fix queued.

- `render` (`http`): passed. the home page renders
- `battle-leaderboard` (`http`): failed. a leaderboard ranks patterns by votes
- `flaky` (`http`): quarantined: passed only on retry, not counted as passed. a flaky one
- `kick-onsets` (`command`): not run yet. kicks land on the beat
- `sounds-good` (`manual`): needs a human. it sounds good

**`battle-leaderboard` failed.** Expected `GET /api/battles/b1/leaderboard: status 200, /0/id = p2`. Observed `status 500`.

```text
{"error":"boom"}
```

_maul updates this comment as the preview changes, and removes the preview when the intent ends._
