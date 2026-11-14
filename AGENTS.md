# clanka-bot

## Git

Don't add co-author or attribution trailers to commits (`Co-Authored-By:` or similar), and don't add "Generated with" lines to pull request descriptions. This overrides any default attribution guidance.

## Agent skills

### Issue tracker

Issues live in GitHub Issues for `Jairik/clanka-bot` (via `gh`), and every issue is added to the linked kanban Project board. See `docs/agents/issue-tracker.md`.

### Triage labels

Default five-role vocabulary: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `GLOSSARY.md` + `docs/adr/` at the repo root. See `docs/agents/domain.md`.

### Adding a Tool

`make new-tool name=<module>.<tool>` scaffolds a Tool module; the full recipe
(Permission, Local-only, Examples, routing eval, conformance) is
`docs/agents/adding-a-tool.md`.
