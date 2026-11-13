// The one live stream: its backoff, its ping watchdog, its tab-visibility
// gating, and the snapshot/delta merge that closes the resync clobber race.
import { act, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { usePageData } from "../api";
import { ChatProvider, useChat } from "../chat";
import { LiveProvider, liveBackoff, liveStableAfter, liveStaleAfter, useLive } from "../events";
import type { HealthView } from "../types";
import { emptyActivity, json, req, stubFetch } from "./harness";

/** A stand-in for the browser's EventSource: the test drives open/error/frames. */
class FakeEventSource {
  static all: FakeEventSource[] = [];
  static last() {
    return FakeEventSource.all[FakeEventSource.all.length - 1];
  }

  closed = false;
  onopen: ((event: Event) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  private listeners = new Map<string, Set<(event: MessageEvent<string>) => void>>();

  constructor(public url: string) {
    FakeEventSource.all.push(this);
  }

  addEventListener(name: string, listener: (event: MessageEvent<string>) => void) {
    const set = this.listeners.get(name) ?? new Set();
    set.add(listener);
    this.listeners.set(name, set);
  }

  removeEventListener(name: string, listener: (event: MessageEvent<string>) => void) {
    this.listeners.get(name)?.delete(listener);
  }

  close() {
    this.closed = true;
  }

  open() {
    this.onopen?.(new Event("open"));
  }

  fail() {
    this.onerror?.(new Event("error"));
  }

  /** Deliver one named frame, the way the server writes it. */
  emit(name: string, data: unknown) {
    const event = new MessageEvent("message", { data: JSON.stringify(data) });
    this.listeners.get(name)?.forEach((listener) => listener(event));
  }
}

const useFakeStream = () => {
  FakeEventSource.all = [];
  vi.stubGlobal("EventSource", FakeEventSource);
};

const setVisibility = (state: "visible" | "hidden") => {
  Object.defineProperty(document, "visibilityState", { configurable: true, get: () => state });
};

function HealthProbe() {
  const { data } = usePageData("health");
  return <output data-testid="health">{(data as HealthView | null)?.CheckedAt ?? "none"}</output>;
}

function ActivityProbe() {
  const { requests } = useChat();
  return <output data-testid="requests">{requests.map((r) => r.ID).join(",")}</output>;
}

/** A route whose responses the test answers by hand. */
function heldRoute() {
  const pending: Array<(res: Response) => void> = [];
  return {
    pending,
    route: () => new Promise<Response>((done) => pending.push(done)),
  };
}

describe("reconnect backoff", () => {
  it("resets the delay only after a stream stayed up", () => {
    vi.useFakeTimers();
    useFakeStream();
    render(
      <LiveProvider>
        <div />
      </LiveProvider>,
    );
    const sources = FakeEventSource.all;
    expect(sources).toHaveLength(1);

    // Accepts then drops: the counter keeps doubling instead of restarting.
    act(() => sources[0].open());
    act(() => sources[0].fail());
    act(() => vi.advanceTimersByTime(liveBackoff(0)));
    expect(sources).toHaveLength(2);
    act(() => sources[1].open());
    act(() => sources[1].fail());
    act(() => vi.advanceTimersByTime(liveBackoff(1) - 1));
    expect(sources).toHaveLength(2);
    act(() => vi.advanceTimersByTime(1));
    expect(sources).toHaveLength(3);

    // A stream that stayed up past the stable window resets the counter, so the
    // next drop retries in one second rather than after another doubling.
    act(() => sources[2].open());
    act(() => vi.advanceTimersByTime(liveStableAfter));
    act(() => sources[2].fail());
    act(() => vi.advanceTimersByTime(liveBackoff(0)));
    expect(sources).toHaveLength(4);
  });
});

describe("staleness watchdog", () => {
  it("reconnects when the stream stops sending frames", () => {
    vi.useFakeTimers();
    useFakeStream();
    render(
      <LiveProvider>
        <div />
      </LiveProvider>,
    );
    const first = FakeEventSource.last();
    act(() => first.open());

    act(() => vi.advanceTimersByTime(liveStaleAfter - 1000));
    expect(first.closed).toBe(false);
    act(() => vi.advanceTimersByTime(1000));
    expect(first.closed).toBe(true);
    act(() => vi.advanceTimersByTime(liveBackoff(0)));
    expect(FakeEventSource.all).toHaveLength(2);
  });

  it("stays connected while pings arrive", () => {
    vi.useFakeTimers();
    useFakeStream();
    render(
      <LiveProvider>
        <div />
      </LiveProvider>,
    );
    const first = FakeEventSource.last();
    act(() => first.open());
    // Four idle ping intervals: well past the staleness deadline in total, but
    // never that long without a frame.
    for (let i = 0; i < 4; i += 1) {
      act(() => vi.advanceTimersByTime(15_000));
      act(() => first.emit("ping", {}));
    }
    expect(first.closed).toBe(false);
    expect(FakeEventSource.all).toHaveLength(1);
  });
});

describe("tab visibility", () => {
  it("closes the stream while hidden and reopens it when visible", () => {
    useFakeStream();
    setVisibility("visible");
    render(
      <LiveProvider>
        <div />
      </LiveProvider>,
    );
    const first = FakeEventSource.last();
    act(() => first.open());

    act(() => {
      setVisibility("hidden");
      document.dispatchEvent(new Event("visibilitychange"));
    });
    expect(first.closed).toBe(true);

    act(() => {
      setVisibility("visible");
      document.dispatchEvent(new Event("visibilitychange"));
    });
    expect(FakeEventSource.all).toHaveLength(2);
    expect(FakeEventSource.last()).not.toBe(first);
    setVisibility("visible");
  });
});

describe("snapshot resync", () => {
  it("keeps a health delta that arrived while the snapshot was in flight", async () => {
    useFakeStream();
    const health = heldRoute();
    stubFetch({ "/api/ui/health": health.route });
    render(
      <LiveProvider>
        <HealthProbe />
      </LiveProvider>,
    );

    await act(async () => health.pending[0](json({ Status: "ok", CheckedAt: "old" })));
    expect(screen.getByTestId("health").textContent).toBe("old");

    // The stream opening is a resync: the view reloads while deltas still flow.
    await act(async () => FakeEventSource.last().open());
    expect(health.pending).toHaveLength(2);
    await act(async () => FakeEventSource.last().emit("health", { Status: "error", CheckedAt: "delta" }));
    expect(screen.getByTestId("health").textContent).toBe("delta");

    // The snapshot carries the state from before the delta; the delta wins.
    await act(async () => health.pending[1](json({ Status: "ok", CheckedAt: "old" })));
    expect(screen.getByTestId("health").textContent).toBe("delta");
  });

  it("keeps a delta received before the snapshot from moving the view back", async () => {
    useFakeStream();
    const health = heldRoute();
    stubFetch({ "/api/ui/health": health.route });
    render(
      <LiveProvider>
        <HealthProbe />
      </LiveProvider>,
    );

    await act(async () => health.pending[0](json({ CheckedAt: "old" })));
    await act(async () => FakeEventSource.last().emit("health", { CheckedAt: "delta" }));
    await act(async () => FakeEventSource.last().open());
    await act(async () => health.pending[1](json({ CheckedAt: "snapshot" })));
    expect(screen.getByTestId("health").textContent).toBe("snapshot");
  });

  it("merges a completed request that arrived while the activity snapshot was in flight", async () => {
    useFakeStream();
    const activity = heldRoute();
    stubFetch({ "/api/ui/activity": activity.route });
    render(
      <LiveProvider>
        <ChatProvider>
          <ActivityProbe />
        </ChatProvider>
      </LiveProvider>,
    );

    await act(async () => activity.pending[0](json(emptyActivity())));
    expect(screen.getByTestId("requests").textContent).toBe("");

    await act(async () => FakeEventSource.last().open());
    await act(async () => FakeEventSource.last().emit("activity", req({ ID: "live", Transcript: "streamed" })));
    // The snapshot predates the request, so a plain assignment would drop it.
    await act(async () => activity.pending[1](json(emptyActivity())));
    expect(screen.getByTestId("requests").textContent).toBe("live");
  });
});

describe("generation", () => {
  function GenerationProbe() {
    const { generation, live } = useLive();
    return <output data-testid="gen">{`${live}:${generation}`}</output>;
  }

  it("bumps on every open so each consumer resyncs", () => {
    useFakeStream();
    render(
      <LiveProvider>
        <GenerationProbe />
      </LiveProvider>,
    );
    expect(screen.getByTestId("gen").textContent).toBe("false:0");
    act(() => FakeEventSource.last().open());
    expect(screen.getByTestId("gen").textContent).toBe("true:1");
  });
});
