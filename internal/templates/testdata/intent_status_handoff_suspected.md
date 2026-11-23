<!-- maul:intent maul/target-1 -->
### maul

**Phase:** `InReview`: the pull requests are open for review.

**Plan:** [r2](https://github.com/devthenet-labs/intents/issues/1#issuecomment-99): Add GET /version returning {sha, built} as JSON

**Approved:** plan r2, by `peter`.

**Pull requests:**

- [devthenet-labs/maul-target#12](https://github.com/devthenet-labs/maul-target/pull/12) (open)

**Preview:** maul can't call this ready: it suspects a defect in `swing-shown-nonzero`, which a person should confirm, and the evaluator judged 1 outcome not to hold, but cited no evidence maul counts. [`target-1.preview.maul.example.com`](https://target-1.preview.maul.example.com) serves `4b825dc642cb`. Evaluation 4: maul can't call this ready: 3/5 outcomes seen to hold, 1 of 2 automatic fixes used.

**Commands now:** `/maul cancel`

_maul updates this comment as the work progresses._
