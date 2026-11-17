// The Activity page's filter chips, driven through the real page.
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
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
    "/api/ui/activity?before=live-next": () => json({ ...emptyActivity([old]), HasHistory: true, Next: "c1" }),
    "/api/ui/activity?before=c1": () => json({ ...emptyActivity([older]), HasHistory: true, Next: "" }),
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

  it("says when a short search only covered the newest window", async () => {
    const notice =
      "Short searches cover only the newest 200 requests. Type at least three characters to search all stored history.";
    stubFetch({
      "/api/ui/session": () => json(session()),
      "/api/ui/activity": () => json({ ...emptyActivity([live]), HasHistory: true }),
      "/api/ui/activity?q=z": () =>
        json({ ...emptyActivity([old]), HasHistory: true, Query: "z", SearchNotice: notice }),
    });
    const user = userEvent.setup();
    render(
      <Providers>
        <ActivityPage />
      </Providers>,
    );
    expect(await screen.findByText("live at the pi")).toBeTruthy();

    await user.type(screen.getByRole("searchbox", { name: "Search activity" }), "z");

    // The store's own limit is stated, and an empty page never claims a
    // completeness the windowed search did not have.
    expect(await screen.findByText(notice)).toBeTruthy();
    expect(await screen.findByText(/in the searched window/)).toBeTruthy();
  });

  it("pages the plain history below the first page instead of repeating it", async () => {
    stubFetch(routes());
    const user = userEvent.setup();
    render(
      <Providers>
        <ActivityPage />
      </Providers>,
    );
    expect(await screen.findByText("live at the pi")).toBeTruthy();

    await user.click(await screen.findByRole("button", { name: "Load older" }));
    expect(await screen.findByText("old blue timer")).toBeTruthy();

    // The second click must follow the loaded page's cursor, not refetch page 2.
    await user.click(screen.getByRole("button", { name: "Load older" }));
    expect(await screen.findByText("oldest blue note")).toBeTruthy();
    expect(screen.getAllByText("old blue timer")).toHaveLength(1);
    expect(screen.queryByRole("button", { name: "Load older" })).toBeNull();
  });
});

it("renders the Focus strip from the focus route", async () => {
  stubFetch(routes());
  render(
    <Providers>
      <ActivityPage />
    </Providers>,
  );
  await screen.findByText("spoken at the pi");
  // The Activity header reads /api/ui/focus and shows the idle state.
  expect(await screen.findByText("Listening")).toBeTruthy();
});

describe("ActivityPage search debounce", () => {
  const live = req({ ID: "live", Transcript: "live at the pi", TimeISO: "2026-11-13T12:00:00" });
  const match = req({ ID: "match", Transcript: "hello world request", TimeISO: "2026-11-06T09:00:00" });

  it("debounces typing: five characters produce one replaceState call and one request", async () => {
    const queryHandler = vi.fn(() =>
      json({ ...emptyActivity([match]), HasHistory: true, Query: "hello" }),
    );
    stubFetch({
      "/api/ui/session": () => json(session()),
      "/api/ui/activity": () => json({ ...emptyActivity([live]), HasHistory: true }),
      "/api/ui/activity?q=hello": queryHandler,
    });
    const replaceSpy = vi.spyOn(history, "replaceState");
    const user = userEvent.setup();
    render(
      <Providers>
        <ActivityPage />
      </Providers>,
    );
    expect(await screen.findByText("live at the pi")).toBeTruthy();
    expect(replaceSpy).not.toHaveBeenCalled();

    const search = screen.getByRole("searchbox", { name: "Search activity" });
    await user.type(search, "hello");

    // Immediately after typing, history.replaceState must not have been called once per character.
    expect(replaceSpy).not.toHaveBeenCalled();
    expect(queryHandler).not.toHaveBeenCalled();

    // After the 250 ms debounce, the URL catches up once and the request is sent once.
    expect(await screen.findByText("hello world request")).toBeTruthy();
    expect(replaceSpy).toHaveBeenCalledTimes(1);
    expect(queryHandler).toHaveBeenCalledTimes(1);
  });

  it("flushes immediately on blur without waiting for the debounce", async () => {
    const blurMatch = req({ ID: "bm", Transcript: "fast blur match", TimeISO: "2026-11-06T09:00:00" });
    const queryHandler = vi.fn(() =>
      json({ ...emptyActivity([blurMatch]), HasHistory: true, Query: "fast" }),
    );
    stubFetch({
      "/api/ui/session": () => json(session()),
      "/api/ui/activity": () => json({ ...emptyActivity([live]), HasHistory: true }),
      "/api/ui/activity?q=fast": queryHandler,
    });
    const replaceSpy = vi.spyOn(history, "replaceState");
    const user = userEvent.setup();
    render(
      <Providers>
        <ActivityPage />
      </Providers>,
    );
    expect(await screen.findByText("live at the pi")).toBeTruthy();

    const search = screen.getByRole("searchbox", { name: "Search activity" });
    await user.type(search, "fast");
    expect(replaceSpy).not.toHaveBeenCalled();

    // Blurring immediately flushes to the URL without waiting 250 ms.
    await user.tab();
    expect(replaceSpy).toHaveBeenCalledTimes(1);
    expect(await screen.findByText("fast blur match")).toBeTruthy();
    // No second replaceState call when the debounce timeout finishes.
    expect(replaceSpy).toHaveBeenCalledTimes(1);
  });

  it("flushes immediately on Enter without waiting for the debounce", async () => {
    const enterMatch = req({ ID: "em", Transcript: "enter match", TimeISO: "2026-11-06T09:00:00" });
    const queryHandler = vi.fn(() =>
      json({ ...emptyActivity([enterMatch]), HasHistory: true, Query: "enter" }),
    );
    stubFetch({
      "/api/ui/session": () => json(session()),
      "/api/ui/activity": () => json({ ...emptyActivity([live]), HasHistory: true }),
      "/api/ui/activity?q=enter": queryHandler,
    });
    const replaceSpy = vi.spyOn(history, "replaceState");
    const user = userEvent.setup();
    render(
      <Providers>
        <ActivityPage />
      </Providers>,
    );
    expect(await screen.findByText("live at the pi")).toBeTruthy();

    const search = screen.getByRole("searchbox", { name: "Search activity" });
    await user.type(search, "enter{Enter}");
    expect(replaceSpy).toHaveBeenCalledTimes(1);
    expect(await screen.findByText("enter match")).toBeTruthy();
    // No second replaceState call after debounce expires.
    expect(replaceSpy).toHaveBeenCalledTimes(1);
  });

  it("restores the query into the field on back/forward navigation", async () => {
    const q1Req = req({ ID: "q1", Transcript: "first result", TimeISO: "2026-11-06T09:00:00" });
    const q2Req = req({ ID: "q2", Transcript: "second result", TimeISO: "2026-11-06T08:00:00" });
    stubFetch({
      "/api/ui/session": () => json(session()),
      "/api/ui/activity": () => json({ ...emptyActivity([live]), HasHistory: true }),
      "/api/ui/activity?q=first": () => json({ ...emptyActivity([q1Req]), HasHistory: true, Query: "first" }),
      "/api/ui/activity?q=second": () => json({ ...emptyActivity([q2Req]), HasHistory: true, Query: "second" }),
    });
    const user = userEvent.setup();
    render(
      <Providers>
        <ActivityPage />
      </Providers>,
    );
    expect(await screen.findByText("live at the pi")).toBeTruthy();

    const search = screen.getByRole("searchbox", { name: "Search activity" }) as HTMLInputElement;
    await user.type(search, "first{Enter}");
    expect(await screen.findByText("first result")).toBeTruthy();
    expect(search.value).toBe("first");

    // Navigate to second query.
    await user.clear(search);
    await user.type(search, "second{Enter}");
    expect(await screen.findByText("second result")).toBeTruthy();
    expect(search.value).toBe("second");

    // Simulate Back navigation restoring the first query.
    history.replaceState(null, "", "/activity?q=first");
    window.dispatchEvent(new PopStateEvent("popstate"));

    await waitFor(() => expect(search.value).toBe("first"));
    expect(await screen.findByText("first result")).toBeTruthy();

    // Simulate Forward navigation restoring the second query.
    history.replaceState(null, "", "/activity?q=second");
    window.dispatchEvent(new PopStateEvent("popstate"));

    await waitFor(() => expect(search.value).toBe("second"));
    expect(await screen.findByText("second result")).toBeTruthy();

    // Simulate Back navigation to empty query.
    history.replaceState(null, "", "/activity");
    window.dispatchEvent(new PopStateEvent("popstate"));

    await waitFor(() => expect(search.value).toBe(""));
    expect(await screen.findByText("live at the pi")).toBeTruthy();
  });
});

