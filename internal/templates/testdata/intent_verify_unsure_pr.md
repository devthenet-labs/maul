<!-- maul:notice maul/marigold-3 preview -->
### maul: preview

maul couldn't confirm this: some outcomes are unsure. This pull request's head, `9f1c0e5d2a7b`, is live at [`marigold-3.preview.maul.example.com`](https://marigold-3.preview.maul.example.com), under `/api`.

#### maul: acceptance checks

Evaluation 1: maul couldn't confirm every outcome: 1/2 outcomes confirmed, 0 of 2 automatic fixes used.

- `light-sets-swing` (`change`): confirmed; maul's check: `browser: goto /, click button Light; 1 expectation`. Light sets the Swing slider to 20.
- `presets-visible-clickable` (`change`): unsure: maul couldn't confirm it; no check maul re-ran covers it. The presets sit right after the Swing slider, visible and clickable.

The evaluator was unsure of `presets-visible-clickable` too; its note (its words, not checked by maul): The presets wrap onto the line below the Swing slider at 1280px; whether a wrapped row still counts as right after the slider is a layout reading for a person.

_maul updates this comment as the preview changes, and removes the preview when the intent ends._
