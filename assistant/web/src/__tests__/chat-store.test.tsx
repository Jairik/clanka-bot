// The chat store's pending list, including two sends in the same millisecond.
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { ChatProvider, useChat } from "../chat";
import { emptyActivity, json, req, stubFetch } from "./harness";

function Probe() {
  const { pending, send } = useChat();
  return (
    <div>
      <output data-testid="ids">{pending.map((p) => `${p.id}:${p.text}`).join("|")}</output>
      <button onClick={() => send("one")}>one</button>
      <button onClick={() => send("two")}>two</button>
    </div>
  );
}

function Requests() {
  const { requests } = useChat();
  return <ul>{requests.map((r) => <li key={r.ID}>{r.Transcript}</li>)}</ul>;
}

describe("ChatProvider polling fallback", () => {
  it("refreshes on the four-second poll when no live stream is available", async () => {
    let calls = 0;
    stubFetch({
      "/api/ui/activity": () => {
        calls += 1;
        return json(emptyActivity(calls > 1 ? [req({ ID: "late", Transcript: "late request" })] : []));
      },
    });
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      render(
        <ChatProvider>
          <Requests />
        </ChatProvider>,
      );
      await act(() => vi.advanceTimersByTimeAsync(50));
      expect(screen.queryByText("late request")).toBeNull();
      // jsdom has no EventSource, which is exactly the fallback path.
      await act(() => vi.advanceTimersByTimeAsync(4_100));
      expect(screen.getByText("late request")).toBeTruthy();
      expect(calls).toBeGreaterThan(1);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("ChatProvider pending requests", () => {
  it("keeps two sends in the same millisecond as separate entries", async () => {
    stubFetch({
      "/api/ui/activity": () => json(emptyActivity()),
      // Never settles: both requests must stay pending so their ids can be compared.
      "/activity/ask": () => new Promise<Response>(() => {}),
    });
    vi.spyOn(Date, "now").mockReturnValue(1_700_000_000_000);
    const user = userEvent.setup();

    render(
      <ChatProvider>
        <Probe />
      </ChatProvider>,
    );
    await user.click(screen.getByRole("button", { name: "one" }));
    await user.click(screen.getByRole("button", { name: "two" }));

    const entries = screen.getByTestId("ids").textContent!.split("|");
    expect(entries).toHaveLength(2);
    const ids = entries.map((e) => e.split(":")[0]);
    expect(new Set(ids).size).toBe(2);
  });
});
