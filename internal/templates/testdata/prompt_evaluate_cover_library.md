## Outcomes you passed with no check

Your report at `/work/scratch/report.md` gives each outcome below `pass`, but maul has no check to count for it: your
report proposes no check bound to it, and no saved check of it passed in an earlier round:

- `repeat-others-kept`
- `repeat-no-mutation`
- `repeat-cycles`
- `repeat-16`
- `repeat-1`
- `repeat-bad-length`
- `repeat-bad-track`
- `validate-kept`

maul passes an outcome only on a check bound to that outcome. A run that showed several outcomes at once counts only
for the outcome its check is bound to, and your own exploring calls count for none, so as your report stands each of
these outcomes stays unconfirmed, and a person has to ask for another round.

This is ask 1 of at most 2.

For each outcome above:

- If a saved check bound to it replayed green this round (`maul-tool check results`) and your instructions do not
  list it under **Saved checks that are not yet proof**, that check already counts: leave the outcome as it is.
- Otherwise write a check bound to it, held to **What a check must cover**, as a new file under
  `/work/scratch/checks`: one check per outcome. A check of another outcome that already asserts this one is a fine
  start: copy its program into a new file, bind it to this outcome by its id, and keep every assertion of this
  outcome's clauses.
- Run it with `maul-tool check run <file>`, then `maul-tool check run --base <file>`, and fix it until it runs green
  at the head, as your instructions say.
- Add it to `proposed` in your report by its file.
- If no check you can write runs green and shows the outcome as worded, change its verdict to `unsure` and say why in
  its note. Never weaken a check, drop a clause or change an input the outcome names to get a green run. If a check
  shows the outcome does not hold, report it as your instructions say for a fail; never fail an outcome only because
  no check of it ran green.

Then rewrite your report with these changes only: the new entries under `proposed`, and the verdicts of the outcomes
above (their `verdict`, `ledger` and `note`; keep every `contrary` seq). Keep everything else in its
frontmatter exactly as it is: every other verdict, every proposal, dispute and retirement, and every anomaly. maul
refuses a rewrite that changes anything else, and keeps the report you wrote before. When it is done, reply with one
short line.
