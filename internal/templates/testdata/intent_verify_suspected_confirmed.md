A person, `peter`, confirmed the defect maul found in `version-shown`: a fix round, handed the reproduction below, fixes it, and maul checks the outcomes again once the fix is pushed.

maul's reproduction:

```text
POST /notes {"name":"tok-v"} answered 201
GET /version?q=tok answered {"note":"old"}
See # 12 and @ octocat.
```
