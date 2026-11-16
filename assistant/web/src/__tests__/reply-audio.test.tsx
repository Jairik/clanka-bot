// A per-reply Play button must speak into the owner's browser, not the Pi.
// These drive the real Home/Exchange tree: the button fetches by request id, one
// shared audio element plays once, a second press stops, and a refusal is a line
// under the row rather than a silent media error.
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it, vi } from "vitest";
import { Home } from "../pages/Home";
import type { Session } from "../types";
import { emptyActivity, json, Providers, req, session, stubFetch } from "./harness";

const wav = () =>
  new Response(new Uint8Array([82, 73, 70, 70]), { status: 200, headers: { "Content-Type": "audio/wav" } });

/** jsdom has neither blob URLs nor media playback; the player uses both. */
const stubAudio = (onPlay: (src: string) => void = () => {}, onPause: () => void = () => {}) => {
  Object.defineProperty(URL, "createObjectURL", { configurable: true, value: () => "blob:reply-audio" });
  Object.defineProperty(URL, "revokeObjectURL", { configurable: true, value: () => {} });
  vi.stubGlobal(
    "Audio",
    class {
      onended: (() => void) | null = null;
      onerror: (() => void) | null = null;
      constructor(public src: string) {}
      play() {
        onPlay(this.src);
        return Promise.resolve();
      }
      pause() {
        onPause();
      }
    },
  );
};

const FIRST = "The first answer";
const SECOND = "The second answer";
let requested: string[] = [];

function mount(sess: Session = session(), routes: Record<string, () => Response | Promise<Response>> = {}) {
  const stub = stubFetch({
    "/api/ui/session": () => json(sess),
    "/api/ui/activity": () =>
      json(
        emptyActivity([
          req({ ID: "req-1", Reply: FIRST, Transcript: "first question" }),
          req({ ID: "req-2", Reply: SECOND, Transcript: "second question" }),
        ]),
      ),
    ...routes,
  });
  globalThis.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
    requested.push(String(input));
    return stub(input, init);
  }) as typeof fetch;
  render(
    <Providers>
      <Home />
    </Providers>,
  );
}

/** The button lives beside the reply in the same bot row. */
const row = (reply: string) => within(screen.getByText(reply).closest("div")!);
const playIn = (reply: string) => row(reply).getByRole("button", { name: /reply aloud/i });
const stopIn = (reply: string) => row(reply).getByRole("button", { name: /stop playing reply aloud/i });

beforeEach(() => {
  requested = [];
  history.replaceState(null, "", "/?view=chat");
  Element.prototype.scrollIntoView = vi.fn();
});

it("fetches the reply by id and plays it once, without putting text in the URL", async () => {
  const played: string[] = [];
  stubAudio((src) => played.push(src));
  mount(session(), { "/reply/audio/req-1": () => wav() });
  await screen.findByText(FIRST);

  await userEvent.click(playIn(FIRST));
  await waitFor(() => expect(played).toHaveLength(1));
  expect(requested).toContain("/reply/audio/req-1");
  // The route carries only the request id; the text stays in the Activity store
  // and the Pi speaker is not involved.
  expect(requested.join(" ")).not.toContain(FIRST);
  expect(requested.filter((u) => u.startsWith("/reply/audio/"))).toHaveLength(1);
  expect(stopIn(FIRST)).toBeTruthy();
});

it("stops on a second press without starting a second clip", async () => {
  const played: string[] = [];
  const paused = vi.fn();
  stubAudio((src) => played.push(src), paused);
  mount(session(), { "/reply/audio/req-1": () => wav() });
  await screen.findByText(FIRST);

  await userEvent.click(playIn(FIRST));
  await waitFor(() => expect(played).toHaveLength(1));
  await userEvent.click(stopIn(FIRST));
  await waitFor(() => expect(paused).toHaveBeenCalled());
  expect(played).toHaveLength(1);
  expect(playIn(FIRST)).toBeTruthy();
});

it("stops the first reply when a second one plays", async () => {
  const played: string[] = [];
  const paused = vi.fn();
  stubAudio((src) => played.push(src), paused);
  mount(session(), {
    "/reply/audio/req-1": () => wav(),
    "/reply/audio/req-2": () => wav(),
  });
  await screen.findByText(FIRST);

  await userEvent.click(playIn(FIRST));
  await waitFor(() => expect(played).toHaveLength(1));
  await userEvent.click(playIn(SECOND));
  await waitFor(() => expect(played).toHaveLength(2));
  expect(paused).toHaveBeenCalled();
  expect(playIn(FIRST)).toBeTruthy();
  expect(stopIn(SECOND)).toBeTruthy();
});

it("has no control when the deployment cannot synthesize", async () => {
  stubAudio();
  mount(session({ CanPlayReplies: false }), { "/reply/audio/req-1": () => wav() });
  await screen.findByText(FIRST);

  expect(screen.queryAllByRole("button", { name: /reply aloud/i })).toHaveLength(0);
  expect(requested.filter((u) => u.startsWith("/reply/audio/"))).toHaveLength(0);
});

it("shows a refused reply's line under that row and plays nothing", async () => {
  const played: string[] = [];
  stubAudio((src) => played.push(src));
  mount(session(), { "/reply/audio/req-1": () => new Response("Unknown reply.", { status: 404 }) });
  await screen.findByText(FIRST);

  await userEvent.click(playIn(FIRST));
  expect(await row(FIRST).findByRole("alert")).toBeTruthy();
  expect(row(FIRST).getByText("Unknown reply.")).toBeTruthy();
  expect(played).toHaveLength(0);
  expect(playIn(FIRST)).toBeTruthy();
});
