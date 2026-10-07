## You reached your turn limit

Your run stopped at its limit of 144 agent turns before it finished.
patchy gives you at most 6 more turns to wrap up, and nothing more after them.

The working tree is kept exactly as it is now: patchy refuses a wrap-up that changes any file in it, and commits only
what it already holds.

- Do not edit code or tests, do not run tests, builds or other commands, and do not commit.
- Check the report you wrote at `/workspace/reports/build.md` against the working tree, and rewrite it
  so it describes what the tree holds now, in the format your instructions gave.
- If the plan is not fully built, or its tests did not run and pass on the tree as it stands, set `success: false` and
  say in `reason` what is unfinished. Never claim more than the tree holds.
- Only if `success` is true: make sure `/workspace/commit.sh` exists and commits the change exactly as the tree
  holds it, and nothing else.
- When you are done, reply with one short line saying so.
