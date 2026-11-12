<!-- maul:notice maul/target-1 comment-4421 -->
`/maul replan` in this comment was not read as a command, and nothing was done: maul reads a command only on a comment's first non-blank line, and this comment starts with other text. To use it, post a new comment with the command on its first line; anything else can follow on the lines below. Editing this comment does not help, since maul never acts on an edited comment.

Commands go on the first line of a comment. Here you can use:

- `/maul approve`: approve the posted plan and start the build
- `/maul replan [note]`: plan again, taking the note and approvers' comments since the last plan into account
- `/maul start`: start this intent now, without waiting for the issues it is blocked by
- `/maul verify [fix|stop|--reset|explore]`: check the change against the plan's acceptance criteria now; `fix` also lets maul fix failures again, `stop` only stops maul fixing failures on its own, `--reset` first restarts the preview with fresh data, `explore` has the evaluator look again even where maul's saved checks alone could settle it
- `/maul accept <id>|all|anomalies`: accept, at the current commits, a manual outcome or criterion, one maul could not confirm or found failing, or one that passed only on retry; `all` accepts every one of them and any anomaly maul noticed at those commits while it looked around, `anomalies` only those anomalies, and a note may follow on the lines below
- `/maul preview`: show the preview link now, before maul has finished checking the change; nothing else changes
- `/maul cancel`: stop work on this intent and close it; open pull requests are left to you
