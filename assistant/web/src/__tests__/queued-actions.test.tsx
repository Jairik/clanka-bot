// The queued-action surfaces read the view's cap and countdown, and Cancel
// posts the id without touching any other API (#297).
import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { QueuedActionsCard } from "../pages/Home";
import { AlertsPage } from "../pages/Alerts";
import { DEFERRED_POLL_MS, expiresIn, type DeferredItem, type DeferredView } from "../queued-actions";
import { json, Providers, session, stubFetch } from "./harness";

afterEach(() => {
  vi.useRealTimers();
});

/** A mutable view so a Cancel POST to /api/deferred is visible to the refresh. */
function queueRoutes(items: DeferredItem[], limit = 8) {
  const view = (): DeferredView => ({ items, limit });
  const postBodies: string[] = [];
  const deferred = vi.fn((init?: RequestInit) => {
    if (init?.method === "POST") {
      const body = String(init.body);
      postBodies.push(body);
      const id = Number(new URLSearchParams(body).get("id"));
      const index = items.findIndex((item) => item.id === id);
      if (index >= 0) items.splice(index, 1);
    }
    return json(view());
  });
  return {
    deferred,
    postBodies,
    routes: { "/api/ui/session": () => json(session()), "/api/deferred": deferred },
  };
}

function alertRoutes(queue: ReturnType<typeof queueRoutes>) {
  return {
    "/api/ui/alerts": () => json({ HasManagers: true, Alerts: [], Alarms: [], Missed: [], ZoneName: "UTC", MissedLimit: 20 }),
    ...queue.routes,
  };
}

describe("expiresIn", () => {
  it("rounds up minutes and renders hours", () => {
    const now = new Date("2026-11-16T12:00:00Z").getTime();
    expect(expiresIn("2026-11-16T12:43:00Z", now)).toBe("43 min");
    expect(expiresIn("2026-11-16T12:00:01Z", now)).toBe("1 min");
    expect(expiresIn("2026-11-16T13:05:00Z", now)).toBe("1 h 5 min");
    expect(expiresIn("2026-11-16T11:59:59Z", now)).toBe("expired");
  });
});

describe("Home queued actions card", () => {
  it("shows the label, event, ticking expiry and view cap, and posts Cancel", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.setSystemTime(new Date("2026-11-16T12:00:00Z"));
    const expires = new Date(Date.now() + 3 * 60_000).toISOString();
    const queue = queueRoutes([{ id: 7, text: "dim the lights", event: "reply", expires }], 3);
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    stubFetch(queue.routes);
    render(<Providers><QueuedActionsCard /></Providers>);
    expect(await screen.findByText("dim the lights")).toBeTruthy();
    expect(screen.getByText(/after reply/).textContent).toContain("3 min");
    expect(screen.getByText("1/3")).toBeTruthy();

    // The local clock ticks independently of the visibility-gated fetch.
    await act(async () => { vi.advanceTimersByTime(30_000); });
    expect(screen.getByText(/after reply/).textContent).toContain("3 min");
    await act(async () => { vi.advanceTimersByTime(60_000); });
    expect(screen.getByText(/after reply/).textContent).toContain("2 min");

    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(new URLSearchParams(queue.postBodies[0]).get("id")).toBe("7");
    expect(await screen.findByText("Nothing is queued.")).toBeTruthy();
    expect(screen.getByText("0/3")).toBeTruthy();
  });

  it("polls at the shared visible cadence", async () => {
    vi.useFakeTimers();
    const queue = queueRoutes([]);
    stubFetch(queue.routes);
    render(<Providers><QueuedActionsCard /></Providers>);
    await act(async () => {});
    expect(queue.deferred).toHaveBeenCalledTimes(1);
    await act(async () => { vi.advanceTimersByTime(DEFERRED_POLL_MS); });
    expect(queue.deferred).toHaveBeenCalledTimes(2);
  });
});

describe("Alerts queued actions section", () => {
  it("renders the label, event, expiry and server cap", async () => {
    const expires = new Date(Date.now() + 43 * 60_000).toISOString();
    stubFetch(alertRoutes(queueRoutes([{ id: 42, text: "turn the lights off", event: "music", expires }])));
    render(<Providers><AlertsPage /></Providers>);
    const section = await screen.findByRole("button", { name: /Queued actions/ });
    expect(within(section).getByText("1/8")).toBeTruthy();
    expect(screen.getByText("turn the lights off")).toBeTruthy();
    expect(screen.getByText(/after music/).textContent).toContain("43 min");
  });

  it("cancels from the section and refreshes the cap count", async () => {
    const expires = new Date(Date.now() + 43 * 60_000).toISOString();
    const queue = queueRoutes([{ id: 42, text: "turn the lights off", event: "music", expires }]);
    stubFetch(alertRoutes(queue));
    const user = userEvent.setup();
    render(<Providers><AlertsPage /></Providers>);
    await user.click(await screen.findByRole("button", { name: "Cancel" }));
    expect(new URLSearchParams(queue.postBodies[0]).get("id")).toBe("42");
    expect(await screen.findByText("Nothing is queued.")).toBeTruthy();
    expect(screen.getByText("0/8")).toBeTruthy();
  });
});
