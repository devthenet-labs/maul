<!-- maul:notice maul/target-1 comment-4430 -->
`/maul verify` was not understood.

Commands go on the first line of a comment. Here you can use:

- `/maul verify [fix|stop|--reset]`: check the change against the plan's acceptance criteria now; `fix` also lets maul fix failures again, `stop` only stops maul fixing failures on its own, `--reset` first restarts the preview with fresh data
