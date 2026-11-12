#!/usr/bin/env python3
"""#29 dev-box prototype run: scripted voice turns through the real stack.

What is real: all five Compose services, the wake word, endpointing, STT, the
Fast path, Tool execution, the Agent loop, TTS and Pulse playback.

What is not: there is no human in this room. Every utterance is Pocket TTS
spoken into a null sink whose monitor feeds the assistant's capture source, so
this is synthesized loopback audio, not human speech. The Agent path has no
OpenRouter key, so it talks to a scripted OpenAI-compatible test double
(`fake_provider.py`) over the Compose network. Its replies and timings are
scripted; never read them as model latency.

The run is isolated: its own Compose project (`clanka-e2e29`), its own loopback
ports, and its own two null sinks. It never stops a container it did not start
and never changes the host's default sink or source.

Usage (from the repo root):

    python3 scripts/prototype-run/prototype_run.py            # full run
    python3 scripts/prototype-run/prototype_run.py --only barge-in,provider-failure
    python3 scripts/prototype-run/prototype_run.py --keep     # leave the stack up

Artifacts land in `scripts/prototype-run/.run/` (gitignored). `--publish DIR`
copies the evidence that belongs in git to DIR.

Only the pure decision code is unit-tested; the Docker and PulseAudio
orchestration is glue. See `tests/test_prototype_run.py`.
"""

import argparse
import array
import json
import math
import os
import pathlib
import re
import shutil
import subprocess
import sys
import time

REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
HARNESS_DIR = pathlib.Path(__file__).resolve().parent
ASSISTANT_DIR = REPO_ROOT / "assistant"
OUT_DIR = HARNESS_DIR / ".run"

PROJECT = "clanka-e2e29"
SINK_IN = "clanka29_in"
SINK_OUT = "clanka29_out"
HTTP_PORT = 19180
DEBUG_PORT = 19181
TTS_PORT = 19200
STT_PORT = 19300
CAPTURE_RATE = 16000
TTS_RATE = 24000
FRAME_SAMPLES = 512  # 32 ms at 16 kHz, the endpointer's frame size.
ENDPOINT_THRESHOLD_DBFS = -38.0  # assistant/config.yaml stt.endpoint.threshold_dbfs

# The Fast path and Agent path Response budgets from issue #12, in milliseconds.
FAST_P50_MS, FAST_P95_MS = 1200, 2000
AGENT_P50_MS, AGENT_P95_MS = 2500, 4000


# --------------------------------------------------------------------------
# Pure seams: parsing, verdicts, budgets, and the fake provider's request log.
# --------------------------------------------------------------------------


def parse_request_lines(lines):
    """Return the assistant's `request` log records from raw log lines.

    Compose prefixes are tolerated (like `latency-report` does), non-JSON and
    other messages are skipped, and a truncated JSON line is ignored.
    """
    records = []
    for raw in lines:
        line = raw.strip()
        brace = line.find("{")
        if brace > 0:
            line = line[brace:]
        if not line.startswith("{"):
            continue
        try:
            record = json.loads(line)
        except ValueError:
            continue
        if record.get("msg") != "request":
            continue
        records.append(record)
    return records


def budget_for_path(path):
    """Return the #12 (p50, p95) Response budget in ms, or None."""
    if path in ("fast_path", "control_phrase", "confirmation"):
        return FAST_P50_MS, FAST_P95_MS
    if path == "agent_path":
        return AGENT_P50_MS, AGENT_P95_MS
    return None


def budget_verdict(path, response_ms):
    """Classify one request against the p50/p95 budget, never inventing a zero."""
    if response_ms is None:
        return "not measured"
    budget = budget_for_path(path)
    if budget is None:
        return "no budget"
    p50, p95 = budget
    if response_ms <= p50:
        return "within p50"
    if response_ms <= p95:
        return "p50-p95"
    return "OVER p95"


def _wanted(value):
    return value if isinstance(value, list) else [value]


def _step_problem(step, record):
    if "path" in step and record.get("path") not in _wanted(step["path"]):
        return "path %r, want %r" % (record.get("path"), step["path"])
    if "tool" in step and record.get("tool", "") not in _wanted(step["tool"]):
        return "tool %r, want %r" % (record.get("tool") or "(none)", step["tool"])
    outcome = step.get("outcome", "ok")
    if record.get("outcome") not in _wanted(outcome):
        return "outcome %r, want %r" % (record.get("outcome"), outcome)
    if "fallthrough_reason" in step and record.get("fallthrough_reason") != step["fallthrough_reason"]:
        return "fallthrough_reason %r, want %r" % (
            record.get("fallthrough_reason"),
            step["fallthrough_reason"],
        )
    if "agent_error" in step and record.get("agent_error") != step["agent_error"]:
        return "agent_error %r, want %r" % (record.get("agent_error"), step["agent_error"])
    return ""


def _observed(records):
    parts = []
    for record in records:
        text = "%s/%s/%s" % (
            record.get("path") or "(none)",
            record.get("tool") or "-",
            record.get("outcome") or "(none)",
        )
        transcript = record.get("transcript")
        if transcript:
            text += " %r" % transcript
        parts.append(text)
    return "; ".join(parts)


def classify(expect, records, extra=None):
    """Decide pass/fail for one scenario from its request lines.

    `expect` carries `steps` (one per expected request line), an optional
    `barge_in` flag, and per-step path/tool/outcome/fallthrough/agent_error.
    `extra` carries observations from outside the request line, such as the
    `voice: barge-in` log line.
    """
    extra = extra or {}
    steps = expect.get("steps") or []
    if not records:
        return {
            "verdict": "fail",
            "reason": "no request line seen",
            "observed": "",
        }
    if len(records) != len(steps):
        return {
            "verdict": "fail",
            "reason": "expected %d request line(s), saw %d" % (len(steps), len(records)),
            "observed": _observed(records),
        }
    if expect.get("barge_in") and not extra.get("barge_in"):
        return {
            "verdict": "fail",
            "reason": "the request lines matched but no voice: barge-in log line appeared",
            "observed": _observed(records),
        }
    for index, (step, record) in enumerate(zip(steps, records), 1):
        problem = _step_problem(step, record)
        if problem:
            return {
                "verdict": "fail",
                "reason": "request %d: %s" % (index, problem),
                "observed": _observed(records),
            }
    return {"verdict": "pass", "reason": "all request lines matched", "observed": _observed(records)}


def find_fake_request(records, last_user_contains):
    """Find the test double's request whose final user message contains text."""
    needle = last_user_contains.lower()
    for record in records:
        messages = record.get("messages") or []
        if not messages:
            continue
        last = messages[-1]
        if last.get("role") == "user" and needle in str(last.get("content", "")).lower():
            return record
    return None


def _has_message(record, role, text, exclude_last=False):
    """True when the request carries a message of the role containing text."""
    needle = text.lower()
    messages = (record or {}).get("messages") or []
    if exclude_last:
        messages = messages[:-1]
    return any(
        message.get("role") == role and needle in str(message.get("content", "")).lower()
        for message in messages
    )


def has_prior_user_turn(record, text):
    """True when the request carries an earlier user message containing text."""
    return _has_message(record, "user", text, exclude_last=True)


def has_prior_assistant_reply(record, text):
    """True when the request carries an earlier assistant message containing text."""
    return _has_message(record, "assistant", text, exclude_last=True)


def has_tool_result(record, contains):
    """True when the request carries a Tool result message containing text."""
    return _has_message(record, "tool", contains)


def prefix_removed(text):
    """True when an Agent request prefix is gone from the model request.

    The prefix table matches on a normalized string, but STT can punctuate the
    prefix ("Agent. What time is it?"). If the last user message still starts
    with the word "agent", the prefix survived; that is a routing defect even
    when the turn happened to reach the Agent path by falling through.
    """
    return re.match(r"^agent\b", str(text).strip().lower()) is None


def scenario_shape_errors(scenarios):
    """Validate the scripted set: ids are unique and every turn is fully specified."""
    errors = []
    seen = set()
    for index, scenario in enumerate(scenarios):
        sid = scenario.get("id")
        if not sid:
            errors.append("scenario %d has no id" % index)
            continue
        if sid in seen:
            errors.append("duplicate scenario id %r" % sid)
        seen.add(sid)
        if not str(scenario.get("say", "")).strip():
            errors.append("%s has no utterance" % sid)
        steps = (scenario.get("expect") or {}).get("steps")
        if not steps:
            errors.append("%s has no expected request steps" % sid)
            continue
        for step_index, step in enumerate(steps):
            if not step.get("path"):
                errors.append("%s step %d has no path" % (sid, step_index))
    return errors


# --------------------------------------------------------------------------
# The scripted turn list. Order matters only for `agent-history`, which checks
# that the preceding Agent turn reached the provider.
# --------------------------------------------------------------------------

SCENARIOS = [
    {
        "id": "control-time",
        "say": "what time is it",
        "note": "Control phrase; never calls embed",
        "expect": {"steps": [{"path": "control_phrase", "tool": "time.now"}]},
    },
    {
        "id": "semantic-time",
        "say": "what time is it right now",
        "note": "Semantic Fast path (not in the Control table)",
        "expect": {"steps": [{"path": "fast_path", "tool": "time.now"}]},
    },
    {
        "id": "date",
        "say": "what's today's date",
        "expect": {"steps": [{"path": "fast_path", "tool": "date.today"}]},
    },
    {
        "id": "timer-set-words",
        "say": "set a timer for ten minutes",
        "note": "Observed miss: 0.786, margin 0.122 < 0.23. Without STT's period the score is 1.0 and margin 0.359",
        "expect": {"steps": [{"path": "fast_path", "tool": "timer.set"}]},
    },
    {
        "id": "timer-set-seconds-miss",
        "say": "set a timer for ten seconds",
        "note": "Second observed timer.set miss (margin 0.063)",
        "expect": {"steps": [{"path": "fast_path", "tool": "timer.set"}]},
    },
    {
        "id": "agent-timer-set",
        "say": "agent please run a timer test for five minutes",
        "note": "Timer Tool through the Agent path: the test double calls timer.set(300 s)",
        "expect": {"steps": [{"path": "agent_path", "tool": "timer.set"}]},
        "verify": [{"kind": "tool_result", "contains": "duration_seconds"}],
    },
    {
        "id": "timer-list",
        "say": "how long is left on my timer",
        "expect": {"steps": [{"path": "fast_path", "tool": "timer.list"}]},
    },
    {
        "id": "timer-cancel",
        "say": "cancel the timer",
        "expect": {"steps": [{"path": "fast_path", "tool": "timer.cancel"}]},
    },
    {
        "id": "volume-set",
        "say": "set the volume to thirty percent",
        "expect": {"steps": [{"path": "fast_path", "tool": "audio.volume_set"}]},
    },
    {
        "id": "volume-up",
        "say": "louder",
        "expect": {"steps": [{"path": "control_phrase", "tool": "audio.volume_set"}]},
    },
    {
        "id": "audio-stop",
        "say": "stop",
        "expect": {"steps": [{"path": "control_phrase", "tool": "audio.stop"}]},
    },
    {
        "id": "weather-control",
        "say": "what's the weather like",
        "note": "Network-bound Tool; may exceed the budget on its own",
        "expect": {"steps": [{"path": "control_phrase", "tool": "weather.current"}]},
    },
    {
        "id": "weather-city",
        "say": "what's the weather in Berlin",
        "expect": {"steps": [{"path": "fast_path", "tool": "weather.current"}]},
    },
    {
        "id": "agent-question",
        "say": "agent what is the capital of France",
        "note": "Prefix must short-circuit the Fast path, not merely fall through to it",
        "expect": {"steps": [{"path": "agent_path", "fallthrough_reason": ""}]},
        "verify": [{"kind": "prefix_stripped", "contains": "capital of france"}],
    },
    {
        "id": "agent-history",
        "say": "agent what did I ask before",
        "note": "Checks the provider received the previous turn",
        "expect": {"steps": [{"path": "agent_path"}]},
        "verify": [
            {
                "kind": "history",
                "contains": "what did i ask",
                "prior": "capital of france",
                "reply": "Paris",
            }
        ],
    },
    {
        "id": "agent-tool",
        "say": "agent what time is it",
        "note": "The test double calls time.now, then answers with its result",
        "expect": {"steps": [{"path": "agent_path", "tool": "time.now"}]},
        "verify": [{"kind": "tool_result", "contains": "time"}],
    },
    {
        "id": "barge-in",
        "say": "tell me a long story",
        "barge_say": "what time is it",
        "note": "Wake word during a slow Agent reply, then a Fast path turn",
        "expect": {
            "barge_in": True,
            "steps": [
                {"path": "agent_path", "outcome": "cancelled"},
                {"path": "control_phrase", "tool": "time.now"},
            ],
        },
        "wait_timeout": 45,
    },
    {
        "id": "provider-failure",
        "say": "agent trigger a provider failure",
        "note": "HTTP 401 from the test double: the same path a bad key takes",
        "expect": {
            "steps": [
                {"path": "agent_path", "outcome": "failed", "agent_error": "unavailable"}
            ]
        },
    },
    {
        "id": "embed-stopped",
        "say": "what's the temperature outside",
        "note": "embed stopped: semantic match fails and the request falls through",
        "pre": "stop_embed",
        "post": "start_embed",
        "expect": {
            "steps": [
                {
                    "path": "agent_path",
                    "outcome": "ok",
                    "fallthrough_reason": "embed_failed",
                }
            ]
        },
    },
    {
        "id": "agent-timer-fire",
        "say": "agent please run a timer test for ten seconds",
        "note": "Timer Tool through the Agent path; the alert is not a request line",
        "expect": {"steps": [{"path": "agent_path", "tool": "timer.set"}]},
        "wait_for_log": "timer fired",
        "wait_timeout": 45,
    },
]


# --------------------------------------------------------------------------
# Runner glue.
# --------------------------------------------------------------------------


class LogTail:
    """Follow a file another process appends to, and expose complete lines."""

    def __init__(self, path):
        self.path = pathlib.Path(path)
        self.pos = 0
        self.buffer = ""
        self.lines = []

    def poll(self):
        try:
            with open(self.path, "r", encoding="utf-8", errors="replace") as handle:
                handle.seek(self.pos)
                chunk = handle.read()
                self.pos = handle.tell()
        except FileNotFoundError:
            return
        if not chunk:
            return
        self.buffer += chunk
        parts = self.buffer.split("\n")
        self.buffer = parts.pop()
        self.lines.extend(parts)

    def wait(self, predicate, timeout):
        deadline = time.time() + timeout
        while time.time() < deadline:
            self.poll()
            if predicate():
                return True
            time.sleep(0.05)
        self.poll()
        return predicate()

    def count(self, needle):
        return sum(1 for line in self.lines if needle in line)


def slug(text):
    return re.sub(r"[^a-z0-9]+", "-", text.lower()).strip("-")


def pcm_stats(path, rate=CAPTURE_RATE):
    """Duration and loudness of a raw mono PCM16 capture."""
    try:
        data = pathlib.Path(path).read_bytes()
    except FileNotFoundError:
        return {"seconds": 0.0, "loud_seconds": 0.0, "peak_dbfs": None}
    even = len(data) // 2 * 2
    if even < 2:
        return {"seconds": 0.0, "loud_seconds": 0.0, "peak_dbfs": None}
    samples = array.array("h")
    samples.frombytes(data[:even])
    frames = 0
    loud = 0
    peak = -120.0
    for start in range(0, len(samples) - FRAME_SAMPLES + 1, FRAME_SAMPLES):
        segment = samples[start : start + FRAME_SAMPLES]
        rms = math.sqrt(sum(float(value) * value for value in segment) / len(segment))
        level = 20 * math.log10(rms / 32768) if rms > 0 else -120.0
        peak = max(peak, level)
        frames += 1
        if level > -50.0:
            loud += 1
    return {
        "seconds": round(len(samples) / rate, 2),
        "loud_seconds": round(loud * FRAME_SAMPLES / rate, 2),
        "peak_dbfs": round(peak, 1),
    }


def measure_mic(device, seconds=4):
    """Passively sample a human mic: availability and idle noise only."""
    out_path = OUT_DIR / "mic-probe.pcm"
    proc = subprocess.Popen(
        [
            "parec",
            "--device=" + device,
            "--rate=%d" % CAPTURE_RATE,
            "--channels=1",
            "--format=s16le",
            "--raw",
        ],
        stdout=open(out_path, "wb"),
        stderr=subprocess.DEVNULL,
    )
    # parec writes in blocks, so a fixed sleep under-collects; wait for the
    # requested duration to land in the file instead.
    target_bytes = seconds * CAPTURE_RATE * 2
    deadline = time.time() + seconds + 5
    while time.time() < deadline:
        if out_path.exists() and out_path.stat().st_size >= target_bytes:
            break
        time.sleep(0.1)
    proc.terminate()
    try:
        proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        proc.kill()
    data = pathlib.Path(out_path).read_bytes()
    even = len(data) // 2 * 2
    samples = array.array("h")
    if even >= 2:
        samples.frombytes(data[:even])
    levels = []
    for start in range(0, max(0, len(samples) - FRAME_SAMPLES + 1), FRAME_SAMPLES):
        segment = samples[start : start + FRAME_SAMPLES]
        rms = math.sqrt(sum(float(value) * value for value in segment) / len(segment))
        levels.append(20 * math.log10(rms / 32768) if rms > 0 else -120.0)
    levels.sort()
    over = sum(1 for level in levels if level > ENDPOINT_THRESHOLD_DBFS)
    rms_all = math.sqrt(sum(float(value) * value for value in samples) / len(samples)) if samples else 0
    rms_dbfs = 20 * math.log10(rms_all / 32768) if rms_all > 0 else -120.0
    percentile = lambda p: levels[min(len(levels) - 1, max(0, int(len(levels) * p / 100)))] if levels else -120.0
    lines = [
        "device=%s" % device,
        "seconds=%.2f" % (len(samples) / CAPTURE_RATE),
        "rms_dbfs=%.1f" % rms_dbfs,
        "frame_dbfs p10=%.1f p50=%.1f p90=%.1f peak=%.1f"
        % (percentile(10), percentile(50), percentile(90), levels[-1] if levels else -120.0),
        "frames_over_threshold=%d/%d threshold_dbfs=%.1f"
        % (over, len(levels), ENDPOINT_THRESHOLD_DBFS),
        "note=passive measurement only; no human speech was produced",
    ]
    if rms_dbfs > ENDPOINT_THRESHOLD_DBFS:
        lines.append(
            "WARNING: idle noise is at or above the endpointer threshold; a human wake "
            "on this mic setting would open an utterance on noise and run to max_utterance"
        )
    return "\n".join(lines) + "\n"


class Run:
    def __init__(self, args):
        self.args = args
        self.tag = args.tag
        self.env = dict(os.environ)
        self.env.update(
            {
                "GIT_SHA": self.tag,
                "ASSISTANT_HTTP_PORT": str(HTTP_PORT),
                "ASSISTANT_DEBUG_PORT": str(DEBUG_PORT),
                "ASSISTANT_PULSE_SOURCE": SINK_IN + ".monitor",
                "ASSISTANT_PULSE_SINK": SINK_OUT,
                "WAKEWORD_THRESHOLD": "0.05",
                "OPENROUTER_API_KEY": "fake-provider-not-a-secret",
            }
        )
        self.sink_modules = []
        self.follow = None
        self.tail = LogTail(OUT_DIR / "assistant.log")
        self.results = []
        self.fake_records = []

    # -- shell helpers -----------------------------------------------------

    def compose(self, args, check=True, capture=False, timeout=None):
        command = [
            "docker",
            "compose",
            "-p",
            PROJECT,
            "-f",
            "docker-compose.yml",
            "-f",
            "scripts/prototype-run/compose.override.yml",
            *args,
        ]
        return subprocess.run(
            command,
            cwd=REPO_ROOT,
            env=self.env,
            check=check,
            capture_output=capture,
            text=True,
            timeout=timeout,
        )

    def container_id(self, service):
        return self.compose(["ps", "-q", service], capture=True).stdout.strip()

    def wait_healthy(self, service, timeout=180):
        container = self.container_id(service)
        if not container:
            raise RuntimeError("no container for service %s" % service)
        deadline = time.time() + timeout
        while time.time() < deadline:
            status = subprocess.run(
                ["docker", "inspect", "-f", "{{.State.Health.Status}}", container],
                capture_output=True,
                text=True,
            ).stdout.strip()
            if status == "healthy":
                return
            time.sleep(1)
        raise RuntimeError("service %s did not become healthy" % service)

    # -- lifecycle ---------------------------------------------------------

    def generate_config(self):
        source = (ASSISTANT_DIR / "config.yaml").read_text(encoding="utf-8")
        needle = "base_url: https://openrouter.ai/api/v1"
        if needle not in source:
            raise RuntimeError("assistant/config.yaml no longer has the expected base_url")
        generated = "# GENERATED by scripts/prototype-run/prototype_run.py.\n" \
                    "# Points the Agent path at the scripted test double, not OpenRouter.\n" + \
                    source.replace(needle, "base_url: http://fakeopenai:8000/v1")
        (OUT_DIR / "config.yaml").write_text(generated, encoding="utf-8")

    def build_binaries(self):
        subprocess.run(
            ["go", "build", "-o", str(OUT_DIR / "latency-say"), "./cmd/latency-say"],
            cwd=ASSISTANT_DIR,
            check=True,
        )

    def start_sinks(self):
        existing = subprocess.check_output(["pactl", "list", "short", "sinks"], text=True)
        for name in (SINK_IN, SINK_OUT):
            if name in existing:
                # A previous interrupted run left the same named sink. Reuse it
                # and leave it alone at teardown rather than touching host state.
                print("reusing existing null sink " + name)
                continue
            module = subprocess.check_output(
                [
                    "pactl",
                    "load-module",
                    "module-null-sink",
                    "sink_name=" + name,
                    "sink_properties=device.description=" + name,
                ],
                text=True,
            ).strip()
            self.sink_modules.append(module)
        deadline = time.time() + 10
        while time.time() < deadline:
            sinks = subprocess.check_output(["pactl", "list", "short", "sinks"], text=True)
            if SINK_IN in sinks and SINK_OUT in sinks:
                return
            time.sleep(0.2)
        raise RuntimeError("null sinks did not appear")

    def start_stack(self):
        self.compose(
            ["up", "-d", "--build", "--wait", "--wait-timeout", "600"],
            timeout=900,
        )
        self.follow = subprocess.Popen(
            [
                "docker",
                "compose",
                "-p",
                PROJECT,
                "-f",
                "docker-compose.yml",
                "-f",
                "scripts/prototype-run/compose.override.yml",
                "logs",
                "-f",
                "--no-log-prefix",
                "assistant",
            ],
            cwd=REPO_ROOT,
            env=self.env,
            stdout=open(OUT_DIR / "assistant.log", "w"),
            stderr=subprocess.STDOUT,
        )
        # Give the follower a moment to replay startup lines.
        time.sleep(1.0)

    def stop(self):
        if self.follow is not None:
            self.follow.terminate()
            try:
                self.follow.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.follow.kill()
            self.follow = None
        if not self.args.keep:
            self.compose(["down", "-v", "--remove-orphans"], check=False, timeout=180)
        for module in self.sink_modules:
            subprocess.run(["pactl", "unload-module", module], check=False)
        self.sink_modules = []

    # -- synthesis and playback -------------------------------------------

    def synthesize(self):
        phrases = {"okay nabu"}
        for scenario in SCENARIOS:
            phrases.add(scenario["say"])
            if scenario.get("barge_say"):
                phrases.add(scenario["barge_say"])
        pcm_dir = OUT_DIR / "pcm"
        pcm_dir.mkdir(parents=True, exist_ok=True)
        formats = {}
        for phrase in sorted(phrases):
            path = pcm_dir / (slug(phrase) + ".pcm")
            result = subprocess.run(
                [
                    str(OUT_DIR / "latency-say"),
                    "-addr",
                    "127.0.0.1:%d" % TTS_PORT,
                    "-text",
                    phrase,
                    "-out",
                    str(path),
                ],
                capture_output=True,
                text=True,
                check=True,
            )
            match = re.search(r"(\d+)Hz (\d+)ch (\d+)bit", result.stdout)
            if not match:
                raise RuntimeError("latency-say printed an unexpected format: %r" % result.stdout)
            formats[phrase] = (int(match.group(1)), int(match.group(2)), int(match.group(3)))
            print("synthesized %-45r %6d bytes  %s" % (phrase, path.stat().st_size, result.stdout.strip()))
        rates = {value[:2] for value in formats.values()}
        if rates != {(TTS_RATE, 1)}:
            raise RuntimeError("unexpected TTS formats: %r" % formats)
        return formats

    def silence(self, seconds):
        return b"\x00" * int(TTS_RATE * 2 * 1 * seconds)

    def play(self, say):
        pcm_dir = OUT_DIR / "pcm"
        wake = (pcm_dir / (slug("okay nabu") + ".pcm")).read_bytes()
        query = (pcm_dir / (slug(say) + ".pcm")).read_bytes()
        # 1.6 s after the wake phrase lets the chime drain before the question,
        # and 1.5 s afterwards gives the endpointer its 500 ms window.
        data = wake + self.silence(1.6) + query + self.silence(1.5)
        subprocess.run(
            [
                "pacat",
                "--device=" + SINK_IN,
                "--rate=%d" % TTS_RATE,
                "--channels=1",
                "--format=s16le",
                "--raw",
            ],
            input=data,
            check=True,
        )

    def start_capture(self, path):
        path.parent.mkdir(parents=True, exist_ok=True)
        return subprocess.Popen(
            [
                "parec",
                "--device=" + SINK_OUT + ".monitor",
                "--rate=%d" % CAPTURE_RATE,
                "--channels=1",
                "--format=s16le",
                "--raw",
            ],
            stdout=open(path, "wb"),
            stderr=subprocess.DEVNULL,
        )

    def stop_capture(self, proc):
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()

    # -- one scenario ------------------------------------------------------

    def run_scenario(self, scenario):
        scenario_id = scenario["id"]
        steps = scenario["expect"]["steps"]
        print("\n== %s: %r" % (scenario_id, scenario["say"]))
        pre = scenario.get("pre")
        if pre == "stop_embed":
            self.compose(["stop", "embed"], check=True, timeout=60)
            print("   embed stopped")

        capture_path = OUT_DIR / "replies" / (scenario_id + ".pcm")
        self.tail.poll()
        before_requests = len(parse_request_lines(self.tail.lines))
        before_barge = self.tail.count("voice: barge-in")
        capture = self.start_capture(capture_path)
        error = ""
        try:
            time.sleep(0.2)  # let parec connect
            self.play(scenario["say"])
            if scenario.get("barge_say"):
                # Land the wake word in the middle of the slow Agent reply.
                time.sleep(3.0)
                print("   barging in with %r" % scenario["barge_say"])
                self.play(scenario["barge_say"])
            timeout = scenario.get("wait_timeout", 40)
            self.tail.wait(
                lambda: len(parse_request_lines(self.tail.lines)) >= before_requests + len(steps),
                timeout,
            )
            if scenario.get("wait_for_log"):
                seen = self.tail.wait(
                    lambda: self.tail.count(scenario["wait_for_log"]) > 0,
                    scenario.get("wait_timeout", 30),
                )
                if seen:
                    # The alert is logged before it is spoken; let it drain into
                    # the capture so the audio evidence is not truncated.
                    time.sleep(2.0)
        except Exception as exc:  # keep the run going; record the break
            error = "%s: %s" % (type(exc).__name__, exc)
            print("   error: " + error)
        finally:
            self.stop_capture(capture)

        self.tail.poll()
        after_requests = parse_request_lines(self.tail.lines)
        records = after_requests[before_requests:]
        barge_in = self.tail.count("voice: barge-in") > before_barge
        if error:
            verdict = {"verdict": "fail", "reason": error, "observed": _observed(records)}
        else:
            verdict = classify(scenario["expect"], records, {"barge_in": barge_in})

        verify = self.verify(scenario) if not error else []
        if verify:
            verdict = {
                "verdict": "fail",
                "reason": "; ".join(verify),
                "observed": verdict.get("observed", ""),
            }

        if scenario.get("post") == "start_embed":
            self.compose(["start", "embed"], check=True, timeout=60)
            self.wait_healthy("embed", timeout=180)
            print("   embed healthy again")

        stats = pcm_stats(capture_path)
        responses = [record.get("response_ms") for record in records]
        response_ms = responses[-1] if responses else None
        barge_stop_ms = None
        if barge_in:
            for line in self.tail.lines:
                match = re.search(r'"msg":"voice: barge-in stopped","stop_ms":(\d+)', line)
                if match:
                    barge_stop_ms = int(match.group(1))
        result = {
            "id": scenario_id,
            "say": scenario["say"],
            "note": scenario.get("note", ""),
            "verdict": verdict["verdict"],
            "reason": verdict["reason"],
            "observed": verdict.get("observed", ""),
            "transcript": records[-1].get("transcript") if records else None,
            "response_ms": response_ms,
            "responses_ms": responses,
            "barge_stop_ms": barge_stop_ms,
            "budget": budget_verdict(records[-1].get("path") if records else None, response_ms),
            "reply_audio_seconds": stats["loud_seconds"],
            "reply_peak_dbfs": stats["peak_dbfs"],
            "requests": records,
        }
        self.results.append(result)
        print(
            "   %s  %s  response=%s  audio=%.2fs"
            % (verdict["verdict"].upper(), verdict.get("observed", ""), response_ms, stats["loud_seconds"])
        )
        if verdict["verdict"] != "pass":
            print("   reason: " + verdict["reason"])
        return result

    def verify(self, scenario):
        """Check the test double's view for the Agent path scenarios."""
        self.fake_records = self.load_fake_records()
        problems = []
        for check in scenario.get("verify") or []:
            kind = check.get("kind")
            if kind == "prefix_stripped":
                record = find_fake_request(self.fake_records, check["contains"])
                if record is None:
                    problems.append("test double never saw an utterance containing %r" % check["contains"])
                else:
                    last = str(record["messages"][-1].get("content", ""))
                    if not prefix_removed(last):
                        problems.append(
                            "Agent request prefix was not stripped before the model call: %r" % last
                        )
            elif kind == "history":
                record = find_fake_request(self.fake_records, check["contains"])
                if record is None:
                    problems.append("test double never saw %r" % check["contains"])
                else:
                    if not has_prior_user_turn(record, check["prior"]):
                        problems.append("prior user turn %r was missing from the request" % check["prior"])
                    if not has_prior_assistant_reply(record, check["reply"]):
                        problems.append("prior assistant reply %r was missing" % check["reply"])
            elif kind == "tool_result":
                if not any(has_tool_result(record, check["contains"]) for record in self.fake_records):
                    problems.append("no Tool result reached the test double")
            else:
                problems.append("unknown verify kind %r" % kind)
        return problems

    def load_fake_records(self):
        path = OUT_DIR / "fake-requests.jsonl"
        records = []
        if not path.exists():
            return records
        for line in path.read_text(encoding="utf-8").splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                records.append(json.loads(line))
            except ValueError:
                pass
        return records

    # -- the whole run -----------------------------------------------------

    def execute(self):
        OUT_DIR.mkdir(parents=True, exist_ok=True)
        errors = scenario_shape_errors(SCENARIOS)
        if errors:
            raise SystemExit("scenario shape errors:\n  " + "\n  ".join(errors))
        self.compose(["down", "-v", "--remove-orphans"], check=False, timeout=120)
        (OUT_DIR / "fake-requests.jsonl").unlink(missing_ok=True)
        self.generate_config()
        self.build_binaries()
        self.start_sinks()
        try:
            if not self.args.skip_mic:
                self.passive_mic_check()
            self.start_stack()
            self.synthesize()
            selected = [s for s in SCENARIOS if not self.args.only or s["id"] in self.args.only]
            for scenario in selected:
                time.sleep(0.3)  # let the voice loop reopen its wake stream
                self.run_scenario(scenario)
        finally:
            self.write_report()
            self.stop()
        self.print_report()

    def passive_mic_check(self):
        info = subprocess.check_output(["pactl", "info"], text=True)
        match = re.search(r"Default Source: (\S+)", info)
        device = match.group(1) if match else ""
        if not device or "monitor" in device:
            rows = subprocess.check_output(["pactl", "list", "short", "sources"], text=True).splitlines()
            names = [row.split("\t")[1] for row in rows if "\t" in row and "monitor" not in row]
            device = names[0] if names else ""
        if not device:
            text = "no non-monitor input source found\n"
        else:
            try:
                text = measure_mic(device)
            except Exception as exc:
                text = "device=%s\nprobe failed: %s: %s\n" % (device, type(exc).__name__, exc)
        (OUT_DIR / "mic-noise.txt").write_text(text, encoding="utf-8")
        print("passive mic check:\n" + text)

    def write_report(self):
        self.tail.poll()
        records = parse_request_lines(self.tail.lines)
        latency_path = OUT_DIR / "latency-report.txt"
        report = subprocess.run(
            ["go", "run", "./cmd/latency-report", "-file", str(OUT_DIR / "assistant.log")],
            cwd=ASSISTANT_DIR,
            capture_output=True,
            text=True,
        )
        latency_path.write_text(report.stdout + report.stderr, encoding="utf-8")
        (OUT_DIR / "requests.jsonl").write_text(
            "".join(json.dumps(record) + "\n" for record in records), encoding="utf-8"
        )
        # The short log: request lines plus the voice/timer/agent lifecycle
        # events that are not request lines (Barge-in, Timer alerts).
        evidence = []
        for line in self.tail.lines:
            if any(marker in line for marker in ('"msg":"request"', '"msg":"voice:', '"msg":"timer', '"msg":"agent')):
                evidence.append(line)
        (OUT_DIR / "assistant-evidence.log").write_text("\n".join(evidence) + "\n", encoding="utf-8")
        (OUT_DIR / "scenario-results.json").write_text(
            json.dumps(self.results, indent=2) + "\n", encoding="utf-8"
        )
        (OUT_DIR / "scenario-results.md").write_text(self.results_markdown(), encoding="utf-8")

    def results_markdown(self):
        lines = [
            "| Scenario | Said (synthesized) | Observed route | STT transcript | Response | #12 budget | Reply audio | Verdict |",
            "| --- | --- | --- | --- | ---: | --- | ---: | --- |",
        ]
        for result in self.results:
            measured = [value for value in result.get("responses_ms", []) if value is not None]
            response = "/".join(str(value) for value in measured) + " ms" if measured else "-"
            transcript = (result["transcript"] or "").replace("|", "\\|")
            lines.append(
                "| %s | %s | %s | %s | %s | %s | %.2f s | %s |"
                % (
                    result["id"],
                    result["say"],
                    result.get("observed", "").replace("|", "\\|"),
                    transcript,
                    response,
                    result["budget"],
                    result["reply_audio_seconds"],
                    result["verdict"].upper(),
                )
            )
        return "\n".join(lines) + "\n"

    def print_report(self):
        print("\n===== scenario results =====")
        print(self.results_markdown())
        print((OUT_DIR / "latency-report.txt").read_text(encoding="utf-8"))


def publish(directory):
    target = pathlib.Path(directory)
    target.mkdir(parents=True, exist_ok=True)
    for name in (
        "scenario-results.json",
        "scenario-results.md",
        "latency-report.txt",
        "requests.jsonl",
        "assistant-evidence.log",
        "fake-requests.jsonl",
        "mic-noise.txt",
    ):
        source = OUT_DIR / name
        if source.exists():
            shutil.copy2(source, target / name)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--tag", default="e2e29", help="image tag for this run")
    parser.add_argument("--only", default="", help="comma-separated scenario ids")
    parser.add_argument("--keep", action="store_true", help="leave the stack running")
    parser.add_argument("--skip-mic", action="store_true", help="skip the passive mic probe")
    parser.add_argument("--publish", default="", help="copy git-worthy evidence to this directory")
    args = parser.parse_args(argv)
    args.only = {part.strip() for part in args.only.split(",") if part.strip()}
    run = Run(args)
    try:
        run.execute()
    except KeyboardInterrupt:
        run.stop()
        raise
    if args.publish:
        publish(args.publish)
        print("published evidence to " + args.publish)
    return 0 if all(result["verdict"] == "pass" for result in run.results) else 1


if __name__ == "__main__":
    sys.exit(main())
