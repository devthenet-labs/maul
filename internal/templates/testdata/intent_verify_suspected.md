maul believes `version-shown` has a defect: the evaluator reported the outcome fails, and maul's own runs of the check it saved to show it, each alone under a fresh run token, failed the same assertion. maul does not fix a defect found this way on its own: a person should confirm it. An approver's `/maul verify fix` starts a fix round, handed the reproduction below; until then the preview link stays held. Known limits: an http or exec check reads right after it writes, so an application that updates asynchronously or serves a cached read for a few seconds can look like this; a `clickable` check judges one moment, so a passing toast or spinner over the element can too; and an exec check's failed assertion is whatever its program reports, so a program that reports every failure that way can look like this too.

maul's reproduction:

```text
POST /notes {"name":"tok-v"} answered 201
GET /version?q=tok answered {"note":"old"}
See # 12 and @ octocat.
```
