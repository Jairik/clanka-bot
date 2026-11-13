// The Activity page's filter chips, driven through the real page.
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { ActivityPage } from "../pages/Activity";
import { emptyActivity, json, Providers, req, session, stubFetch } from "./harness";

const view = (requests = [
  req({ ID: "v", Source: "voice", SourceLabel: "Voice", Transcript: "spoken at the pi" }),
  req({ ID: "t", Source: "dashboard", SourceLabel: "Dashboard typed", Transcript: "typed on the page" }),
  req({
    ID: "m",
    Source: "dashboard_mic",
    SourceLabel: "Dashboard mic",
    Transcript: "spoken at the browser",
    Stages: [{ Name: "stt_finalize", Label: "STT finalize", MS: "820 ms" }],
  }),
  req({ ID: "f", Source: "voice", SourceLabel: "Voice", Transcript: "this one broke", Outcome: "failed", OutcomeText: "Failed", Error: "boom" }),
]) =>
  emptyActivity(requests);

const routes = (requests?: Parameters<typeof view>[0]) => ({
  "/api/ui/session": () => json(session()),
  "/api/ui/activity": () => json(view(requests)),
});

const chip = async (name: string) => screen.getByRole("radio", { name });

describe("ActivityPage filters", () => {
  it("lists every request under All", async () => {
    stubFetch(routes());
    render(
      <Providers>
        <ActivityPage />
      </Providers>,
    );
    expect(await screen.findByText("spoken at the pi")).toBeTruthy();
    expect(screen.getByText("typed on the page")).toBeTruthy();
    expect(screen.getByText("this one broke")).toBeTruthy();
  });

  it("filters to problems", async () => {
    stubFetch(routes());
    const user = userEvent.setup();
    render(
      <Providers>
        <ActivityPage />
      </Providers>,
    );
    await screen.findByText("spoken at the pi");
    await user.click(await chip("Problems"));

    expect(await screen.findByText("this one broke")).toBeTruthy();
    expect(screen.queryByText("spoken at the pi")).toBeNull();
    expect(screen.queryByText("typed on the page")).toBeNull();
  });

  it("keeps Typed to the keyboard and Voice to the Pi", async () => {
    stubFetch(routes());
    const user = userEvent.setup();
    render(
      <Providers>
        <ActivityPage />
      </Providers>,
    );
    await screen.findByText("spoken at the pi");
    await user.click(await chip("Typed"));
    expect(await screen.findByText("typed on the page")).toBeTruthy();
    expect(screen.queryByText("spoken at the pi")).toBeNull();

    await user.click(await chip("Voice"));
    expect(await screen.findByText("spoken at the pi")).toBeTruthy();
    expect(screen.queryByText("typed on the page")).toBeNull();
  });

  it("filters to browser-mic requests and shows their STT time", async () => {
    stubFetch(routes());
    const user = userEvent.setup();
    render(
      <Providers>
        <ActivityPage />
      </Providers>,
    );
    await screen.findByText("spoken at the pi");
    await user.click(await chip("Mic"));

    expect(await screen.findByText("spoken at the browser")).toBeTruthy();
    expect(screen.getByText("STT 820 ms")).toBeTruthy();
    expect(screen.queryByText("spoken at the pi")).toBeNull();
    expect(screen.queryByText("typed on the page")).toBeNull();
  });

  it("offers a way out of an empty filter", async () => {
    stubFetch(routes([req({ ID: "t", Source: "dashboard", Transcript: "only typed" })]));
    const user = userEvent.setup();
    render(
      <Providers>
        <ActivityPage />
      </Providers>,
    );
    await screen.findByText("only typed");
    await user.click(await chip("Problems"));

    expect(await screen.findByText(/Nothing matches this filter/)).toBeTruthy();
    await user.click(screen.getByText("Clear"));
    expect(await screen.findByText("only typed")).toBeTruthy();
  });
});

describe("ActivityPage durable history", () => {
  const live = req({ ID: "live", Transcript: "live at the pi", TimeISO: "2026-11-13T12:00:00" });
  const old = req({ ID: "old", Transcript: "old blue timer", TimeISO: "2026-11-06T09:00:00" });
  const older = req({ ID: "older", Transcript: "oldest blue note", TimeISO: "2026-11-05T09:00:00" });

  const routes = () => ({
    "/api/ui/session": () => json(session()),
    "/api/ui/activity": () => json({ ...emptyActivity([live]), HasHistory: true, Next: "live-next" }),
    "/api/ui/activity?q=blue": () => json({ ...emptyActivity([old]), HasHistory: true, Filter: "all", Query: "blue", Next: "c1" }),
    "/api/ui/activity?q=blue&before=c1": () => json({ ...emptyActivity([older]), HasHistory: true, Query: "blue", Next: "" }),
  });

  it("searches the durable history and pages below the first page", async () => {
    stubFetch(routes());
    const user = userEvent.setup();
    render(
      <Providers>
        <ActivityPage />
      </Providers>,
    );
    expect(await screen.findByText("live at the pi")).toBeTruthy();

    const search = screen.getByRole("searchbox", { name: "Search activity" });
    await user.type(search, "blue");

    expect(await screen.findByText("old blue timer")).toBeTruthy();
    expect(screen.queryByText("oldest blue note")).toBeNull();
    expect(screen.queryByText("live at the pi")).toBeNull();

    // The loader asks for the next cursor and appends its page.
    await user.click(screen.getByRole("button", { name: "Load older" }));
    expect(await screen.findByText("oldest blue note")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Load older" })).toBeNull();
  });
});
