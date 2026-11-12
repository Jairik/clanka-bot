# Utility Tools

The quick-win utility Tools are `time.in_city`, `date.countdown`,
`calc.evaluate`, `units.convert`, `random.flip`, `random.roll` and
`random.pick`. All are `read` and `LocalOnlyNo`: they have no side effects and
their results are safe for the Agent path. None has a `tools.<name>` config
block; the limits below are code constants so a malformed or huge request cannot
turn one turn into unbounded work.

All of them run on the semantic Fast path through their `Examples` and `Extract`
functions. `Extract` is also a guard: when the utterance does not fit the Tool's
documented grammar, it returns false and the request goes to the Agent path
instead of producing a wrong Fast path answer. The routing corpus labels live in
`internal/pipeline/testdata/routing_eval.yaml`.

Try one through the localhost debug entry:

```sh
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"what time is it in Tokyo"}'
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"how many days until Christmas"}'
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"what is 15% of 80"}'
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"convert 5 miles to km"}'
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"roll a d20"}'
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"pick between pizza and tacos"}'
```

## `time.in_city`

`assistant/tools/clock/city.go` ships a fixed city-to-IANA table: **278 named
cities and 324 spoken aliases across 149 zones**. A lookup normalises the name
and calls `time.LoadLocation`; the binary embeds `time/tzdata` through
`internal/tz`, so there is no geocoding, IP lookup or HTTP request. The
`TestCityMakesNoNetworkCall` test poisons the default HTTP transport to keep it
that way.

Coverage is deliberately bounded to major cities. The table is the answer to
"which cities do you know?"; a missing city gets a truthful reply naming a few
examples rather than a guessed zone. Add a city by adding one map entry and
running the coverage test, which loads every shipped zone. A country suffix
after a comma or in the country list (`Tokyo, Japan`, `Tokyo Japan`) is
stripped; a state or province suffix is not, because "Washington state" is not
the same request as "Washington".

Bare names that map to more than one zone are refused with a hint:
`Portland`, `Birmingham`, `Springfield`, `Cambridge` and `Columbus`. Qualified
forms such as `Portland, Oregon`, `Portland, Maine`, `Birmingham, UK` and
`Cambridge, Massachusetts` resolve. London, Paris, Rome, Naples, Melbourne and
Sydney are mapped to their best-known country; say the qualifier when a
different one is meant.

`Extract` requires a time word (`time` or `clock`) and the last " in " before a
known city, so "what time zone is Tokyo in", "what's the weather in Tokyo" and
"how many people live in Tokyo" stay off the Fast path.

## `date.countdown`

`assistant/tools/clock/countdown.go` counts **calendar days** from today to the
next fixed date, in the effective `timezone` setting. It computes date-only UTC
ordinals, so a DST change between today and the target never turns 271 calendar
days into 270.95 elapsed days; the test covers the Europe/Berlin spring-forward
boundary. With no timezone set, the `TZ` fallback and then UTC apply, exactly as
for clock-time Alarms and Reminders.

Supported events are **Christmas** (25 December), **New Year's Day** (1
January), **Halloween** (31 October) and **Valentine's Day** (14 February).
"Today" is zero and "tomorrow" is one. Adding an event is one fixed-date table
entry; a floating holiday would need its own rule and is not promised. The
`timezone` setting is read per call, so a change applies without a restart.

`Extract` accepts a day/countdown marker (`days until`, `days till`, `days to`,
`countdown to`, `until`, `till`, `before`) only when the event is known. "How
many days until my birthday" and "what day is Christmas this year" stay on the
Agent path.

## `calc.evaluate`

`assistant/tools/calc/calc.go` is a hand-written lexer and recursive-descent
parser, not `eval`, `exec`, a shell or a calculator process. The whitelisted
grammar is:

```
expression := term (('+' | '-') term)*
term       := unary (('*' | '/' | 'of') unary)*
unary      := ('+' | '-') unary | postfix
postfix    := primary '%'*
primary    := number | '(' expression ')'
```

`%` is a postfix percent (`15%` is 0.15), and `of` is multiplication, so
`15% of 80` is 12. The usual precedence applies: `3 + 4 * 2` is 11 and
parentheses override it. Spoken forms such as `plus`, `minus`, `times`,
`multiplied by`, `divided by`, `over`, `percent` and `x` are accepted, as are
the `×`, `÷` and Unicode minus signs. Thousands commas are removed only between
digits.

Limits: 200 bytes of expression, 128 tokens, 16 nested parentheses and 64
operations. Literals and intermediate values are bounded to `1e15`; a result
that is NaN, infinite or larger is a spoken error, not a panic. Zero division
says "I can't divide by zero." Any unknown character or word is a parse error;
`Extract` only accepts an expression that already evaluates, so "what's the
square root of 144", code-like input and "calculate my taxes" go to the Agent
path.

## `units.convert`

`assistant/tools/units/units.go` has a closed table of common units:

| Dimension | Units |
| --- | --- |
| Length | mm, cm, m, km, in, ft, yd, mi, nautical mile |
| Mass | mg, g, kg, tonne, oz, lb, stone |
| Volume | ml, l, cup, pt, qt, gal, fl oz, tbsp, tsp (US measures) |
| Temperature | C, F, K (affine) |
| Time | s, min, h, day, week |
| Speed | m/s, km/h, mph, knot |
| Area | m², km², hectare, acre, square foot, square inch |

Each unit converts through its dimension's base (meter, kilogram, liter,
second, meter per second, square meter). Temperature uses affine formulas with
Celsius as the base, so `0 C = 32 F`, `100 C = 212 F` and `-40 C = -40 F`.
Cross-dimension requests such as miles to kilograms are a spoken error naming
both units. Unknown units, including every currency, are refused with the
supported dimensions; there is no exchange-rate lookup, no dynamic unit file and
no network call. Values are finite and bounded to `1e15`, and a result beyond
that is refused.

Accepted phrasings include `convert 5 miles to km`, `5 miles in km`,
`how many km in 5 miles`, `how many minutes in a day` and
`what is 100 fahrenheit in celsius`. `Extract` requires both units to be known,
so "how many calories in an apple" and "convert 5 miles to dollars" stay on the
Agent path; the Tool itself still gives the helpful unknown-unit message when
the Agent or a direct call reaches it.

## `random`

`assistant/tools/random` uses `crypto/rand` through one `Intn` seam. Production
draws are uniform without modulo bias; tests inject a fixed sequence through
`NewModuleWithSource`. There is no clock seed and no global random state.

- `random.flip` maps 0 to heads and 1 to tails.
- `random.roll` accepts `Count` 1–100 and `Sides` 2–1000, inclusive. A d20 is 1
  through 20. Zero fields default to one d6, so an Agent call with no arguments
  is a valid die roll. Every die is returned in `rolls`, with `total`.
  `Extract` reads `d20`, `2d6`, `roll a six-sided die`, `roll two dice with
  eight sides` and a `pair of dice`; it does not treat "roll out the red
  carpet" or "roll the dough" as a dice request.
- `random.pick` needs at least two options and accepts up to 50, each at most
  100 characters. It chooses one index and returns the option exactly as the
  user said it, trimming only surrounding whitespace and punctuation. Lists are
  split on commas, then " or ", then " and "; "pick a number between 1 and 10"
  is treated as a range and stays on the Agent path. It never maps "pizza" to a
  category or invents an option.
