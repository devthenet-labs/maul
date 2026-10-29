<!-- maul:notice maul/target-1 command-4418 -->
`/maul ship` is not a command maul knows on an intent issue.

Commands go on the first line of a comment. Here you can use:

- `/maul approve`: approve the posted plan and start the build
- `/maul replan [note]`: plan again, taking the note and approvers' comments since the last plan into account
- `/maul verify [fix|stop|--reset]`: check the change against the plan's acceptance criteria now; `fix` also lets maul fix failures again, `stop` only stops maul fixing failures on its own, `--reset` first restarts the preview with fresh data
- `/maul accept <id>|all`: accept, at the current commits, a manual outcome or criterion, one maul could not confirm or found failing, or one that passed only on retry; `all` accepts every one of them, and a note may follow on the lines below
- `/maul preview`: show the preview link now, before maul has finished checking the change; nothing else changes
- `/maul cancel`: stop work on this intent and close it; open pull requests are left to you
