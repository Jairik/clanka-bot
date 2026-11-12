# Routing evaluation and threshold tuning

The semantic Fast path is deliberately precision-first: a wrong Tool call is
worse than sending the request to the Agent path. This page explains how the
checked-in evaluation works, how the tuning was chosen, and how to add a
misroute from the request log.

## What is checked in

- `assistant/internal/pipeline/testdata/routing_eval.yaml` — the labelled
  oracle. It has at least 20 positives for each Fast path Tool (exact counts in
  the results table), 177 plain
  negatives, and 31 compound requests (exact counts in the results table).
- `assistant/internal/pipeline/testdata/routing_baseline.yaml` — the recorded
  Map 1 totals the real-MiniLM evaluation must not fall below.
- `assistant/internal/pipeline/routing_eval_test.go` —
  `TestRoutingEvalFixtureShape` checks the corpus without any service;
  `TestRoutingEvalBaselineShape` checks the baseline file;
  `TestRoutingEvalAgainstMiniLM` runs every utterance through the real
  pipeline, real Tools and the real MiniLM embed service.
- `assistant/tools/conformance` — the shared module contract test every Tool
  module calls once; see "Conformance suite" below.
- `assistant/config.yaml` — `semantic.threshold` and `semantic.margin` are set
  from the evaluation.

The fixture is an oracle, not training data. Labels describe what the user
meant, never what the model happened to predict. Positives must survive real
extraction; a match that then fails extraction counts as a miss. Negatives
include Agent requests, questions that sound like a Tool but are not one, and
compounds.

## The agreed floor

The user confirmed a **minimum precision of 0.95 for each Tool** for #26.
Overall precision does not excuse a weak Tool, so the test enforces the floor
per Tool. Per-Tool recall is reported but is not a floor; the Agent path is the
safe fallback. The real-MiniLM evaluation also gates the overall Map 1 totals
in `routing_baseline.yaml`: overall precision and recall may not drop, and no
negative or compound may start reaching the Fast path. A new Tool ticket may
raise those totals but never lower them.

A Tool with no Fast path predictions has no measurable precision, so the test
fails it rather than letting an empty denominator score 1.00. It also reports
coverage split by path (`control_tp` from the exact Control phrase table,
`semantic_tp` from the MiniLM router) and fails a Tool whose only TP are
Control-phrase matches: those numbers would say nothing about the semantic
tuning. The reported per-Tool precision is therefore always computed from
predictions the router actually made.

## Conformance suite

Every Tool module has one conformance test line:

```go
// One line wires the shared contract check; the real-model part skips offline.
func TestModuleConformance(t *testing.T) { conformance.Run(t, NewModule()) }
```

`Run` registers the production module list with the module under test and
checks that it registers cleanly, that config defaults validate, that every
Tool declares a valid Permission and Local-only flag, and that each Speak
renderer returns non-empty text for a representative output. A module with
Speak renderers implements the small `conformance.Sampler` interface
(`ConformanceSamples() map[string]any`) with one successful output per
canonical Tool name. Eligibility is explicit: a Tool must either complete the
Fast path contract (Speak, Examples and Extract when it takes arguments) or set
`AgentOnly: true` with no Examples/Extract. The suite fails a Tool that is
neither, so a missing renderer cannot silently drop a Tool off the voice path.

For Fast-path-eligible Tools only, it then embeds every Example with the real
MiniLM service and fails if an Example does not route to its own Tool.
Agent-only Tools have no Examples to embed; their coverage is the explicit
Agent integration test plus the negative utterances in the eval. The routing
part skips when `ROUTING_EVAL_EMBED_URL` is unset, so the normal offline suite
still passes; CI sets it. It never invents vectors for an Agent-only Tool.

## Running it

The normal suite stays offline: `TestRoutingEvalAgainstMiniLM` skips unless
`ROUTING_EVAL_EMBED_URL` is set. The explicit integration target is
`make routing-eval ROUTING_EVAL_EMBED_URL=http://127.0.0.1:18085`; it fails
when the variable is missing, so the 0.95 per-Tool floor and the Map 1
baseline are not silently unguarded. To run it against the real model, start
only the embed service in an isolated Compose project and publish a spare
loopback port. Do not reuse another stack's project or stop anyone else's
containers.

```sh
# Build the same pinned MiniLM image the stack uses, if it is not built yet.
docker compose build embed

# /tmp/clanka-routing-eval-compose.yml
cat >/tmp/clanka-routing-eval-compose.yml <<'YAML'
services:
  embed:
    ports:
      - "127.0.0.1:18085:8085"
YAML

docker compose -p clanka-routing-eval \
  -f docker-compose.yml \
  -f /tmp/clanka-routing-eval-compose.yml up -d embed

# Wait for {"status":"ok"} at http://127.0.0.1:18085/healthz, then:
cd assistant
ROUTING_EVAL_EMBED_URL=http://127.0.0.1:18085 \
  go test ./internal/pipeline -run TestRoutingEvalAgainstMiniLM -count=1 -v

# Tear down only this evaluation project.
cd ..
docker compose -p clanka-routing-eval \
  -f docker-compose.yml \
  -f /tmp/clanka-routing-eval-compose.yml down
```

The test prints one `ROUTING_EVAL_TOOL` line per Tool with TP, Control-phrase
TP, semantic TP, FP, FN, precision and recall, then
`ROUTING_EVAL_COMPOUNDS`, `ROUTING_EVAL_NEGATIVES`, `ROUTING_EVAL_FOCUS`,
`ROUTING_EVAL_FOCUS_TOTAL`, `ROUTING_EVAL_TOTAL` and
`ROUTING_EVAL_BASELINE`. It fails if any Tool is below the precision floor,
made no predictions, was only ever right through a Control phrase, or if the
overall result is below the Map 1 baseline. The harness allows a 10s embed
timeout on purpose: routing quality is under test here, and latency is tracked
separately (#27). The embeddings are always the real MiniLM output; there are
no fake or toy vectors in this path.

CI (`.github/workflows/ci.yml`) runs the offline Go suite on every push and
pull request. A second job builds the pinned embed image, waits for its real
`/healthz`, then runs the full suite with `ROUTING_EVAL_EMBED_URL` set so the
conformance Example checks and the baseline gate both run against MiniLM.

## Chosen values and measured results

Threshold `0.71`, margin `0.23`, from the shipped `config.yaml`. This is the
lowest threshold/margin pair found in the sweep that kept every Tool at or
above the 0.95 precision floor. Run against
`sentence-transformers/all-MiniLM-L6-v2` (the production model) on 2026-11-12:

| Tool | Positives | TP | Control TP | Semantic TP | FP | FN | Precision | Recall |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `time.now` | 20 | 12 | 5 | 7 | 0 | 8 | 1.00 | 0.60 |
| `date.today` | 20 | 17 | 0 | 17 | 0 | 3 | 1.00 | 0.85 |
| `time.in_city` | 21 | 9 | 0 | 9 | 0 | 12 | 1.00 | 0.4286 |
| `date.countdown` | 22 | 22 | 0 | 22 | 0 | 0 | 1.00 | 1.00 |
| `weather.current` | 20 | 11 | 4 | 7 | 0 | 9 | 1.00 | 0.55 |
| `audio.volume_set` | 20 | 9 | 1 | 8 | 0 | 11 | 1.00 | 0.45 |
| `audio.stop` | 20 | 8 | 0 | 8 | 0 | 12 | 1.00 | 0.40 |
| `timer.set` | 26 | 11 | 0 | 11 | 0 | 15 | 1.00 | 0.4231 |
| `timer.cancel` | 20 | 14 | 0 | 14 | 0 | 6 | 1.00 | 0.70 |
| `timer.remaining` | 20 | 9 | 0 | 9 | 0 | 11 | 1.00 | 0.45 |
| `timer.list` | 20 | 12 | 0 | 12 | 0 | 8 | 1.00 | 0.60 |
| `alarm.set` | 30 | 13 | 0 | 13 | 0 | 17 | 1.00 | 0.4333 |
| `alarm.list` | 21 | 13 | 0 | 13 | 0 | 8 | 1.00 | 0.6190 |
| `alarm.cancel` | 20 | 8 | 0 | 8 | 0 | 12 | 1.00 | 0.4000 |
| `alarm.update` | 23 | 23 | 0 | 23 | 0 | 0 | 1.00 | 1.00 |
| `voice.set` | 20 | 20 | 0 | 20 | 0 | 0 | 1.00 | 1.00 |
| `voice.list` | 20 | 6 | 4 | 2 | 0 | 14 | 1.00 | 0.30 |
| `voice.current` | 20 | 7 | 3 | 4 | 0 | 13 | 1.00 | 0.35 |
| `voice.rate` | 20 | 16 | 2 | 14 | 0 | 4 | 1.00 | 0.80 |
| `persona.switch` | 20 | 20 | 0 | 20 | 0 | 0 | 1.00 | 1.00 |
| `persona.list` | 20 | 17 | 3 | 14 | 0 | 3 | 1.00 | 0.85 |
| `persona.current` | 20 | 9 | 4 | 5 | 0 | 11 | 1.00 | 0.45 |
| `quiet.set` | 20 | 20 | 0 | 20 | 0 | 0 | 1.00 | 1.00 |
| `quiet.off` | 20 | 16 | 2 | 14 | 0 | 4 | 1.00 | 0.80 |
| `quiet.dnd` | 20 | 20 | 0 | 20 | 0 | 0 | 1.00 | 1.00 |
| `quiet.awake` | 20 | 20 | 2 | 18 | 0 | 0 | 1.00 | 1.00 |
| `notify.phone` | 22 | 22 | 0 | 22 | 0 | 0 | 1.00 | 1.00 |
| `reminder.set` | 24 | 13 | 0 | 13 | 0 | 11 | 1.00 | 0.5417 |
| `reminder.list` | 22 | 12 | 0 | 12 | 0 | 10 | 1.00 | 0.5455 |
| `reminder.cancel` | 22 | 12 | 0 | 12 | 0 | 10 | 1.00 | 0.5455 |
| `calc.evaluate` | 22 | 17 | 0 | 17 | 0 | 5 | 1.00 | 0.7727 |
| `units.convert` | 24 | 22 | 0 | 22 | 0 | 2 | 1.00 | 0.9167 |
| `random.flip` | 21 | 21 | 0 | 21 | 0 | 0 | 1.00 | 1.00 |
| `random.roll` | 22 | 22 | 0 | 22 | 0 | 0 | 1.00 | 1.00 |
| `random.pick` | 22 | 17 | 0 | 17 | 0 | 5 | 1.00 | 0.7727 |
| `assistant.repeat` | 20 | 14 | 0 | 14 | 0 | 6 | 1.00 | 0.70 |
| `assistant.help` | 20 | 15 | 0 | 15 | 0 | 5 | 1.00 | 0.75 |
| `system.status` | 20 | 10 | 0 | 10 | 0 | 10 | 1.00 | 0.50 |
| **Total** | **804** | **559** | **30** | **529** | **0** | **245** | **1.00** | **0.6953** |

These numbers are from the merged tree after #66, #67 and #68: the #57
per-Alarm overrides, #59 quick-wins, #60 utility Tools, #62 Notifications, #65
Persona, #53 Alarm, #54 Reminder and the Agent-only web/GitHub Tools. Precision
is 1.00 with no negative or compound misroute, and every Tool clears the 0.95
floor. `alarm.update` adds 23 positives at 1.00 recall; `alarm.set` carries the
per-Alarm override phrasings (30 positives) and keeps precision 1.00, with its
ambiguous variants falling to the Agent path. The #59 Tools add 60 positives
and three negative/compound groups, and the Persona Tools add 60 positives and
11 plain negatives. `assistant.repeat` and `assistant.help` are Local-only, so
their recall only exercises the semantic router and never the Agent path.
`persona.switch`
resolves all 20 positives, so "switch to the GLaDOS persona" reaches the Fast
path; the list/current questions are lexically close to each other, so their
short forms are exact Control phrases and the paraphrases fall to the Agent
path rather than risk the wrong read Tool. `notify.phone`'s positives include
demonstrative "send that to my phone" wording and explicit-message forms; its
phone-shaped negatives and compound keep it from swallowing device questions,
other transports and two-action requests. Reminder recall is conservative
because the set/list/cancel Examples are kept lexically distinct so no pair
sits inside the 0.23 margin: "list my reminders" and "delete my reminder" are
~0.22 apart from the other Reminder Tool, which is just under the shipped 0.23
margin. Those variants fall to the Agent path rather than risk a wrong
Reminder action; the Agent can call
`reminder.set`/`reminder.cancel`, while `reminder.list` is Local-only, so a list
variant that lacks a strong phrasing is better served by the Scheduled alerts
page (#58) than guessed. The voice, quiet and city-time recall notes below apply. `time.in_city` recall
is low because the city token dominates the embedding: known phrasings route,
and an unlisted city reaches the same Tool through the Agent path, which then
uses the local city table rather than a guess.

Plain negatives: **0 of 177** reached the Fast path, including the utility
non-requests such as an unsupported event, a currency conversion, a bare
"roll out the red carpet" and an open-ended pick, plus the reminder-, phone-,
repeat-, web-lookup-, page-read- and GitHub-shaped non-requests. Compounds: **0 of 31** reached the Fast path,
including mixed utility requests such as calculating a percentage and
converting miles. Every Tool is exercised by the semantic
router at least twice, so no Tool clears the floor only because the Control
phrase table happened to cover its positives. The twelve Agent-only web
negatives cover open-ended lookups and page reads, and the six GitHub negatives
cover issue writes, review-requested pull requests and notification reads.
`web.search`, `web.fetch` and every `github.*` Tool are Agent-only, so their
coverage is negative direct routing plus the Agent integration tests
(`internal/agent` for web search/fetch and
`internal/pipeline/github_confirmation_test.go` for the GitHub confirmation
journey), never a Fast path positive. The timer and list confusions are
the main reason recall is conservative; those requests go to the Agent path
rather than risking a wrong timer action.

### Focus cases

The `focus:` section in `routing_eval.yaml` is a separate oracle for the shared
Control phrases (#44). Each case names the holders present (`ring`, `reply`,
`music`), the phrase's intent, and the holder that must take it, or an empty
`handled_by` when no present holder declares the intent and the utterance must
fall through. The same word appears under different holders — `stop` with a
ringing alert, with a reply, with music, and with nothing — so a regression that
always picks one holder or always falls through fails. The offline fixture test
checks the priority walk and the shipped phrase table; the real-MiniLM run also
checks that a fall-through still reaches the semantic Fast path. Holder
capabilities in the fixture mirror the production declarations: a ringing alert
handles `stop`/`cancel`, the current reply handles `stop`, and music handles the
transport controls.

For comparison, the old placeholders (`0.70` / `0.08`) scored overall precision
0.985 and recall 0.79, but missed the floor where it matters: `time.now` at
0.941 and `timer.set` at 0.944. Two compounds leaked to the Fast path. That is
why the floor is enforced per Tool.

## Tuning a change

Threshold and margin default to `config.yaml`; two environment variables
override them for a sweep without editing the file:

```sh
cd assistant
for threshold in 0.65 0.68 0.70 0.71 0.72; do
  for margin in 0.10 0.15 0.20 0.23 0.25; do
    ROUTING_EVAL_EMBED_URL=http://127.0.0.1:18085 \
    ROUTING_EVAL_THRESHOLD=$threshold \
    ROUTING_EVAL_MARGIN=$margin \
      go test ./internal/pipeline -run TestRoutingEvalAgainstMiniLM -count=1 -v || true
  done
done
```

Pick the pair with the most recall that still clears the per-Tool floor, then
put it in `config.yaml` and record the new numbers in the PR. Do not lower the
floor or delete a hard case to make a pair look better. If no acceptable pair
exists, the fix is better Tool examples or routing logic, not a weaker corpus.

## Promoting a request-log misroute into the fixture

Every handled request writes one JSON line with `transcript`, `path`, `tool`,
`top_tools`, `margin`, `extract`, `fallthrough_reason` and `request_id`.

1. Find a real miss in the request log. Two shapes matter: a Fast path Tool
   that answered the wrong intent (false positive), or an Agent/fallthrough
   request that clearly belonged to one Fast path Tool (miss).
2. Decide the label from what the user meant, **not** from `top_tools` or what
   the model answered. This keeps the corpus an independent oracle.
3. Add the utterance to `positives.<tool>` if it belongs to a Fast path Tool,
   or to `negatives` if it must go to the Agent path. If it asks for more than
   one thing, add it to `compounds` instead.
4. Remove personal data first. Names, addresses, phone numbers, account
   details and anything else identifying should be replaced with a synthetic
   equivalent that keeps the same shape. Never paste raw user transcripts into
   the checked-in fixture.
5. Run `TestRoutingEvalFixtureShape` (offline) and then the MiniLM evaluation.
   If the new case drops a Tool below the floor, do not delete it. Either retune
   threshold/margin from a sweep, or file a follow-up for the real gap in Tool
   examples or extraction.
6. Commit the fixture and any tuning change together, and include the new
   per-Tool numbers in the PR.
