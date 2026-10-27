<!-- maul:notice maul/target-1 command-4418 -->
`/maul ship` is not a command maul knows on an intent issue.

Commands go on the first line of a comment. Here you can use:

- `/maul approve`: approve the posted plan and start the build
- `/maul replan [note]`: plan again, taking the note and approvers' comments since the last plan into account
- `/maul verify [fix|--reset]`: check the change against the plan's acceptance criteria now; `fix` also lets maul fix failures again, `--reset` first restarts the preview with fresh data
- `/maul accept <id>`: accept a manual criterion, or one that passed only on retry, at the current commits
- `/maul cancel`: stop work on this intent and close it; open pull requests are left to you
