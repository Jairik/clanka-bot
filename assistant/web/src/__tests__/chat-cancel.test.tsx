// Cancelling a pending typed request: the POST is aborted, the pending bubble
// leaves the list, and cancelling an already-finished request is a no-op.
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useRef } from "react";
import { describe, expect, it } from "vitest";
import { ChatProvider, useChat } from "../chat";
import { emptyActivity, json, req, stubFetch } from "./harness";

function Probe() {
  const { pending, send, cancel } = useChat();
  const lastId = useRef<string | null>(null);
  if (pending.length > 0) lastId.current = pending[pending.length - 1].id;
  return (
    <div>
      <output data-testid="ids">{pending.map((p) => p.id).join("|")}</output>
      <button onClick={() => send("what time is it")}>send</button>
      <button onClick={() => { if (lastId.current) cancel(lastId.current); }}>cancel-last</button>
      <button onClick={() => cancel("missing")}>cancel-missing</button>
      {pending.map((p) => (
        <button key={p.id} onClick={() => cancel(p.id)} aria-label={`cancel ${p.id}`}>
          cancel-{p.id}
        </button>
      ))}
    </div>
  );
}

describe("ChatProvider cancel", () => {
  it("aborts the in-flight POST and removes the pending bubble", async () => {
    let signal: AbortSignal | null | undefined;
    stubFetch({
      "/api/ui/activity": () => json(emptyActivity()),
      // Never settles: the request stays in flight so cancel has something to abort.
      "/activity/ask": (init) => {
        signal = init?.signal;
        return new Promise<Response>(() => {});
      },
    });
    const user = userEvent.setup();

    render(
      <ChatProvider>
        <Probe />
      </ChatProvider>,
    );

    await user.click(screen.getByRole("button", { name: "send" }));
    expect(screen.getByTestId("ids").textContent).toContain("p");

    // Only the per-pending Cancel affordance aborts; the others are no-op stubs.
    await user.click(screen.getByRole("button", { name: /^cancel p/ }));
    // The AbortController signal reached the POST and was aborted.
    await waitFor(() => expect(signal?.aborted).toBe(true));
    // The bubble leaves the pending list.
    expect(screen.getByTestId("ids").textContent).toBe("");
  });

  it("is a no-op when cancelling an already-finished request", async () => {
    let abortCalls = 0;
    stubFetch({
      "/api/ui/activity": () => json(emptyActivity()),
      "/activity/ask": (init) => {
        // A finished request has no live controller, so abort must never fire.
        if (init?.signal) init.signal.addEventListener("abort", () => { abortCalls += 1; });
        return json(emptyActivity([req({ ID: "done", Transcript: "what time is it", Outcome: "no_audio", OutcomeText: "No audio" })]));
      },
    });
    const user = userEvent.setup();

    render(
      <ChatProvider>
        <Probe />
      </ChatProvider>,
    );

    await user.click(screen.getByRole("button", { name: "send" }));
    // The request settles and the bubble is gone; its controller is deleted.
    await waitFor(() => expect(screen.getByTestId("ids").textContent).toBe(""));
    expect(abortCalls).toBe(0);

    // Cancelling the finished id, or any unknown id, is a no-op.
    await user.click(screen.getByRole("button", { name: "cancel-last" }));
    await user.click(screen.getByRole("button", { name: "cancel-missing" }));
    expect(abortCalls).toBe(0);
    expect(screen.getByTestId("ids").textContent).toBe("");
  });
});
