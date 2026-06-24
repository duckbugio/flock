# Contributing to Flock

Thanks for your interest in contributing to Flock — the open-source bot at the
heart of [DuckBug](https://github.com/duckbugio). This guide covers how to set up,
make a change, and open a pull request.

By participating, you agree to abide by our [Code of Conduct](CODE_OF_CONDUCT.md).

## Ways to contribute

- **Report a bug** — open an issue with the *Bug report* template.
- **Request a feature** — open an issue with the *Feature request* template.
- **Send a fix or feature** — fork, branch, and open a pull request (below).
- **Improve the docs** — README, the translated READMEs, or files under `docs/`.

For anything non-trivial, please open an issue first so we can agree on the
approach before you invest time in a PR.

> **Security issues are different:** do **not** open a public issue or PR for a
> vulnerability. Follow [SECURITY.md](SECURITY.md) instead.

## Development setup

Flock is a Go monorepo. The only hard requirement is **Docker** — the lint and
test toolchain is pinned inside a `dev-tools` image so your results match CI. You
do not need a local Go install.

```bash
# Install the task runner (https://taskfile.dev), then:
task dev-tools:build   # build the pinned lint/test image (first run only)
task lint              # gofmt + go vet + golangci-lint
task tests             # go test -race ./...
task build             # compile the binaries
```

These are the exact entrypoints [CI](.github/workflows/ci.yml) runs, so a green
local run is a green CI run. See the README's *Repo layout* section for where
each service lives.

## Pull request process

1. **Fork** the repository and clone your fork.
2. **Create a branch** off `main` for your change
   (`git checkout -b fix/clear-error-message`).
3. **Make your change.** Keep it focused — one logical change per PR.
4. **Add tests.** New behaviour needs coverage; a bug fix should add a test that
   fails before the fix and passes after.
5. **Run the checks** — `task lint && task tests && task build` must all pass.
6. **Open a pull request** against `main`, fill in the template, and link any
   related issue. Describe *what* changed and *why*, and how you verified it.

A maintainer will review; please respond to review comments by pushing follow-up
commits to the same branch. PRs are squash-merged once approved and green.

## Conventions

- **Commit messages** follow [Conventional Commits](https://www.conventionalcommits.org/)
  in the imperative mood, scoped by area:
  `feat(schedule): ...`, `fix(telegram): ...`, `docs: ...`, `chore(deps): ...`.
- **Go style** is enforced by `golangci-lint` (`.golangci.yml`) and `gofumpt`.
  Run `task lint` before pushing; the linter is strict by design.
- **Errors** are wrapped with context: `fmt.Errorf("doing x: %w", err)` — lower
  case, no trailing punctuation (the `error-strings` rule).
- **Keep PRs free of unrelated churn** (formatting-only diffs, dependency bumps
  mixed with features, etc.).

## License

By contributing, you agree that your contributions are licensed under the
project's [MIT License](LICENSE).
