// The Activity filter and hour-bucket rules, without a DOM.
import { describe, expect, it } from "vitest";
import { activityQuery, asFilter, bucketActivity, filterActivity, isProblem, isTypedSource, mergeActivity } from "../activity";
import { liveBackoff } from "../events";
import { req } from "./harness";

const voice = req({ ID: "v", Source: "voice" });
const typed = req({ ID: "t", Source: "dashboard" });
const mic = req({ ID: "m", Source: "dashboard_mic" });
const failed = req({ ID: "f", Source: "voice", Outcome: "failed", Error: "boom" });
const cancelled = req({ ID: "c", Source: "voice", Outcome: "cancelled" });

describe("activity filters", () => {
  it("keeps every request for All", () => {
    expect(filterActivity([voice, typed, mic], "all", "").map((r) => r.ID)).toEqual(["v", "t", "m"]);
  });

  it("puts only keyboard requests under Typed", () => {
    expect(filterActivity([voice, typed, mic], "typed", "").map((r) => r.ID)).toEqual(["t"]);
  });

  it("keeps Voice for the Pi and Mic for the browser", () => {
    expect(filterActivity([voice, typed, mic], "voice", "").map((r) => r.ID)).toEqual(["v"]);
    expect(filterActivity([voice, typed, mic], "mic", "").map((r) => r.ID)).toEqual(["m"]);
  });

  it("counts a failure, a cancel and an error as problems", () => {
    expect([failed, cancelled, req({ ID: "e", Error: "nope" }), voice].filter(isProblem).map((r) => r.ID)).toEqual(["f", "c", "e"]);
    expect(filterActivity([voice, failed], "problems", "").map((r) => r.ID)).toEqual(["f"]);
  });

  it("searches transcripts, replies, tools, paths and errors", () => {
    const rows = [req({ ID: "a", Transcript: "weather tomorrow" }), req({ ID: "b", Reply: "It will rain" }), req({ ID: "c", Error: "timeout" })];
    expect(filterActivity(rows, "all", "WEATHER").map((r) => r.ID)).toEqual(["a"]);
    expect(filterActivity(rows, "all", "rain").map((r) => r.ID)).toEqual(["b"]);
    expect(filterActivity(rows, "all", "timeout").map((r) => r.ID)).toEqual(["c"]);
    expect(filterActivity(rows, "all", "  ").map((r) => r.ID)).toEqual(["a", "b", "c"]);
  });

  it("searches the raw path and call names the server also matches", () => {
    const rows = [
      req({ ID: "p", Path: "agent_path", PathLabel: "Agent path" }),
      req({ ID: "c", Path: "fast_path", PathLabel: "Other", Calls: [{ Name: "notes.search", LocalOnly: true, Result: "" }] }),
    ];
    expect(filterActivity(rows, "all", "agent_path").map((r) => r.ID)).toEqual(["p"]);
    expect(filterActivity(rows, "all", "agent path").map((r) => r.ID)).toEqual(["p"]);
    expect(filterActivity(rows, "all", "notes.search").map((r) => r.ID)).toEqual(["c"]);
  });

  it("falls back to All for an unknown filter", () => {
    expect(asFilter("nonsense")).toBe("all");
    expect(filterActivity([voice, typed], "nonsense", "")).toHaveLength(2);
  });

  it("only treats the keyboard sources as typed", () => {
    expect(isTypedSource("dashboard")).toBe(true);
    expect(isTypedSource("debug")).toBe(true);
    expect(isTypedSource("dashboard_mic")).toBe(false);
    expect(isTypedSource("voice")).toBe(false);
  });
});

describe("activity buckets", () => {
  const now = new Date("2026-11-13T12:00:00");

  it("groups one hour and keeps input order", () => {
    const a = req({ ID: "a", TimeISO: "2026-11-13T10:05:00" });
    const b = req({ ID: "b", TimeISO: "2026-11-13T10:59:00" });
    const buckets = bucketActivity([a, b], now);
    expect(buckets).toHaveLength(1);
    expect(buckets[0].list.map((r) => r.ID)).toEqual(["a", "b"]);
  });

  it("keeps the same hour one week apart in different buckets", () => {
    const thisWeek = req({ ID: "now", TimeISO: "2026-11-13T09:00:00" });
    const lastWeek = req({ ID: "then", TimeISO: "2026-11-06T09:00:00" });
    const buckets = bucketActivity([thisWeek, lastWeek], now);
    expect(buckets).toHaveLength(2);
    expect(buckets[0].key).not.toBe(buckets[1].key);
    expect(buckets[0].key).toContain("2026");
  });

  it("labels today by hour and older days by date", () => {
    const today = bucketActivity([req({ ID: "a", TimeISO: "2026-11-13T09:00:00" })], now)[0];
    const older = bucketActivity([req({ ID: "b", TimeISO: "2026-11-01T09:00:00" })], now)[0];
    expect(today.title).not.toContain("·");
    expect(older.title).toContain("·");
  });
});

describe("activity history query", () => {
  it("omits defaults so the first page keeps its plain URL", () => {
    expect(activityQuery({ filter: "all", q: "  " })).toBe("/api/ui/activity");
  });

  it("carries the filter, the trimmed search and the cursor", () => {
    expect(activityQuery({ filter: "problems", q: " blue timer ", before: "c1" })).toBe(
      "/api/ui/activity?filter=problems&q=blue+timer&before=c1",
    );
  });

  it("falls back to All for an unknown filter", () => {
    expect(activityQuery({ filter: "nonsense", q: "x" })).toBe("/api/ui/activity?q=x");
  });
});

describe("live activity deltas", () => {
  it("prepends an event once and keeps the page bounded", () => {
    const a = req({ ID: "a" });
    const b = req({ ID: "b" });
    const next = mergeActivity([a, b], req({ ID: "c" }), 2);
    expect(next.map((r) => r.ID)).toEqual(["c", "a"]);
    // A delta for a request the POST already inserted must not duplicate it.
    const three = mergeActivity([a, b], req({ ID: "c" }), 3);
    expect(mergeActivity(three, req({ ID: "c" }), 3).map((r) => r.ID)).toEqual(["c", "a", "b"]);
  });
});

describe("live reconnect backoff", () => {
  it("doubles from one second and stops at thirty", () => {
    expect([0, 1, 2, 3, 4, 5, 6].map(liveBackoff)).toEqual([1000, 2000, 4000, 8000, 16000, 30000, 30000]);
  });
});
