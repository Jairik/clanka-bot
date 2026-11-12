# Adding a Tool

The recipe for every Tool in this repo. Use it for a new Tool module yourself,
and hand it to Hermes or OpenCode when the assistant is asked to extend itself.
Terms follow [GLOSSARY.md](../../GLOSSARY.md); architecture decisions are in
[docs/adr](../adr/).

## 1. Pick the canonical name and module

A **Tool** is one leaf operation with a canonical dotted snake_case name,
`<domain>.<operation>` (for example `timer.cancel`). A **Tool module** is the
package that owns related Tools, their `tools.<module>` config, and an optional
health check; several Tools can share one module.

If `assistant/tools/<module>` already exists, add the Tool to that module by
hand; the scaffold only creates a new module and rejects existing names. To
exercise the generator before hand-adapting its contract, run it in a
throwaway copy of the tree and copy the shape you need; never leave the
generated placeholder module or its `tools.go` line in production.

Completion: you have a canonical name and a module that does not exist yet.

## 2. Run the scaffold

From the repo root:

```sh
make new-tool name=<module>.<tool>
```

It creates `assistant/tools/<module>/` with the module value and typed config,
one placeholder Tool, `samples.go` for the conformance suite, and a
`conformance_test.go`. It also adds the import and `NewModule()` line to the one
module list in `assistant/tools/tools.go`. The files are formatted and already
pass the suite as generated.

```sh
cd assistant
go test ./tools/<module> -run TestModuleConformance -count=1 -v
go build ./...
```

Offline this checks the static contract; the Example routing subtest skips until
you set the real embed URL in step 6.

Completion: the command printed the three files and the module line, and the
conformance test passes with no hand edits.

## 3. Replace the placeholder Tool

The starter returns and speaks a configured `Reply` so every seam is visible.
Replace it with the real operation:

- `In` and `Out`: the typed contract. Keep `In` empty if the Tool takes no
  arguments; an empty `In` is what keeps it eligible for the Fast path. A Tool
  with arguments needs `Extract` to be Fast-path eligible.
- `Handler`: the side effect. Return a `tool.ToolError{...}` with a `Say` line
  only when the user needs to hear the failure.
- `Config` and `DefaultConfig`: the `tools.<module>` section. Decode over the
  defaults and reject invalid values in `validate`. Add a commented block to
  `assistant/config.yaml` when the setting should be discoverable; an omitted
  block uses the defaults.
- `Health(ctx)` is optional; add it when the module depends on an upstream.
- `Speak`: the one-line Fast path reply. Keep it short and stable.

Completion: the Tool does the real work, the placeholder config is replaced, and
`go test ./tools/<module>` still passes.

### If the Tool needs a secret

Declare the name on the module by implementing the optional
`tool.SecretDeclarer` interface:

```go
// RequiredSecrets lists .env names the write-only store can also hold.
func (*Module) RequiredSecrets() []string { return []string{"EXAMPLE_API_KEY"} }
```

Read it through the shared `tool.Deps.Secrets` resolver (`Get(name)`), which
returns the stored value first and the `.env` bootstrap second. Never put the
value in `config.yaml`, a setting, a log line or an error; the secrets API lists
the declared name with only `set`, `last4`, `updated_at` and `source`.

### If the Tool can hold Focus

A shared Control phrase (`stop`, `pause`, `resume`, `next`, `cancel`, `snooze`)
goes to whatever holds Focus, in this fixed order: a ringing Scheduled alert,
then the current reply, then music. A module that can be one of those holders
declares the intents it handles and claims Focus only while it is active:

```go
// Intents lists what this module handles while it holds Focus.
func (*Module) Intents() []focus.Intent { return []focus.Intent{focus.Stop} }

// Claim at the matching priority while the activity runs; release when it ends.
claim := deps.Focus.Claim(focus.Music, holder)
defer claim.Release()
```

The holder implements `focus.Holder`: `Intents()` returns the same set, and
`HandleIntent` acts and returns optional spoken text. Claiming with a token
means a late turn cannot clear a newer holder. Do not claim at turn start for a
request that may route a shared phrase: the phrase must still reach the holder
it is meant to stop. `tool.IntentDeclarer` is the static module declaration; the
conformance suite checks every intent is in the fixed vocabulary. A module that
never takes Focus omits both.

### If the Tool recovers persisted state

A module that starts a scheduler or recovers rows from SQLite implements the
optional `tool.Startable` interface:

```go
// Start runs after main has bound the live settings, Cues, Zone and Ring seams.
func (m *Module) Start(context.Context) error { return m.manager.AttachStore(...) }
```

`tools.Set.StartAll` calls it once, in module order, after `RegisterAll` and
after the live settings service exists. Do not recover in `Register`: an alert
that is already overdue would fire with the code defaults instead of the
effective settings. Read a live setting per action through
`tool.Deps.Settings` (a late-bound accessor) and never snapshot it.

## 4. Choose the Permission level

| Permission | Use when | Fast path |
| --- | --- | --- |
| `read` | No side effects: asking, looking up, listing. | Yes |
| `act` | Harmless or easily undone: setting a timer, changing volume. | Yes |
| `confirm` | Hard to undo or outward-facing: sending, buying, deleting, posting. | No; needs a spoken Confirmation first. |

The scaffold defaults to `read`. Choose the minimum that fits the real
operation, and never set `confirm` on something the Fast path should call
directly. A setting override can only tighten this level, never loosen it.

Completion: the level matches the real side effect, not the scaffold default.

## 5. Choose Local-only

`LocalOnlyYes` means the result must never leave the Pi: the Agent path cannot
see or call it, and neither can Hermes or the MCP surface. `LocalOnlyNo` leaves
it available to both paths. Registration fails until you choose one explicitly,
and no setting can override it. The scaffold default `LocalOnlyNo` is only safe
because the placeholder has no private data.

Completion: the declaration matches where the Tool's results may travel.

## 6. Write Examples, negative cases and eval cases

`Examples` are the utterances a user would actually say. Every Example must
route to this Tool alone on the semantic Fast path, or conformance fails. Write
three to eight varied, natural phrasings and replace the starter placeholders:

- Put the Tool's distinctive words in each one. Generic wording ties with a
  neighbouring Tool and lands below the margin.
- Do not copy another Tool's vocabulary, and do not use a Control phrase;
  Control phrases bypass semantic matching.
- Add the opposite side too: an utterance that sounds like this Tool but must
  not trigger it belongs in the eval's `negatives` list.

Then update the routing eval; **this is part of the definition of done for a
Tool**:

1. Add the canonical name to `routingEvalTools` in
   `assistant/internal/pipeline/routing_eval_test.go`.
2. Add at least 20 labelled positives under `positives:<tool>` in
   `assistant/internal/pipeline/testdata/routing_eval.yaml`.
3. Add the negative cases to `negatives:` (and compounds to `compounds:` when
   the request asks for more than one thing).

These three edits keep the fixture honest: the real eval asserts that the router
indexed exactly `routingEvalTools`, enforces 0.95 precision per Tool, and gates
the Map 1 totals in `testdata/routing_baseline.yaml`.

Run it against the real MiniLM service:

```sh
make routing-eval ROUTING_EVAL_EMBED_URL=http://127.0.0.1:18085
# or a known dev endpoint:
cd assistant
ROUTING_EVAL_EMBED_URL=http://192.168.192.4:8085 go test ./internal/pipeline \
  -run TestRoutingEvalAgainstMiniLM -count=1 -v
```

See [docs/routing-eval.md](../routing-eval.md) for starting the embed service.
Do not lower the baseline or delete a hard negative to make the numbers pass;
fix the Examples or send the utterance to the Agent path.

Completion: the fixture shape test passes, the real eval reports this Tool at
precision 0.95 or better, and no negative or compound reaches the Fast path.

## 7. Run the whole contract

```sh
cd assistant
go test ./tools/<module> -run TestModuleConformance -count=1 -v
ROUTING_EVAL_EMBED_URL=http://192.168.192.4:8085 \
  go test ./tools/<module> -run TestModuleConformance -count=1 -v
go vet ./...
go test ./...
cd ..
make check
```

The first command checks registration, config defaults, Permission, Local-only,
`Speak` and `ConformanceSamples`; the second also embeds every Example against
the real model and fails a misroute. `ConformanceSamples` must return one
representative successful `Out` per Speak Tool, keyed by canonical name; update
it whenever `Out` changes. The suite registers the production module list, so a
broken sibling module also fails this test; that is intentional.

## 8. Update the glossary if a new term appeared

If the Tool introduces a domain word, add it to `GLOSSARY.md` with a definition
and the words to avoid, following [domain.md](domain.md). If the new word is
just an implementation detail, leave the glossary alone rather than inventing a
synonym for an existing term.

## Definition of done

- [ ] The module contains the real Tool, not the placeholder; `In`/`Out`,
      `Handler`, `Speak` and config are the final contract.
- [ ] Permission and Local-only are deliberate, not scaffold defaults by
      accident.
- [ ] `Examples` route to this Tool, and the negative cases are written down.
- [ ] `routingEvalTools`, the 20+ positives and the negatives are updated, and
      the real-MiniLM routing eval passes at the 0.95 per-Tool floor without
      lowering the baseline.
- [ ] The module conformance test passes offline and with
      `ROUTING_EVAL_EMBED_URL` set.
- [ ] `go vet ./...`, `go test ./...` and `make check` pass.
- [ ] `GLOSSARY.md` is updated if a new term appeared.
- [ ] `assistant/tools/tools.go` has exactly the one generated module line; no
      other registration was touched.
