# Contributing to SkillLoop

Thank you for helping improve SkillLoop. Contributions of code, tests, documentation, and reproducible bug reports are welcome.

## Development environment

SkillLoop uses Nix to keep the Go toolchain and development tools reproducible on Linux and macOS.

```sh
git clone https://github.com/flemzord/skillloop.git
cd skillloop
nix develop
just pre-commit
```

With direnv installed, `direnv allow` loads the same development shell automatically.

Run `just --list` to see the available tasks. The most useful commands are:

```sh
just tests          # Unit tests with the race detector and coverage
just lint           # Go linting and formatting checks
just audit          # Known-vulnerability scan
just build          # Build the local skillloop binary
just release-local  # Build the complete release matrix without publishing
just pre-commit     # Format, generate, lint, test, build, and check the release config
```

## Making a change

1. Open an issue first for substantial features, behavioral changes, or changes to the security and permission model.
2. Keep the patch focused and add tests for new behavior and regressions.
3. Preserve the local-first, fail-open, reversible design. Hooks must never prevent Codex or Claude Code from running.
4. Do not include transcripts, credentials, tokens, personal data, or raw sensitive logs in fixtures or bug reports.
5. Run `just pre-commit` before opening a pull request and commit any generated changes.

Use [Conventional Commits](https://www.conventionalcommits.org/) for commit messages and pull request titles, for example:

```text
feat(capture): ingest Claude Code hook events
fix(rollback): restore the previously promoted skill revision
docs: explain transcript retention
```

Breaking changes must include `!` after the type or scope and explain the migration in the pull request.

## Pull requests

A pull request should explain:

- the user-visible behavior and motivation;
- the security, privacy, compatibility, and rollback impact;
- the exact validation that was run;
- any known limitations or follow-up work.

Keep unrelated refactors out of the same pull request. Reviewers may ask for additional tests when a change affects hooks, transcript parsing, Git worktrees, promotion, or rollback.

## Reporting security issues

Do not open a public issue for a suspected vulnerability. Follow [SECURITY.md](SECURITY.md) instead.

## License

By contributing, you agree that your contributions are licensed under the repository's [Apache License 2.0](LICENSE).
