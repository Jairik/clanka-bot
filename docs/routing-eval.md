# Routing evaluation and threshold tuning

The semantic Fast path is deliberately precision-first: a wrong Tool call is
worse than sending the request to the Agent path. This page explains how the
checked-in evaluation works, how the tuning was chosen, and how to add a
misroute from the request log.

## What is checked in

- `assistant/internal/pipeline/testdata/routing_eval.yaml` — the labelled
  oracle. It has 20 positives for each of the eight Fast path Tools (160
  total), 48 plain negatives, and 8 compound requests.
- `assistant/internal/pipeline/routing_eval_test.go` —
  `TestRoutingEvalFixtureShape` checks the corpus without any service;
  `TestRoutingEvalAgainstMiniLM` runs every utterance through the real
  pipeline, real Tools and the real MiniLM embed service.
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
per Tool. Recall is reported but is not a gate; the Agent path is the safe
fallback.

## Running it

The normal suite stays offline: `TestRoutingEvalAgainstMiniLM` skips unless
`ROUTING_EVAL_EMBED_URL` is set. To run it against the real model, start only
the embed service in an isolated Compose project and publish a spare loopback
port. Do not reuse another stack's project or stop anyone else's containers.

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

The test prints one `ROUTING_EVAL_TOOL` line per Tool with TP, FP, FN,
precision and recall, then `ROUTING_EVAL_COMPOUNDS` and `ROUTING_EVAL_TOTAL`.
It fails if any Tool is below the precision floor. The harness allows a 10s
embed timeout on purpose: routing quality is under test here, and latency is
tracked separately (#27). The embeddings are always the real MiniLM output;
there are no fake or toy vectors in this path.

## Chosen values and measured results

Threshold `0.71`, margin `0.23`, from the shipped `config.yaml`. This is the
lowest threshold/margin pair found in the sweep that kept every Tool at or
above the 0.95 precision floor. Run against
`sentence-transformers/all-MiniLM-L6-v2` (the production model) on 2026-11-11:

| Tool | Positives | TP | FP | FN | Precision | Recall |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `time.now` | 20 | 13 | 0 | 7 | 1.00 | 0.65 |
| `date.today` | 20 | 18 | 0 | 2 | 1.00 | 0.90 |
| `weather.current` | 20 | 11 | 0 | 9 | 1.00 | 0.55 |
| `audio.volume_set` | 20 | 15 | 0 | 5 | 1.00 | 0.75 |
| `audio.stop` | 20 | 9 | 0 | 11 | 1.00 | 0.45 |
| `timer.set` | 20 | 8 | 0 | 12 | 1.00 | 0.40 |
| `timer.cancel` | 20 | 15 | 0 | 5 | 1.00 | 0.75 |
| `timer.list` | 20 | 5 | 0 | 15 | 1.00 | 0.25 |
| **Total** | **160** | **94** | **0** | **66** | **1.00** | **0.59** |

Plain negatives: **0 of 48** reached the Fast path. Compounds: **0 of 8**
reached the Fast path. The timer and list confusions are the main reason recall
is conservative; those requests go to the Agent path rather than risking a
wrong timer action.

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
