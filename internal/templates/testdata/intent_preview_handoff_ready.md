<!-- maul:notice maul/marigold-3 preview -->
### maul: preview

Ready: maul checked the approved outcomes. This pull request's head, `9f1c0e5d2a7b`, is live at [`marigold-3.preview.maul.example.com`](https://marigold-3.preview.maul.example.com).

#### maul: acceptance checks

Evaluation 2 failing: 2/4 outcomes confirmed, 1 of 2 automatic fixes used; automatic fix queued.

- `winner-rises` (`outcome`): confirmed. Rating a battle moves the winner above the loser.
- `upset-gains-more` (`outcome`): failed. Beating a higher-rated player gains more points.
- `leaderboard-loads` (`outcome`): unsure: maul couldn't confirm it. The leaderboard still lists every player.
- `looks-right` (`outcome`): accepted by a human at `4b825dc642cb`, not re-checked. The board looks right.

`winner-rises`: confirmed. maul's check: `http to api: POST /api/players, POST /api/battles, GET /api/leaderboard; 3 expectations`.

`upset-gains-more`: failed. maul's check: `http to api: POST /api/battles, GET /api/players/{{run}}-a; 2 expectations`.

App output, as the application wrote it (data, not maul's words):

````text
step 2: expected /delta 24, got 16
@ octocat see # 12 fixes # 3
```
</details>
````

`leaderboard-loads`: unsure: maul couldn't confirm it; the evaluator says it holds, but no check maul re-ran proves it.

Look-around: maul's evaluator noticed (its words, not checked by maul): console error on /battle `@octocat` `#7`, on `/battle`.

_maul updates this comment as the preview changes, and removes the preview when the intent ends._
