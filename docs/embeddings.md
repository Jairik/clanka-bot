# Embedding model evaluation (2026-11-14)

Question: the semantic Fast path sometimes misses a natural paraphrase. Is a
different sentence model better than the shipped
`sentence-transformers/all-MiniLM-L6-v2`, and can the thresholds be adjusted?

Short answer: **MiniLM-L6 remains the best model for this precision-first
routing task.** Every candidate either lost recall at the same precision or
needed a lower margin that produced false positives. The useful adjustment is
not a model swap; it is enriching the weak Tools' `Examples` and, if the owner
accepts a few false positives, lowering the margin to 0.14.

## Method

- Real embed images were built with `EMBED_MODEL=<candidate>` (the model is
  baked at image build time) and run on separate loopback ports.
- The checked-in evaluation was run unchanged against each:
  `ROUTING_EVAL_EMBED_URL=http://127.0.0.1:<port> go test ./internal/pipeline -run TestRoutingEvalAgainstMiniLM -count=1 -v`.
- Threshold and margin were swept because cosine distributions are
  model-specific; comparing at MiniLM's 0.71/0.23 alone is unfair to a model
  with a different scale.
- Device: Arch Linux desktop, 12 logical cores, CPU-only embed image (the
  Compose image uses the CPU torch wheel). The Pi is slower.
- Corpus: 1,220 labelled positives, 225 negatives, 44 compounds; per-Tool
  precision floor 0.95, overall Map 1 baseline precision 1.0000 / recall
  0.5926 / zero negative and compound misroutes.

## Routing quality

| Model | Dim | Threshold / margin | TP | FP | Precision | Recall | Per-Tool floor failures |
| --- | ---: | --- | ---: | ---: | ---: | ---: | ---: |
| **MiniLM-L6 (shipped)** | 384 | 0.71 / 0.23 | 817 | 0 | **1.0000** | **0.6697** | 0 |
| MiniLM-L6 | 384 | 0.71 / 0.18 | 876 | 3 | 0.9966 | 0.7180 | 0 |
| MiniLM-L6 | 384 | 0.71 / 0.14 | 930 | 4 | 0.9957 | 0.7623 | 0 |
| MiniLM-L6 | 384 | 0.65 / 0.12 | 963 | 7 | 0.9928 | 0.7893 | 0 |
| MiniLM-L6 | 384 | 0.60 / 0.10 | 999 | 16 | 0.9842 | 0.8189 | 0 |
| MiniLM-L12-v2 | 384 | 0.71 / 0.23 | 772 | 3 | 0.9961 | 0.6328 | 3 |
| MiniLM-L12-v2 | 384 | 0.60 / 0.16 | 925 | 8 | 0.9914 | 0.7582 | 3 |
| multi-qa-MiniLM-L6-cos-v1 | 384 | 0.71 / 0.23 | 770 | 3 | 0.9961 | 0.6311 | 2 |
| multi-qa-MiniLM-L6-cos-v1 | 384 | 0.60 / 0.16 | 928 | 11 | 0.9883 | 0.7607 | 2 |
| bge-small-en-v1.5 | 384 | 0.71 / 0.23 | 178 | 0 | 1.0000 | 0.1705 | 26 |
| bge-small-en-v1.5 | 384 | 0.60 / 0.08 | 888 | 11 | 0.9882 | 0.7525 | 1 |
| bge-base-en-v1.5 | 768 | 0.60 / 0.16 | 598 | 2 | 0.9967 | 0.4902 | 2 negatives misrouted |
| snowflake-arctic-embed-s | 384 | 0.60 / 0.08 | 544 | 1 | 0.9983 | 0.4705 | 4 |

Readings:

- **MiniLM-L6 at 0.71/0.14** is the best recall gain that still keeps every
  Tool above 0.95: +9.3 recall points for 4 false positives in 934 predictions
  (overall precision 0.9957). It does **not** clear the checked-in Map 1
  baseline (precision 1.0000, no negative misroutes), so it is a deliberate
  owner decision, not a silent default change.
- **BGE and Arctic** need much lower margins to match MiniLM's precision; at
  that point they still lose recall. BGE-small at margin 0.08 has one Tool
  below the precision floor and 11 false positives for 0.7525 recall, worse
  than MiniLM's 0.9957/0.7623.
- **BGE-base** is 768-dimensional and roughly 3.4× slower on a build batch; it
  also loses recall at any precision-comparable point.
- Model size is not the bottleneck. The task is short, symmetric utterance
  matching; MiniLM's training is well suited and the corpus thresholds were
  tuned on it.

## Latency (warm, CPU, one query / 559-example index build)

| Model | Single query median | p95 | 559-example batch |
| --- | ---: | ---: | ---: |
| MiniLM-L6 | 8.7 ms | 10.5 ms | 848 ms |
| MiniLM-L12 | 14.3 ms | 15.8 ms | 1211 ms |
| multi-qa-MiniLM | 8.9 ms | 9.2 ms | 840 ms |
| bge-small | 13.8 ms | 16.0 ms | 1060 ms |
| bge-base | 30.0 ms | 32.1 ms | 2914 ms |
| arctic-s | 14.4 ms | 16.1 ms | 1108 ms |

The per-request budget is `embed.timeout: 150ms`; every candidate fits a warm
query. The build batch happens once at startup and is the visible difference
(under a second for MiniLM-L6, about three seconds for BGE-base). A Pi is
materially slower, so the 768-dimensional model is the riskiest choice there.

## Bug found and fixed

The Go embed client capped a response at 8 MB. The semantic index is embedded
in **one** batch at startup, and 559 examples at 768 dimensions is about
8.6 MB of JSON, so every 768-dimensional model failed to build with
`unexpected end of JSON input`. The cap is now 32 MB
(`assistant/internal/embed/embed.go`), which still refuses an unbounded body
but leaves room for a real index. This is independent of the model decision.

## Recommendation

1. **Keep `sentence-transformers/all-MiniLM-L6-v2` and 0.71 / 0.23** as the
   shipped default. It is the only configuration that meets the owner's
   precision-first rule with zero false positives on the corpus.
2. **Improve real-world recall with better Examples**, not a new model. The
   lowest-recall Tools are `spotify.volume` (0.19), `spotify.previous` (0.20),
   `spotify.play_playlist` (0.22), `spotify.queue_add` (0.24), `spotify.next`
   (0.29), `voice.current` (0.35), `spotify.repeat`/`spotify.like` (0.40),
   `spotify.pause` (0.45), `spotify.play` (0.48), `system.status` (0.50) and
   the three `reminder.*` Tools (~0.54). Adding natural paraphrase variants to
   those Tools' `Examples` raises recall without moving the threshold.
3. **Optional, owner decision:** lower `semantic.margin` to 0.14 and update the
   recorded baseline. That buys about nine recall points for four false
   positives (precision 0.9957); every Tool stays above 0.95. Do not do this
   silently: the baseline file is the agreed no-regression contract, and a
   wrong Tool call is worse than an Agent fallback.

## Reproduce

```sh
# Build and run a candidate beside the shipped model.
docker build --build-arg EMBED_MODEL=BAAI/bge-small-en-v1.5 -t clanka/embed:bge-small embed
docker run -d --name embed-bge -p 127.0.0.1:18086:8085 clanka/embed:bge-small
cd assistant
ROUTING_EVAL_EMBED_URL=http://127.0.0.1:18086 \
  ROUTING_EVAL_THRESHOLD=0.60 ROUTING_EVAL_MARGIN=0.08 \
  go test ./internal/pipeline -run TestRoutingEvalAgainstMiniLM -count=1 -v
```

The eval never rewrites the baseline; compare the `ROUTING_EVAL_TOTAL` and
per-Tool lines by hand, then update `routing_baseline.yaml` only as an explicit
decision.
