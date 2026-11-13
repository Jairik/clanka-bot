// The speaker control: it reads the level and says why a write failed.
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { VolumeControl } from "../shell";
import { emptyActivity, json, Providers, session, stubFetch } from "./harness";

const volume = (over: Record<string, unknown> = {}) => ({ Available: true, Percent: 42, Muted: false, Error: "", ...over });

const routes = (extra: Record<string, (init?: RequestInit) => Response | Promise<Response>> = {}) => ({
  "/api/ui/session": () => json(session()),
  "/api/ui/activity": () => json(emptyActivity()),
  "/api/ui/volume": () => json(volume()),
  ...extra,
});

describe("VolumeControl", () => {
  it("shows the level the speaker reported", async () => {
    stubFetch(routes());
    render(
      <Providers>
        <VolumeControl />
      </Providers>,
    );
    expect(await screen.findAllByText("42%")).not.toHaveLength(0);
  });

  it("shows the server's error text when a write is refused", async () => {
    stubFetch(routes({ "/api/ui/volume": (init) => (init?.method === "POST" ? json(volume({ Error: "The speaker did not accept that change." }), 502) : json(volume())) }));
    const user = userEvent.setup();
    render(
      <Providers>
        <VolumeControl />
      </Providers>,
    );

    await user.click(await screen.findByRole("button", { name: "Mute speaker" }));
    expect((await screen.findByRole("status")).textContent).toContain("The speaker did not accept that change.");
  });

  it("says when the speaker could not be reached at all", async () => {
    stubFetch(routes({ "/api/ui/volume": (init) => (init?.method === "POST" ? Promise.reject(new TypeError("network")) : json(volume())) }));
    const user = userEvent.setup();
    render(
      <Providers>
        <VolumeControl />
      </Providers>,
    );

    await user.click(await screen.findByRole("button", { name: "Mute speaker" }));
    expect((await screen.findByRole("status")).textContent).toContain("Couldn't reach the speaker.");
  });

  it("reports an unavailable speaker instead of an inert control", async () => {
    stubFetch(routes({ "/api/ui/volume": () => json(volume({ Available: false, Error: "The speaker is not wired." })) }));
    render(
      <Providers>
        <VolumeControl />
      </Providers>,
    );
    expect(await screen.findByText("The speaker is not wired.")).toBeTruthy();
  });
});
