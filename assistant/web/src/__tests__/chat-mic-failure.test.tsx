// A failed mic upload must not show "Recording" (or "Transcribing…") as if it
// were the user's transcript: the bubble is left empty and the error + Retry
// below say what happened. See issue #248.
import { render, screen } from "@testing-library/react";
import { expect, it } from "vitest";
import { PendingExchange } from "../chat";
import type { PendingAsk } from "../chat";
import { emptyActivity, json, Providers, session, stubFetch } from "./harness";

const pending = (over: Partial<PendingAsk>): PendingAsk => ({
  id: "p1",
  source: "dashboard_mic",
  text: "",
  at: new Date("2026-11-16T10:00:00"),
  ...over,
});

it("shows no transcript wording for a failed mic upload, only the error and Retry", async () => {
  stubFetch({
    "/api/ui/session": () => json(session()),
    "/api/ui/activity": () => json(emptyActivity()),
  });

  render(
    <Providers>
      <PendingExchange p={pending({ error: "connect ECONNREFUSED" })} />
    </Providers>,
  );

  // The session gates the tree, so wait for it to mount.
  // Neither word may appear as the bubble's (mistaken) transcript.
  expect(await screen.findByText("connect ECONNREFUSED")).toBeTruthy();
  expect(screen.queryByText("Recording")).toBeNull();
  expect(screen.queryByText("Transcribing…")).toBeNull();
  // The real outcome is still shown and actionable.
  expect(await screen.findByRole("button", { name: /Retry/i })).toBeTruthy();
  expect(screen.getByRole("button", { name: /Dismiss/i })).toBeTruthy();
});

it("still shows Transcribing… for a pending (not yet failed) mic take", async () => {
  stubFetch({
    "/api/ui/session": () => json(session()),
    "/api/ui/activity": () => json(emptyActivity()),
  });

  render(
    <Providers>
      <PendingExchange p={pending({})} />
    </Providers>,
  );

  expect(await screen.findByText("Transcribing…")).toBeTruthy();
  expect(screen.queryByText("Recording")).toBeNull();
});
