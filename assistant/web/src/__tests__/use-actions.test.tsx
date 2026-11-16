// useActions: concurrent writes must resolve in order (newest wins) and the
// pending state must be keyed so two rows that share a URL stay distinct.
import { act, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { WriteResult } from "../types";
import { useActions } from "../forms";

type TestView = { id: string };

// A controllable postForm: each call registers a resolver we settle by hand so
// the test can resolve responses out of order.
let resolvers: Array<(r: WriteResult<TestView>) => void>;
let captured: string[];
vi.mock("../api", () => ({
  postForm: vi.fn(async (action: string) => {
    captured.push(action);
    return new Promise<WriteResult<TestView>>((resolve) => {
      resolvers.push(resolve);
    });
  }),
}));

let lastView: TestView | null = null;
let actionsRef: ReturnType<typeof useActions<TestView>> | null = null;

function Harness() {
  const actions = useActions<TestView>((v) => {
    lastView = v;
  });
  actionsRef = actions;
  return (
    <div>
      <span data-testid="pending">{actions.pending}</span>
      <span data-testid="pk">{actions.pendingKey}</span>
      <span data-testid="view">{lastView ? lastView.id : ""}</span>
    </div>
  );
}

beforeEach(() => {
  resolvers = [];
  captured = [];
  lastView = null;
  actionsRef = null;
  vi.clearAllMocks();
});

afterEach(() => {
  vi.useRealTimers();
});

describe("useActions ordering", () => {
  it("lets the newest write's view win when responses arrive out of order", async () => {
    render(<Harness />);
    const run = actionsRef!.run;

    // Two writes to different URLs; the first one's answer will be held back.
    const a = run("/a", { id: "first" });
    const b = run("/b", { id: "second" });

    expect(resolvers).toHaveLength(2);

    // Resolve the second write first (newest), then the first (now stale).
    await act(async () => {
      resolvers[1]({ ok: true, status: 200, view: { id: "second" }, text: "" });
    });
    await act(async () => {
      resolvers[0]({ ok: true, status: 200, view: { id: "first" }, text: "" });
    });

    await expect(b).resolves.toBe(true);
    await expect(a).resolves.toBe(true);

    // The newer response owns the view — the stale first reply was ignored.
    expect(screen.getByTestId("view").textContent).toBe("second");
    // Once both settles, the spinner is cleared.
    expect(screen.getByTestId("pending").textContent).toBe("");
  });

  it("clears pending only after the newest write settles, not on a stale one", async () => {
    render(<Harness />);
    const run = actionsRef!.run;

    run("/a", { id: "first" });
    run("/b", { id: "second" });

    // Settle the stale (first) write while the newest is still in flight.
    await act(async () => {
      resolvers[0]({ ok: true, status: 200, view: { id: "first" }, text: "" });
    });
    // The spinner must still be running for the newest write.
    expect(screen.getByTestId("pending").textContent).toBe("/b");

    await act(async () => {
      resolvers[1]({ ok: true, status: 200, view: { id: "second" }, text: "" });
    });
    expect(screen.getByTestId("pending").textContent).toBe("");
  });
});

describe("useActions pending key", () => {
  it("falls back to the action URL and accepts an explicit key", async () => {
    render(<Harness />);
    const run = actionsRef!.run;

    // No key: pending and pendingKey are the URL.
    const a = run("/x");
    await act(async () => {});
    expect(screen.getByTestId("pending").textContent).toBe("/x");
    expect(screen.getByTestId("pk").textContent).toBe("/x");
    await act(async () => {
      resolvers[0]({ ok: true, status: 200, view: { id: "x" }, text: "" });
    });
    await expect(a).resolves.toBe(true);

    // With a key, pending stays the URL but pendingKey carries the caller's key.
    const b = run("/phrasebook/Ada", { tool: "lights" }, undefined, "lights");
    await act(async () => {});
    expect(screen.getByTestId("pending").textContent).toBe("/phrasebook/Ada");
    expect(screen.getByTestId("pk").textContent).toBe("lights");
    await act(async () => {
      resolvers[1]({ ok: true, status: 200, view: { id: "y" }, text: "" });
    });
    await expect(b).resolves.toBe(true);
  });
});
