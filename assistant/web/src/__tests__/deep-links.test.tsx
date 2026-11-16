// Deep links and Back/Forward land where the reader was (#300): a hash names
// a server-supplied anchor, and a history entry owns the page's scroll state.
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { navigate, useLocation } from "../api";
import { LiveProvider } from "../events";
import { ActivityPage } from "../pages/Activity";
import { Home } from "../pages/Home";
import { SettingsPage } from "../pages/Settings";
import { VoicesPage } from "../pages/Voices";
import { PageFrame } from "../shell";
import type { Setting, SettingsView, VoiceRow, VoicesView } from "../types";
import { emptyActivity, json, Providers, req, session, stubFetch } from "./harness";

class NoopEventSource {
  addEventListener() {}
  removeEventListener() {}
  close() {}
}

let scrollIntoView: ReturnType<typeof vi.fn>;
let originalScrollIntoView: typeof Element.prototype.scrollIntoView;

const setScroll = (el: HTMLElement, top: number) => {
  Object.defineProperty(el, "scrollTop", { configurable: true, writable: true, value: top });
};

const setting = (key: string, anchor: string): Setting =>
  ({ Key: key, Anchor: anchor, Label: "Quiet volume", Edit: "", Display: "20", FormKind: "bool", Bool: true }) as Setting;

const settingsView = (quiet: Setting[]): SettingsView =>
  ({
    Owner: "owner@example.com",
    Page: "settings",
    Flash: "",
    Error: "",
    HasSettings: true,
    Tools: [],
    Ignored: [],
    Agent: [],
    ToolWording: [],
    Quiet: quiet,
    Live: [],
    Restart: [],
    Voice: "alba",
    CanPreview: false,
    PreviewMaxRunes: 240,
    Wakeword: { Enabled: false, Addr: "", Models: [], Note: "" },
  }) as SettingsView;

const voicesView = (voices: VoiceRow[]): VoicesView =>
  ({
    Owner: "owner@example.com",
    Page: "voices",
    Flash: "",
    Error: "",
    HasLibrary: true,
    CanUpload: false,
    CanPreview: false,
    CanPlayOnPi: false,
    SampleText: "hello",
    MaxSampleText: 240,
    MaxUpload: "1 MB",
    Revision: "r1",
    ActiveName: "",
    Default: "alba",
    Rate: "1",
    RateSource: "config",
    RateLabel: "1",
    RateLower: "1",
    RateEdit: "1",
    HasRate: false,
    Voices: voices,
  }) as VoicesView;

const serveSettings = (quiet: Setting[]) =>
  stubFetch({
    "/api/ui/settings": () => json(settingsView(quiet)),
    "/api/ui/equalizer": () =>
      json({ Profile: { Preset: "Flat", Bands: Array(10).fill(0) }, Custom: { Preset: "Custom", Bands: Array(10).fill(0) }, Error: "" }),
  });

const serveActivity = () =>
  stubFetch({
    "/api/ui/session": () => json(session()),
    "/api/ui/activity": () =>
      json(
        emptyActivity([
          req({ ID: "ok", Transcript: "spoken at the pi", Reply: "hello" }),
          req({ ID: "bad", Transcript: "this one broke", Reply: "", Outcome: "failed", OutcomeText: "Failed", Error: "boom" }),
        ]),
      ),
  });

/** A tiny router: enough to observe which page a history entry renders. */
function Frames() {
  const { path } = useLocation();
  // The App keys each view by page id, so a navigation really remounts it.
  if (path === "/activity") return <PageFrame key="activity" id="activity"><p>Activity body</p></PageFrame>;
  if (path === "/settings") return <PageFrame key="settings" id="settings"><p>Settings body</p></PageFrame>;
  return <p>Somewhere else</p>;
}

const back = async (expected: string) => {
  await act(async () => {
    history.back();
  });
  await waitFor(() => expect(location.pathname + location.search + location.hash).toBe(expected));
};

beforeEach(() => {
  history.replaceState(null, "", "/");
  vi.stubGlobal("EventSource", NoopEventSource);
  scrollIntoView = vi.fn();
  originalScrollIntoView = Element.prototype.scrollIntoView;
  Element.prototype.scrollIntoView = scrollIntoView as typeof Element.prototype.scrollIntoView;
});

afterEach(() => {
  Element.prototype.scrollIntoView = originalScrollIntoView;
});

describe("hash deep links", () => {
  it("opens Settings scrolled to the named row with focus", async () => {
    history.replaceState(null, "", "/settings#setting-quiet.volume");
    serveSettings([setting("quiet.volume", "setting-quiet.volume")]);

    render(<SettingsPage />);

    expect(await screen.findByText("quiet.volume")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Settings" })).toBeTruthy();
    await waitFor(() => expect(document.activeElement?.id).toBe("setting-quiet.volume"));
    expect(scrollIntoView).toHaveBeenCalledWith({ block: "start", behavior: "auto" });
    // The Quiet hours section starts closed; a deep link must open it first.
    expect(document.getElementById("setting-quiet.volume")?.closest("[hidden]")).toBeNull();
    expect(document.getElementById("setting-quiet.volume")?.classList.contains("deep-link-target")).toBe(true);
  });

  it("does the same on Voices", async () => {
    history.replaceState(null, "", "/voices#voice-3");
    stubFetch({ "/api/ui/voices": () => json(voicesView([{ Name: "alba", Anchor: "voice-3" } as VoiceRow])) });

    render(<VoicesPage />);

    await waitFor(() => expect(document.getElementById("voice-3")).toBeTruthy());
    expect(screen.getByRole("heading", { name: "Voices" })).toBeTruthy();
    await waitFor(() => expect(document.activeElement?.id).toBe("voice-3"));
    expect(scrollIntoView).toHaveBeenCalledWith({ block: "start", behavior: "auto" });
  });
});

describe("query-param deep links", () => {
  it("renders the named Activity search and filter", async () => {
    history.replaceState(null, "", "/activity?q=broke&filter=problems");
    serveActivity();

    render(
      <Providers>
        <ActivityPage />
      </Providers>,
    );

    expect(await screen.findByText("this one broke")).toBeTruthy();
    expect(screen.queryByText("spoken at the pi")).toBeNull();
    expect((screen.getByPlaceholderText("Search requests, replies or tools") as HTMLInputElement).value).toBe("broke");
    expect(screen.getByRole("radio", { name: "Problems" }).getAttribute("aria-checked")).toBe("true");
  });

  it("opens the Home conversation when the link names it", async () => {
    history.replaceState(null, "", "/?view=chat");
    serveActivity();

    render(
      <LiveProvider>
        <Providers>
          <Home />
        </Providers>
      </LiveProvider>,
    );

    expect(await screen.findByRole("button", { name: "Show overview" })).toBeTruthy();
    expect(screen.getByRole("radio", { name: "Conversation" }).getAttribute("aria-checked")).toBe("true");
  });

  it("leaves a plain Home visit on the overview", async () => {
    serveActivity();

    render(
      <LiveProvider>
        <Providers>
          <Home />
        </Providers>
      </LiveProvider>,
    );

    expect(await screen.findByText("here's a quick look at what clanka has been up to.")).toBeTruthy();
    expect(screen.getByRole("radio", { name: "Overview" }).getAttribute("aria-checked")).toBe("true");
    expect(screen.queryByRole("button", { name: "Show overview" })).toBeNull();
    expect(location.search).toBe("");
  });
});

describe("Back and Forward", () => {
  it("restores the scroll position of the page Back returns to", async () => {
    render(<Frames />);
    act(() => navigate("/activity"));
    const activity = screen.getByTestId("page-scroller");
    setScroll(activity, 400);
    fireEvent.scroll(activity);

    act(() => navigate("/settings"));
    expect(screen.getByText("Settings body")).toBeTruthy();

    await back("/activity");
    expect(screen.getByText("Activity body")).toBeTruthy();
    expect(screen.getByTestId("page-scroller").scrollTop).toBe(400);
  });

  it("starts at the top on a fresh navigation, even after a scroll was saved", async () => {
    render(<Frames />);
    act(() => navigate("/activity"));
    const activity = screen.getByTestId("page-scroller");
    setScroll(activity, 400);
    fireEvent.scroll(activity);

    act(() => navigate("/settings"));
    act(() => navigate("/activity"));

    expect(screen.getByTestId("page-scroller").scrollTop).toBe(0);
  });

  it("moves between Home views instead of replacing the entry", async () => {
    serveActivity();
    render(
      <LiveProvider>
        <Providers>
          <Home />
        </Providers>
      </LiveProvider>,
    );
    await screen.findByText("here's a quick look at what clanka has been up to.");

    await userEvent.click(screen.getByRole("radio", { name: "Conversation" }));
    expect(location.search).toBe("?view=chat");
    expect(await screen.findByRole("button", { name: "Show overview" })).toBeTruthy();

    await back("/");
    expect(await screen.findByText("here's a quick look at what clanka has been up to.")).toBeTruthy();
  });
});
