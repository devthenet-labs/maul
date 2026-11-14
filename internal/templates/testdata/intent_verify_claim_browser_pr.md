<!-- maul:notice maul/marigold-3 preview -->
### maul: preview

maul couldn't confirm this: the evaluator judged 1 outcome not to hold, but no check maul re-ran shows it. This pull request's head, `9f1c0e5d2a7b`, is live at [`marigold-3.preview.maul.example.com`](https://marigold-3.preview.maul.example.com), under `/api`.

#### maul: acceptance checks

Evaluation 2: maul couldn't confirm every outcome: 4/5 outcomes confirmed, 0 of 2 automatic fixes used.

- `saved-line-readable` (`change`): unsure: maul couldn't confirm it; the evaluator says it does not hold, but no check maul re-ran shows it. The Saved line is readable on the panel.

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

_maul updates this comment as the preview changes, and removes the preview when the intent ends._
