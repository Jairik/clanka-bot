| Scenario | Said (synthesized) | Observed route | STT transcript | Response | #12 budget | Reply audio | Verdict |
| --- | --- | --- | --- | ---: | --- | ---: | --- |
| control-time | what time is it | control_phrase/time.now/ok 'What time is it?' | What time is it? | 734 ms | within p50 | 1.02 s | PASS |
| semantic-time | what time is it right now | fast_path/time.now/ok 'What time is it right now?' | What time is it right now? | 801 ms | within p50 | 0.51 s | PASS |
| date | what's today's date | fast_path/date.today/ok "What's today's date?" | What's today's date? | 743 ms | within p50 | 2.08 s | PASS |
| timer-set-words | set a timer for ten minutes | fast_path/timer.set/ok 'Set a timer for ten minutes.' | Set a timer for ten minutes. | 735 ms | within p50 | 0.38 s | PASS |
| timer-set-seconds-miss | set a timer for ten seconds | fast_path/timer.set/ok 'Set a timer for ten seconds.' | Set a timer for ten seconds. | 736 ms | within p50 | 0.48 s | PASS |
| agent-timer-set | agent please run a timer test for five minutes | agent_path/timer.set/ok 'Agent, please run a timer test for five minutes.' | Agent, please run a timer test for five minutes. | 3720 ms | p50-p95 | 7.78 s | PASS |
| timer-list | how long is left on my timer | fast_path/timer.list/ok 'How long is left on my timer?' | How long is left on my timer? | 804 ms | within p50 | 2.53 s | PASS |
| timer-cancel | cancel the timer | fast_path/timer.cancel/ok 'Cancel the timer.' | Cancel the timer. | 857 ms | within p50 | 0.64 s | PASS |
| volume-set | set the volume to thirty percent | fast_path/audio.volume_set/ok 'Set the volume to thirty percent.' | Set the volume to thirty percent. | 854 ms | within p50 | 1.15 s | PASS |
| volume-up | louder | control_phrase/audio.volume_set/ok 'Louder.' | Louder. | 812 ms | within p50 | 0.22 s | PASS |
| audio-stop | stop | control_phrase/audio.stop/ok 'Stop.' | Stop. | 696 ms | within p50 | 0.29 s | PASS |
| weather-control | what's the weather like | control_phrase/weather.current/failed "What's the weather like?" | What's the weather like? | 3395 ms | OVER p95 | 0.80 s | FAIL |
| weather-city | what's the weather in Berlin | fast_path/weather.current/ok "What's the weather in Berlin?" | What's the weather in Berlin? | 1083 ms | within p50 | 1.95 s | PASS |
| agent-question | agent what is the capital of France | agent_path/-/ok 'Agent, what is the capital of France?' | Agent, what is the capital of France? | 2117 ms | within p50 | 2.11 s | PASS |
| agent-history | agent what did I ask before | agent_path/-/ok 'Agent, what did I ask before?' | Agent, what did I ask before? | 2137 ms | within p50 | 1.92 s | PASS |
| agent-tool | agent what time is it | agent_path/time.now/ok 'Agent, what time is it?' | Agent, what time is it? | 2157 ms | within p50 | 5.70 s | PASS |
| barge-in | tell me a long story | agent_path/-/cancelled 'Tell me a long story.'; control_phrase/time.now/ok 'What time is it?' | What time is it? | 2257/751 ms | within p50 | 4.32 s | PASS |
| provider-failure | agent trigger a provider failure | agent_path/-/failed 'Agent trigger a provider failure.' | Agent trigger a provider failure. | 2174 ms | within p50 | 1.09 s | PASS |
| embed-stopped | what's the temperature outside | agent_path/-/ok "What's the temperature outside?" | What's the temperature outside? | 2225 ms | within p50 | 2.37 s | PASS |
| agent-timer-fire | agent please run a timer test for ten seconds | agent_path/timer.set/ok 'Agent, please run a timer test for ten seconds.' | Agent, please run a timer test for ten seconds. | 2214 ms | within p50 | 9.44 s | PASS |
