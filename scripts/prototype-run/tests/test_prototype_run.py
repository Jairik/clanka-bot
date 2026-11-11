"""Seam tests for the #29 prototype-run harness.

The harness itself is shelling out to Docker and PulseAudio; these tests only
cover the pure seams: parsing assistant logs, deciding a scenario verdict,
mapping Response latency onto the #12 budgets, and the fake provider's
request-plan. That is where a mistake would silently turn a broken scenario
into a pass.

Run from the repo root:

    python3 -m unittest discover -s scripts/prototype-run/tests -v
"""

import json
import pathlib
import sys
import unittest

HARNESS_DIR = pathlib.Path(__file__).resolve().parents[1]
sys.path.insert(0, str(HARNESS_DIR))

import fake_provider  # noqa: E402
import prototype_run  # noqa: E402


def request(**fields):
    """One assistant `request` log record with sensible defaults."""
    record = {
        "msg": "request",
        "source": "voice",
        "path": "fast_path",
        "tool": "",
        "outcome": "ok",
        "response_ms": 800,
        "stages": {"endpointing_ms": 512},
    }
    record.update(fields)
    return record


class ParseRequestLinesTest(unittest.TestCase):
    def test_keeps_only_request_lines_and_ignores_prefixes(self):
        lines = [
            '{"level":"INFO","msg":"assistant started"}',
            'assistant-1  | {"level":"INFO","msg":"request","path":"fast_path","outcome":"ok"}',
            '{"level":"INFO","msg":"voice: transcript","text":"what time is it"}',
            '{"level":"INFO","msg":"request","path":"agent_path","outcome":"failed"}',
            "not json at all",
        ]
        records = prototype_run.parse_request_lines(lines)
        self.assertEqual([r["path"] for r in records], ["fast_path", "agent_path"])

    def test_ignores_invalid_json_that_looks_like_a_request(self):
        records = prototype_run.parse_request_lines(['{"msg":"request",'])
        self.assertEqual(records, [])


class BudgetTest(unittest.TestCase):
    def test_budget_per_path_matches_issue_12(self):
        self.assertEqual(prototype_run.budget_for_path("fast_path"), (1200, 2000))
        self.assertEqual(prototype_run.budget_for_path("control_phrase"), (1200, 2000))
        self.assertEqual(prototype_run.budget_for_path("confirmation"), (1200, 2000))
        self.assertEqual(prototype_run.budget_for_path("agent_path"), (2500, 4000))

    def test_unknown_path_has_no_budget(self):
        self.assertIsNone(prototype_run.budget_for_path("fallthrough"))

    def test_within_budget_uses_the_p50_and_p95_budgets(self):
        self.assertEqual(prototype_run.budget_verdict("fast_path", 800), "within p50")
        self.assertEqual(prototype_run.budget_verdict("fast_path", 1500), "p50-p95")
        self.assertEqual(prototype_run.budget_verdict("fast_path", 2500), "OVER p95")
        self.assertEqual(prototype_run.budget_verdict("agent_path", 3000), "p50-p95")
        self.assertEqual(prototype_run.budget_verdict("agent_path", 5000), "OVER p95")
        self.assertEqual(prototype_run.budget_verdict("fallthrough", 500), "no budget")

    def test_missing_response_is_not_zero_latency(self):
        self.assertEqual(prototype_run.budget_verdict("fast_path", None), "not measured")


class ClassifyTest(unittest.TestCase):
    def test_simple_expected_route_passes(self):
        result = prototype_run.classify(
            {"steps": [{"path": "control_phrase", "tool": "time.now", "outcome": "ok"}]},
            [request(path="control_phrase", tool="time.now")],
        )
        self.assertEqual(result["verdict"], "pass")

    def test_wrong_tool_fails_with_both_names(self):
        result = prototype_run.classify(
            {"steps": [{"path": "fast_path", "tool": "weather.current"}]},
            [request(path="fast_path", tool="date.today")],
        )
        self.assertEqual(result["verdict"], "fail")
        self.assertIn("weather.current", result["reason"])
        self.assertIn("date.today", result["reason"])

    def test_missing_request_fails(self):
        result = prototype_run.classify(
            {"steps": [{"path": "fast_path", "tool": "date.today"}]}, []
        )
        self.assertEqual(result["verdict"], "fail")
        self.assertIn("no request", result["reason"])

    def test_extra_request_fails(self):
        result = prototype_run.classify(
            {"steps": [{"path": "control_phrase", "tool": "time.now"}]},
            [
                request(path="control_phrase", tool="time.now"),
                request(path="agent_path", tool=""),
            ],
        )
        self.assertEqual(result["verdict"], "fail")
        self.assertIn("saw 2", result["reason"])

    def test_spoken_failure_is_a_pass_when_expected(self):
        result = prototype_run.classify(
            {
                "steps": [
                    {"path": "agent_path", "outcome": "failed", "agent_error": "unavailable"}
                ]
            },
            [
                request(
                    path="agent_path",
                    outcome="failed",
                    agent_error="unavailable",
                    response_ms=450,
                )
            ],
        )
        self.assertEqual(result["verdict"], "pass")

    def test_fallthrough_reason_must_match_when_expected(self):
        expected = {
            "steps": [
                {
                    "path": "agent_path",
                    "outcome": "ok",
                    "fallthrough_reason": "embed_failed",
                }
            ]
        }
        passing = prototype_run.classify(
            expected, [request(path="agent_path", fallthrough_reason="embed_failed")]
        )
        failing = prototype_run.classify(
            expected, [request(path="agent_path", fallthrough_reason="no_match")]
        )
        self.assertEqual(passing["verdict"], "pass")
        self.assertEqual(failing["verdict"], "fail")
        self.assertIn("embed_failed", failing["reason"])

    def test_barge_in_requires_the_log_line_and_two_steps(self):
        expected = {
            "barge_in": True,
            "steps": [
                {"path": "agent_path", "outcome": "cancelled"},
                {"path": "control_phrase", "tool": "time.now"},
            ],
        }
        records = [
            request(path="agent_path", outcome="cancelled", response_ms=2100),
            request(path="control_phrase", tool="time.now"),
        ]
        passing = prototype_run.classify(expected, records, {"barge_in": True})
        missing_log = prototype_run.classify(expected, records, {"barge_in": False})
        self.assertEqual(passing["verdict"], "pass")
        self.assertEqual(missing_log["verdict"], "fail")
        self.assertIn("barge-in", missing_log["reason"])

    def test_transcript_is_reported_even_when_routing_is_unexpected(self):
        result = prototype_run.classify(
            {"steps": [{"path": "fast_path", "tool": "timer.list"}]},
            [request(path="agent_path", transcript="how long is left on my timer")],
        )
        self.assertEqual(result["verdict"], "fail")
        self.assertIn("how long is left on my timer", result["observed"])


class PrefixTest(unittest.TestCase):
    def test_a_clean_rest_means_the_prefix_was_removed(self):
        self.assertTrue(prototype_run.prefix_removed("what is the capital of France"))
        self.assertTrue(prototype_run.prefix_removed("ask the agent for the time"))

    def test_a_punctuated_or_bare_prefix_is_still_present(self):
        # STT punctuates "Agent." even though the prefix table says "agent".
        self.assertFalse(prototype_run.prefix_removed("Agent. What is the capital of France?"))
        self.assertFalse(prototype_run.prefix_removed("agent what time is it"))
        self.assertFalse(prototype_run.prefix_removed("agent"))

    def test_the_word_boundary_is_respected(self):
        self.assertTrue(prototype_run.prefix_removed("agentic workflows are interesting"))


class FakeLogQueryTest(unittest.TestCase):
    def records(self):
        return [
            {
                "messages": [
                    {"role": "system", "content": "test"},
                    {"role": "user", "content": "what is the capital of France"},
                ]
            },
            {
                "messages": [
                    {"role": "system", "content": "test"},
                    {"role": "user", "content": "what is the capital of France"},
                    {"role": "assistant", "content": "The capital of France is Paris."},
                    {"role": "user", "content": "what did I ask before"},
                ]
            },
            {
                "messages": [
                    {"role": "user", "content": "what time is it"},
                    {
                        "role": "assistant",
                        "content": "",
                        "tool_calls": [{"id": "call_1"}],
                    },
                    {"role": "tool", "content": '{"time":"2026-11-11T15:04:00Z"}'},
                ]
            },
        ]

    def test_find_by_last_user_message(self):
        found = prototype_run.find_fake_request(self.records(), "what did i ask")
        self.assertIsNotNone(found)
        self.assertIn("capital of France", str(found))

    def test_prior_user_turn_and_assistant_reply(self):
        record = prototype_run.find_fake_request(self.records(), "what did i ask")
        self.assertTrue(prototype_run.has_prior_user_turn(record, "capital of france"))
        self.assertTrue(prototype_run.has_prior_assistant_reply(record, "Paris"))
        self.assertFalse(prototype_run.has_prior_user_turn(record, "unrelated"))

    def test_tool_result_present(self):
        records = self.records()
        self.assertTrue(any(prototype_run.has_tool_result(record, "2026-11-11") for record in records))
        self.assertFalse(any(prototype_run.has_tool_result(record, "nothing here") for record in records))


class FakeProviderPlanTest(unittest.TestCase):
    def user(self, text):
        return [{"role": "system", "content": "test"}, {"role": "user", "content": text}]

    def test_provider_failure_request_returns_401(self):
        plan = fake_provider.plan(self.user("agent trigger a provider failure"), [])
        self.assertEqual(plan["action"], "fail")
        self.assertEqual(plan["status"], 401)

    def test_time_question_asks_for_the_time_tool(self):
        plan = fake_provider.plan(
            self.user("what time is it"), ["time__now", "date__today"]
        )
        self.assertEqual(plan["action"], "tool_call")
        self.assertEqual(plan["name"], "time__now")

    def test_no_tool_call_when_time_tool_is_not_offered(self):
        plan = fake_provider.plan(self.user("what time is it"), [])
        self.assertEqual(plan["action"], "content")

    def test_timer_test_asks_for_the_timer_tool(self):
        plan = fake_provider.plan(
            self.user("Agent. Please run a timer test for five minutes."),
            ["timer__set", "time__now"],
        )
        self.assertEqual(plan["action"], "tool_call")
        self.assertEqual(plan["name"], "timer__set")
        self.assertEqual(json.loads(plan["arguments"])["duration_seconds"], 300)

    def test_timer_test_can_ask_for_ten_seconds(self):
        plan = fake_provider.plan(
            self.user("Agent. Please run a timer test for ten seconds."),
            ["timer__set"],
        )
        self.assertEqual(json.loads(plan["arguments"])["duration_seconds"], 10)

    def test_a_plain_timer_request_does_not_call_a_tool(self):
        # The Fast path miss scenarios must not set a timer through the double.
        plan = fake_provider.plan(
            self.user("Set a timer for ten minutes."), ["timer__set"]
        )
        self.assertEqual(plan["action"], "content")

    def test_tool_result_turn_answers_with_content(self):
        messages = [
            {"role": "user", "content": "what time is it"},
            {"role": "assistant", "content": ""},
            {"role": "tool", "tool_call_id": "call_1", "content": '{"time":"2026-11-11T15:04:00Z"}'},
        ]
        plan = fake_provider.plan(messages, ["time__now"])
        self.assertEqual(plan["action"], "content")
        self.assertIn("2026-11-11", plan["text"])

    def test_long_story_is_slow(self):
        plan = fake_provider.plan(self.user("tell me a long story"), [])
        self.assertEqual(plan["action"], "slow")

    def test_history_question_counts_prior_user_turns(self):
        messages = [
            {"role": "system", "content": "test"},
            {"role": "user", "content": "what is the capital of France"},
            {"role": "assistant", "content": "The capital of France is Paris."},
            {"role": "user", "content": "what did I ask before"},
        ]
        plan = fake_provider.plan(messages, [])
        self.assertEqual(plan["action"], "content")
        self.assertIn("asked 1", plan["text"])

    def test_default_reply_is_labelled_as_a_test_double(self):
        plan = fake_provider.plan(self.user("hello there"), [])
        self.assertEqual(plan["action"], "content")
        self.assertIn("test double", plan["text"].lower())


class ScenarioShapeTest(unittest.TestCase):
    def test_checked_in_scenarios_are_complete(self):
        self.assertEqual(prototype_run.scenario_shape_errors(prototype_run.SCENARIOS), [])

    def test_shape_rejects_a_scenario_without_an_expectation(self):
        broken = [{"id": "x", "say": "hello"}]
        self.assertTrue(prototype_run.scenario_shape_errors(broken))


if __name__ == "__main__":
    unittest.main()
