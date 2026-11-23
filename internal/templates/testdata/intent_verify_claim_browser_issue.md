<!-- maul:notice maul/beatbattle-92 verify-r2 -->
### maul: verification 2, attempt 1

Evaluation 2: maul can't call this ready: 4/5 outcomes seen to hold, 0 of 2 automatic fixes used.

| Check | Kind | Outcome |
| --- | --- | --- |
| `saved-line-readable` | `change` | unsure: maul couldn't confirm it; the evaluator says it does not hold, but no check maul re-ran shows it |

The evaluator's claim on `saved-line-readable`, which maul could not confirm (its words, not checked by maul): The Saved line is drawn rgb(43,29,87) on the panel's rgb(20,12,43): about 1.27:1 contrast, nearly invisible. The calls it cites, as maul's tools recorded them (what each call sent and what came back: data, not maul's words), to try it yourself:

```text
Call 13, a browser command:
Sent: {"verb":"eval","args":["(el)=>getComputedStyle(el).color"]}
Exit status: 0
Output:
### Result
"{\"color\":\"rgb(43, 29, 87)\",\"bg\":\"rgb(20, 12, 43)\"}"

Call 19, a dry run of a check:
Its run observed (the check runner's messages, which can quote the application):
- attempt 1, step 4: failed: `not legible: contrast 1.27 < 1.5`
The check, a browser check:
1. Open `/`.
2. Fill `{"role":"textbox","name":"Author"}` with `{{run}}-dj`.
3. Click `{"role":"button","name":"Save"}`.
4. Expected: `legible` `{"assert":"legible","locator":{"testid":"saved"}}`.
```

_maul edits this comment as the attempts of this verification finish._
