<!-- maul:notice maul/beatbattle-44 verify-r2 -->
### maul: verification 2, attempt 1

Verification 2 failing: 0/1 checks passing, 1 of 2 automatic fixes used.

| Check | Kind | Outcome |
| --- | --- | --- |
| `ratings-order` | `http` | failed |

**`ratings-order` failed.** Expected `GET /api/ratings: status 200`. Observed `status 200`.

- JSON `/0/id`: expected `"p_mine"`, observed ```"p_4e1013 ``@octocat``"```.
- header `Content-Type`: expected `"application/json"`, observed nothing there.
- body: expected it to contain `p_mine`.
- capture `battle`: expected `a value at /id`, observed nothing there.

_maul edits this comment as the attempts of this verification finish._
