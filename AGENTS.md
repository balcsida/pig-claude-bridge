# Agent rules

- `~/.claude/projects/**` is not evidence of Claude Code behaviour. Never cite it to justify a protocol or session-format claim.
- Prove undocumented Claude Code behaviour with a live probe before relying on it.
- Pin every proven behaviour with a contract test, so a change in Claude Code fails a test instead of users.
- Keep `CHANGELOG.md` with an `Unreleased` section at the top; entries use Conventional Commits style (`feat(scope): ...`, `fix(scope): ...`).
