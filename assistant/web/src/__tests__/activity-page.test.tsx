// The Activity page's filter chips, driven through the real page.
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { ActivityPage } from "../pages/Activity";
import { emptyActivity, json, Providers, req, session, stubFetch } from "./harness";

const view = (requests = [
  req({ ID: "v", Source: "voice", SourceLabel: "Voice", Transcript: "spoken at the pi" }),
  req({ ID: "t", Source: "dashboard", SourceLabel: "Dashboard typed", Transcript: "typed on the page" }),
  req({ ID: "m", Source: "dashboard_mic", SourceLabel: "Dashboard mic", Transcript: "spoken at the browser" }),
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
