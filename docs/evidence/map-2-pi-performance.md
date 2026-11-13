Pi diagnostics and TTS investigation — 2026-11-12, app commit 27880d2.

Baseline: 8 GB Pi 5, load averages 0.70/0.83/0.76, approximately 2.5 GiB RAM used and 5.4 GiB available, 65 MiB swap occupied but no swap-in/out during sampled intervals. No obvious competing CPU-heavy process before the isolated benchmark. During tests additional Python model process is expected. CPU reaches 2.4 GHz with ondemand governor. Temperature 59.3 C during diagnostics, earlier 67 C; throttled=0x0. No recent OOM, undervoltage, throttle or disk-error kernel messages in sampled 30-minute window. Root filesystem 47% used, approximately 30 GB available. No Docker CPU/memory limits. PSI unavailable in this kernel.

Memory accounting blocker: kernel command line includes cgroup_disable=memory, cgroup memory controller absent, Docker stats reports 0B. #69/#74 container RAM acceptance cannot use these values. Enabling memory accounting requires boot-config change and reboot, neither performed.

TTS benchmark, same seeded short phrase and resident model per run: current model 4 threads 0.258/0.256x playback speed, 2 threads 0.261/0.267x, 1 thread 0.330/0.317x. Smaller built-in english_18m_2026-10: 1 thread 1.224/1.124x, 2 threads 0.895/0.983x, 4 threads 1.032/1.066x. Smaller-model sustained tests, 1 thread: 16 words 1.219x (first chunk 205 ms); 23 words 1.194x (first chunk 196 ms). These are isolated synthesis tests with live stack resident, not a full conversation load or acceptance matrix.

Smaller-model weights downloaded using installed pinned config, tested in separate process; live server configuration unchanged. Sample on Pi: ~/.local/state/clanka-diagnostics/tts-18m-sample.wav, played through Echo for owner quality review. Existing original model remains live. No image, Compose, repo or boot changes in this investigation.

Logs on desktop: /tmp/clanka-pi-tts-benchmark.log, /tmp/clanka-pi-tts-small-benchmark.log, /tmp/clanka-pi-tts-18m-sustained.log. Upstream model source: https://github.com/kyutai-labs/pocket-tts .

Deployment candidate: Pi-only TTS_LANGUAGE=english_18m_2026-10 and TTS_THREADS=1. Owner confirms the smaller model sample is clear and acceptable. Compose exposes TTS_THREADS through OMP_NUM_THREADS; default 4 preserves prior CPU behavior elsewhere. Language changes are baked by existing TTS build args. Live rollout and correct wake-to-time reply remain pending.
