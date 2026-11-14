// The chat's formatters and the pending-request ids.
import { describe, expect, it } from "vitest";
import { createRequestId, fmtClock, fmtMS, parseMS, totalMS } from "../chat";
import { req } from "./harness";

describe("timing formatters", () => {
  it("parses the server's millisecond and second strings", () => {
    expect(parseMS("840 ms")).toBe(840);
    expect(parseMS("1.2 s")).toBeCloseTo(1200);
    expect(parseMS(" 12.5 ms ")).toBe(12.5);
    expect(parseMS("no idea")).toBe(0);
    expect(parseMS("")).toBe(0);
  });

  it("formats milliseconds with the same units the server uses", () => {
    expect(fmtMS(0)).toBe("0 ms");
    expect(fmtMS(840)).toBe("840 ms");
    expect(fmtMS(999)).toBe("999 ms");
    expect(fmtMS(1000)).toBe("1.0 s");
    expect(fmtMS(1234)).toBe("1.2 s");
  });

  it("sums every stage of a request", () => {
    const r = req({
      ID: "r",
      Stages: [
        { Name: "stt", Label: "STT", MS: "400 ms" },
        { Name: "agent", Label: "Agent", MS: "1.1 s" },
      ],
    });
    expect(totalMS(r)).toBe(1500);
    expect(totalMS(req({ ID: "none", Stages: null }))).toBe(0);
  });

  it("renders a clock time from an ISO string or a Date", () => {
    const d = new Date("2026-11-13T14:05:00");
    const expected = d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
    expect(fmtClock(d)).toBe(expected);
    expect(fmtClock(d.toISOString())).toBe(expected);
  });
});

describe("pending-request ids", () => {
  it("stays unique when two sends land in the same millisecond", () => {
    const next = createRequestId(() => 1_700_000_000_000);
    const ids = Array.from({ length: 50 }, next);
    expect(new Set(ids).size).toBe(ids.length);
  });

  it("carries the timestamp so ids stay distinct across milliseconds", () => {
    let t = 1;
    const next = createRequestId(() => t++);
    expect(next()).toBe("p1-1");
    expect(next()).toBe("p2-2");
  });
});
