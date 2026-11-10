<!-- maul:notice maul/marigold-3 preview -->
### maul: evaluation

Ready: maul checked the approved outcomes. This pull request's head is `9f1c0e5d2a7b`.

#### maul: acceptance checks

Evaluation 1: maul couldn't confirm every outcome: 1/2 outcomes confirmed, 0 of 2 automatic fixes used.

- `onsets-500ms` (`change`): confirmed; maul's check: `run (go) of {{bin}}: 1 file; exit 0, stdout JSON`. `bbrender --bpm 120 kick.json` writes kick onsets 500 ms apart.
- `bpm-flag` (`change`): unsure: maul couldn't confirm it; the program failed without asserting anything about the change; maul's check: `exec of {{bin}}: --bpm 90; exit 0, stdout contains`. `bbrender --bpm 90` writes a slower beat.

_maul updates this comment as it checks the change._
