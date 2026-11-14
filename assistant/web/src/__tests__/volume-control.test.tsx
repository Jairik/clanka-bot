// The speaker control: it reads the level and says why a write failed.
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { VolumeControl } from "../shell";
import { emptyActivity, json, Providers, session, stubFetch } from "./harness";

const volume = (over: Record<string, unknown> = {}) => ({ Available: true, Percent: 42, Muted: false, Error: "", Stale: false, ...over });

// What the bar says while the speaker has not answered a read (#160).
const UNCONFIRMED = "The speaker has not answered just now; this is the last level it reported.";

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

  it("keeps the last level it reported when the speaker does not answer", async () => {
    stubFetch(routes({ "/api/ui/volume": () => json(volume({ Stale: true })) }));
    render(
      <Providers>
        <VolumeControl />
      </Providers>,
    );

    // Both places that state the level still do, and the label says which level
    // it is: the sm number carries the mark and the hint, the phone button its
    // own accessible name.
    const label = await screen.findByTitle(UNCONFIRMED);
    expect(label.textContent).toContain("42%");
    expect(label.textContent).toContain("?");
    expect(screen.getByRole("button", { name: "Volume 42%, last level the speaker reported" }).textContent).toContain("42%");
  });

  it("drops the last-known mark once a read is confirmed", async () => {
    stubFetch(routes());
    render(
      <Providers>
        <VolumeControl />
      </Providers>,
    );

    expect(await screen.findAllByText("42%")).not.toHaveLength(0);
    expect(screen.queryByTitle(UNCONFIRMED)).toBeNull();
    expect(screen.queryByText("?")).toBeNull();
    expect(screen.getByRole("button", { name: "Volume 42%" })).toBeTruthy();
  });
});
