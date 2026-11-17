// The "Reconnecting to the Pi…" strip: absent while connected, shown once after
// the grace window when the stream drops or a request fails at the network
// layer, and gone again on the next open (#301).
import { act, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { getJSON } from "../api";
import { LiveProvider, liveBackoff, piUnreachableAfter, resetPiStatus } from "../events";
import { Shell } from "../shell";
import type { HealthView } from "../types";
import { json, stubFetch } from "./harness";

/** A stand-in for EventSource: the test drives open/error by hand. */
class FakeEventSource {
  static all: FakeEventSource[] = [];
  static last() {
    return FakeEventSource.all[FakeEventSource.all.length - 1];
  }
  onopen: ((event: Event) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  closed = false;
  constructor(public url: string) {
    FakeEventSource.all.push(this);
  }
  addEventListener() {}
  removeEventListener() {}
  close() {
    this.closed = true;
  }
  open() {
    this.onopen?.(new Event("open"));
  }
  fail() {
    this.onerror?.(new Event("error"));
  }
}

const health: HealthView = {
  Owner: "owner@example.com",
  Page: "health",
  CheckedAt: "now",
  HealthTimeout: "3s",
  Services: [],
  Host: [],
  Wakeword: [],
  Location: { Name: "pi", Status: "ok", StatusLabel: "OK", Detail: "" },
  Modules: [],
  Spotify: [],
  Routing: { Name: "Semantic routing", Status: "not-configured", StatusLabel: "not configured", Detail: "" },
  FalseWakes: null,
  DiagnosticsURL: "/api/ui/diagnostics",
};

const mount = () => render(<LiveProvider><Shell><div data-testid="page">page</div></Shell></LiveProvider>);
const settle = () => act(async () => {});

beforeEach(() => {
  FakeEventSource.all = [];
  vi.stubGlobal("EventSource", FakeEventSource);
  resetPiStatus();
  vi.useFakeTimers();
  // Local-time constructor so getHours() is 12 in any test-runner timezone.
  vi.setSystemTime(new Date(2026, 10, 16, 12, 4, 0));
  Object.defineProperty(document, "visibilityState", { configurable: true, get: () => "visible" });
  stubFetch({ "/api/ui/health": () => json(health) });
});

describe("Pi unreachable strip", () => {
  it("stays out of the way while the stream is connected", async () => {
    mount();
    act(() => FakeEventSource.last().open());
    await settle();

    // Well past the grace window with the stream still up.
    act(() => vi.advanceTimersByTime(piUnreachableAfter + 30_000));
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("appears after the grace window with the last update time", async () => {
    mount();
    act(() => FakeEventSource.last().open()); // stamps 12:04 as the last update
    await settle();
    act(() => FakeEventSource.last().fail());

    act(() => vi.advanceTimersByTime(piUnreachableAfter - 1));
    expect(screen.queryByRole("status")).toBeNull();
    act(() => vi.advanceTimersByTime(1));

    const strip = screen.getByRole("status");
    expect(strip.textContent).toContain("Reconnecting to the Pi… (last update 12:04)");
    // It is an overlay, not a row in the page's (or composer's) layout flow.
    expect(strip.closest("main")).toBeNull();
  });

  it("clears on the next successful open", async () => {
    mount();
    act(() => FakeEventSource.last().open());
    await settle();
    act(() => FakeEventSource.last().fail());
    act(() => vi.advanceTimersByTime(piUnreachableAfter));
    expect(screen.getByRole("status")).toBeTruthy();

    // The backoff fires and a new stream opens — the generation bump we clear on.
    act(() => vi.advanceTimersByTime(liveBackoff(0)));
    act(() => FakeEventSource.last().open());
    await settle();
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("starts the same grace window when a request fails with a network error", async () => {
    mount();
    act(() => FakeEventSource.last().open());
    await settle();
    expect(screen.queryByRole("status")).toBeNull();

    globalThis.fetch = vi.fn(() => Promise.reject(new TypeError("Failed to fetch"))) as typeof fetch;
    await act(async () => {
      await getJSON("/api/ui/health").catch(() => {});
    });
    act(() => vi.advanceTimersByTime(piUnreachableAfter));
    expect(screen.getByRole("status").textContent).toContain("Reconnecting to the Pi… (last update 12:04)");
  });
});
