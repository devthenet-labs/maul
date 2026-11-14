<!-- maul:notice maul/bbcount-96 verify-r1 -->
### maul: verification 1, attempt 1

Evaluation 1: maul couldn't confirm every outcome: 1/2 outcomes confirmed, 0 of 2 automatic fixes used.

| Check | Kind | Outcome |
| --- | --- | --- |
| `offbeat-plain-example` | `change` | confirmed; maul's check: `exec: {{bin}} -offbeat pattern.json; 2 expectations` |
| `offbeat-rule` | `change` | unsure: maul couldn't confirm it; the evaluator says it does not hold, but no check maul re-ran shows it |

The evaluator's claim on `offbeat-rule`, which maul could not confirm (its words, not checked by maul): The plain path never resets its counter (offbeat.go, n declared outside the loop) `@octocat` closes `#12` \`\`\` \</details> \<b>x\</b>. The calls it cites, as maul's tools recorded them (what each call sent and what came back: data, not maul's words), to try it yourself:

`````text
Call 14, a dry run of a check:
maul's own run of this check, call 27, observed (the check runner's messages, which can quote the application):
- attempt 1, step 1: failed: `the program printed its assertion marker: offbeat-rule: got "offbeat kick 2\noffbeat snare 3\noffbeat hat 3\n"`
The check, a Go program built against the pull request's tree:
1. Write the file `main.go`:

   ```text
   package main

   import (
   	"fmt"
   	"os"
   	"os/exec"
   )

   func main() {
   	os.WriteFile("p.json", []byte(`{"tracks":{"kick":[false,true,true]}}`), 0o644)
   	out, _ := exec.Command("{{bin}}", "-offbeat", "p.json").Output()
   	want := "offbeat kick 2\noffbeat snare 1\noffbeat hat 0\n"
   	if string(out) != want {
   		fmt.Printf("{{marker}} offbeat-rule: got %q\n", out)
   		return
   	}
   	fmt.Println("ok {{run}}")
   }
   ```
2. maul writes the program's `go.mod` and builds it with the tree in a Go workspace.
3. Run it (timeout `30s`). Expected: exit status `0`; standard output contains `ok {{run}}`.

Call 6, a program run:
Sent: {"argv":["{{bin}}","-offbeat","p.json"]}
Exit status: 0
Output:
offbeat kick 2
offbeat snare 3
````
</details>
@ octocat fixes # 3

Call 7, a program run at the base:
Sent: {"argv":["{{bin}}","-offbeat","p.json"]}
Exit status: 2
Its output is not shown: maul's copy of it is not whole.
`````

_maul edits this comment as the attempts of this verification finish._
