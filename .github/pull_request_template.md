<!--
Pull requests merge by fast-forward, so each commit lands on main as written: make every commit a Conventional
Commit, `type(scope): description` (feat, fix, docs, test, refactor, chore), and give the title the same form.
Open the pull request as a draft until it is complete and `make pr` passes. See CONTRIBUTING.md.
-->

## Why

<!-- The problem this solves, and links to the issue it addresses. -->

## What changes

-

## How it was tested

<!-- The tests written first, and how you ran the change end to end. Paste the output of anything that failed. -->

- [ ] Tests written before the code; a bug fix has a regression test that failed before the fix
- [ ] `make pr` passes
- [ ] `make e2e` passes, or this change does not touch a controller
- [ ] Documentation updated under `docs/` (and `zensical.toml` for a new page), or nothing a user sees changed
- [ ] Every commit is a Conventional Commit

## Security

<!-- Does this touch authentication, credentials, input handling, data access, the agent sandbox, the egress broker or
preview isolation? If so, say what and why. Otherwise write "None". -->
