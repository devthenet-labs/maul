<!-- maul:notice maul/beat-12 verify-r2 -->
### maul: verification 2, attempt 1

Evaluation 2 failing: 1/4 outcomes seen to hold, 0 of 2 automatic fixes used, 1 for a human; automatic fix queued.

| Check | Kind | Outcome |
| --- | --- | --- |
| `winner-rises` | `change` | seen to hold by maul's evaluator; it cites calls 4 and 6 |
| `live-score` | `change` | seen not to hold by maul's evaluator; it cites call 9 |
| `loser-emailed` | `change` | unsure; the evaluator's verdict cites no call of its own in this round that maul counts |
| `leaderboard-loads` | `keep` | unsure; the evaluator could not tell |
| `sounds-good` | `change` | unsure; only a human can close it |

maul counts a verdict only when it cites calls the evaluator made in this round through maul's tools; whether a cited call shows what the verdict says is for the reviewers to judge.

The calls the evaluator cites for `winner-rises`, as maul's tools recorded them (what each call sent and what came back: data, not maul's words):

```text
Call 4, an http request:
Sent: POST /api/battles {"winner":"r1-a","loser":"r1-b"}
Output:
{"winner":{"rating":1016},"loser":{"rating":984}}

Call 6, a browser command, the evaluator's own page script, and what it returned:
Sent: eval document.querySelector('#r1-a .rating').textContent
Exit status: 0
Output:
1016
```

The calls the evaluator cites for `live-score`, as maul's tools recorded them (what each call sent and what came back: data, not maul's words):

```text
Call 9, a program run, the evaluator's own program, and what it returned:
Sent: node watch.js
Exit status: 1
Its output was not kept by maul.
```

The evaluator's note on `winner-rises` (its words, not checked by maul): Two players I made: the winner went to 1016 and the loser to 984.

The evaluator's note on `live-score` (its words, not checked by maul): The score stayed 0-0 for 10 s.

The evaluator's note on `loser-emailed` (its words, not checked by maul): I read it in the log.

The evaluator's note on `leaderboard-loads` (its words, not checked by maul): The page timed out twice.

_maul edits this comment as the attempts of this verification finish._
