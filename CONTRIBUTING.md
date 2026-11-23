# Contributing to maul

Thank you for helping. This page is how a change gets from your machine into a release. To report a security problem, do
not open an issue: follow [SECURITY.md](SECURITY.md).

Before you start, read [AGENTS.md](AGENTS.md), the map of the repository (where each binary and package lives), and
[DESIGN.md](DESIGN.md), the requirements and the state machine. The user documentation is the site under
[`docs/`](docs/), published at [devthenet-labs.github.io/maul](https://devthenet-labs.github.io/maul/).

## Set up

The toolchain comes from the `.mise/` submodule: [mise](https://mise.jdx.dev) installs the pinned tools, and the
`Makefile` only forwards to mise tasks (`make <task>` is `mise run <task>`).

Install mise (https://mise.jdx.dev), then:

```sh
git clone https://github.com/devthenet-labs/maul.git
cd maul
git submodule update --init
mise install
make build   # every binary into bin/
```

## The commands you will use

| Command              | What it does                                                                                |
| -------------------- | ------------------------------------------------------------------------------------------- |
| `make test`          | Unit tests, with the race detector and coverage                                             |
| `make lint`          | Static analysis: golangci-lint, govulncheck, license headers, shell, containers and prose   |
| `make fmt`           | Format Go and Markdown, and add missing license headers                                     |
| `make e2e`           | The end-to-end suite: envtest, the real binaries and an in-memory GitHub, no cluster needed |
| `mise run envtest`   | The tests that need a real kube-apiserver (they skip without `KUBEBUILDER_ASSETS`)          |
| `mise run codegen`   | Regenerate deepcopy and the CRD manifests after changing `api/v1alpha1`; CI fails on drift  |
| `mise run helm-lint` | Lint and render the Helm charts                                                             |
| `mise run docs`      | Regenerate the CLI reference (`docs/cli/`) and the shell completions from the command tree  |
| `mise run serve`     | Preview the docs site locally; `mise run docs-build` builds it as CI does                   |
| `make pr`            | The full local gate before a pull request (below)                                           |

`make pr` runs tidy, format, the codegen drift check, lint, the chart, terraform and scaffold gates, the unit and
envtest tests, the build, the CLI reference, the docs build and a binary size report, and finally runs `./commit.sh` if
one exists. It does not run `make e2e`; run that too when you change a controller. Run both before you ask for review,
and if a step cannot run on your machine, say which one in the pull request.

## Tests first

- Write the test before the code. New behaviour is not done until it has tests, and a bug fix comes with a regression
  test that fails before the fix and passes after it.
- Test behaviour through public interfaces, not implementation details. Do not mock the thing under test.
- Use table-driven tests from the standard library, not testify. Prefer fakes over mocks, and the controller-runtime
  fake client for reconciler tests.
- Never weaken, skip or delete a test to make a suite pass. If a test fails and you cannot fix it, say so in the pull
  request with its output.

## Code conventions

These are the ones in [AGENTS.md](AGENTS.md#conventions), plus the rules the linters enforce:

- Go, at the version in `go.mod`. Commands use cobra and viper (environment variables take the `MAUL_` prefix).
- Log with `log/slog` to stderr. Stdout is reserved: agent-runner's event stream lives there.
- Every package has a `doc.go`, and every source file starts with the MIT SPDX header (`make fmt` adds it).
- `internal/` holds all private code, one package per concern. `pkg/` holds only the public plugin seams, and their
  exported signatures must not reference `internal/` types.
- Keep the harness and runner separation: the harness builds the command line, the runner executes it, the harness
  parses its output.
- Generated files are never edited by hand: regenerate them (`mise run codegen`, `mise run docs`).
- Markdown prose wraps at 120 columns; `make fmt` formats it with the pinned prettier.
- Keep a change to one concern. An unrelated improvement you notice belongs in its own pull request, or an issue.

## Commits

Use [Conventional Commits](https://www.conventionalcommits.org/): `type(scope): description`, in the imperative, with
the body explaining why. Common types are `feat`, `fix`, `docs`, `test`, `refactor` and `chore`. For example:

```text
type(scope): what the change does, in the imperative

Why the change is needed, and anything a reviewer should know.
```

[release-please](https://github.com/googleapis/release-please) builds the changelog and the version from these messages,
and pull requests are merged by fast-forward, so every commit lands on `main` as you wrote it. Make each commit small
and coherent, and make each message one a reader of the changelog can follow.

## Pull requests

1. Work on a branch, never on `main`.
2. Open the pull request as a **draft** and fill in the template. It stays a draft until `make pr` passes and the
   description is complete.
3. Mark it ready for review.
4. A maintainer merges it, by fast-forward: a `/merge` comment merges an approved pull request whose checks are all
   green, and `/auto-merge` (or the `auto-merge` label) merges it once it is. Do not use the GitHub merge button.

Call out anything security-sensitive in the description (authentication, credentials, input handling, data access, the
agent sandbox, the egress broker or the preview isolation), so it gets a careful review.

## Documentation

A change people will notice needs its documentation in the same pull request: the page under `docs/` that describes the
feature, and the navigation in `zensical.toml` for a new page. `docs/cli/` is generated: change a command's `Short`,
`Long` or `Example` and run `mise run docs` instead of editing the Markdown.

## License

maul is MIT licensed: see [LICENSE](LICENSE).
