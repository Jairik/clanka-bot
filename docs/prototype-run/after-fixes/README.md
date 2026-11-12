# Final fix pass evidence (synthetic)

These files are from the final fix pass rerun on 2026-11-11, not the original
#29 run above. They are still synthetic: Pocket TTS audio looped through a null
sink, no human speaker, and a scripted fake provider instead of OpenRouter.
The mic probe was skipped. See `docs/prototype-run.md` § "Final fix pass" for
the commands, 19/20 result, and remaining gaps.

- `scenario-results.json` / `.md` — per-scenario verdicts
- `requests.jsonl` — the assistant's request lines
- `assistant-evidence.log` — voice, Agent and Timer lines
- `fake-requests.jsonl` — the test double's view
- `latency-report.txt` — per-stage summary
