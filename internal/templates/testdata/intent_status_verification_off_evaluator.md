<!-- maul:intent maul/target-1 -->
### maul

**Phase:** `InReview`: the pull requests are open for review.

**Plan:** [r2](https://github.com/devthenet-labs/intents/issues/1#issuecomment-99): Add GET /version returning {sha, built} as JSON

**Approved:** plan r2, by `peter`.

**Pull requests:**

- [devthenet-labs/maul-target#12](https://github.com/devthenet-labs/maul-target/pull/12) (open)

**Preview:** [`target-1.preview.maul.example.com`](https://target-1.preview.maul.example.com) serves `4b825dc642cb`.

**Verification:** off: intent-controller runs without the agent evaluator (`--intent-evaluator`), which is what verifies intents, so its acceptance outcomes are not checked and its preview is handed over at once. The Project's `VerificationOff` condition says the same.

**Commands now:** `/maul cancel`

_maul updates this comment as the work progresses._
