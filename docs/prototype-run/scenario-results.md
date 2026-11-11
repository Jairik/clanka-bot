| Scenario | Said (synthesized) | Observed route | STT transcript | Response | #12 budget | Reply audio | Verdict |
| --- | --- | --- | --- | ---: | --- | ---: | --- |
| control-time | what time is it | control_phrase/time.now/ok 'What time is it?' | What time is it? | 628 ms | within p50 | 0.74 s | PASS |
| semantic-time | what time is it right now | fast_path/time.now/ok 'What time is it right now?' | What time is it right now? | 827 ms | within p50 | 0.74 s | PASS |
| date | what's today's date | fast_path/date.today/ok "What's today's date?" | What's today's date? | 836 ms | within p50 | 2.37 s | PASS |
| timer-set-words | set a timer for ten minutes | agent_path/-/ok 'Set a timer for ten minutes.' | Set a timer for ten minutes. | 2167 ms | within p50 | 2.30 s | FAIL |
| timer-set-seconds-miss | set a timer for ten seconds | agent_path/-/ok 'Set a timer for ten seconds.' | Set a timer for ten seconds. | 2208 ms | within p50 | 1.92 s | FAIL |
| agent-timer-set | agent please run a timer test for five minutes | agent_path/timer.set/ok 'Agent, please run a timer test for five minutes.' | Agent, please run a timer test for five minutes. | 2109 ms | within p50 | 7.97 s | PASS |
| timer-list | how long is left on my timer | fast_path/timer.list/ok 'How long is left on my timer?' | How long is left on my timer? | 901 ms | within p50 | 1.86 s | PASS |
| timer-cancel | cancel the timer | fast_path/timer.cancel/ok 'Cancel the timer.' | Cancel the timer. | 896 ms | within p50 | 0.77 s | PASS |
| volume-set | set the volume to thirty percent | fast_path/audio.volume_set/ok 'Set the volume to thirty percent.' | Set the volume to thirty percent. | 822 ms | within p50 | 1.09 s | PASS |
| volume-up | louder | control_phrase/audio.volume_set/ok 'Louder.' | Louder. | 715 ms | within p50 | 0.22 s | PASS |
| audio-stop | stop | control_phrase/audio.stop/ok 'Stop.' | Stop. | 674 ms | within p50 | 0.29 s | PASS |
| weather-control | what's the weather like | control_phrase/weather.current/failed "What's the weather like?" | What's the weather like? | 6462 ms | OVER p95 | 1.44 s | FAIL |
| weather-city | what's the weather in Berlin | fast_path/weather.current/ok "What's the weather in Berlin?" | What's the weather in Berlin? | 896 ms | within p50 | 0.35 s | PASS |
| agent-question | agent what is the capital of France | agent_path/-/ok 'Agent, what is the capital of France?' | Agent, what is the capital of France? | 2235 ms | within p50 | 2.59 s | PASS |
| agent-history | agent what did I ask before | agent_path/-/ok 'Agent, what did I ask before?' | Agent, what did I ask before? | 2145 ms | within p50 | 1.57 s | PASS |
| agent-tool | agent what time is it | agent_path/time.now/ok 'Agent. What time is it?' | Agent. What time is it? | 2174 ms | within p50 | 4.13 s | PASS |
| barge-in | tell me a long story | agent_path/-/cancelled 'Tell me a long story.'; control_phrase/time.now/ok 'What time is it?' | What time is it? | 2219/656 ms | within p50 | 4.42 s | PASS |
| provider-failure | agent trigger a provider failure | agent_path/-/failed 'Agent trigger a provider failure.' | Agent trigger a provider failure. | 2143 ms | within p50 | 1.02 s | PASS |
| embed-stopped | what's the temperature outside | agent_path/-/ok "What's the temperature outside?" | What's the temperature outside? | 2232 ms | within p50 | 2.18 s | PASS |
| agent-timer-fire | agent please run a timer test for ten seconds | agent_path/timer.set/ok 'Agent, please run a timer test for ten seconds.' | Agent, please run a timer test for ten seconds. | 2138 ms | within p50 | 8.45 s | PASS |
