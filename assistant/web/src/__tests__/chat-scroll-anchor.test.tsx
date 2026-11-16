// A reply arriving while the reader is up the conversation must not yank them
// to the bottom (#293). Follow only near the end (or for this tab's own sends),
// and offer a pill to jump down otherwise.
import { act, fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { LiveProvider } from "../events";
import { Home } from "../pages/Home";
import { emptyActivity, json, Providers, req, session, stubFetch } from "./harness";

/** A stand-in for EventSource: the test drives the activity frames by hand. */
class FakeEventSource {
  static all: FakeEventSource[] = [];
  static last() {
    return FakeEventSource.all[FakeEventSource.all.length - 1];
  }
  onopen: ((event: Event) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  closed = false;
  private listeners = new Map<string, Set<(event: MessageEvent<string>) => void>>();
  constructor(public url: string) {
    FakeEventSource.all.push(this);
  }
  addEventListener(name: string, listener: (event: MessageEvent<string>) => void) {
    const set = this.listeners.get(name) ?? new Set();
    set.add(listener);
    this.listeners.set(name, set);
  }
  removeEventListener(name: string, listener: (event: MessageEvent<string>) => void) {
    this.listeners.get(name)?.delete(listener);
  }
  close() {
    this.closed = true;
  }
  emit(name: string, data: unknown) {
    const event = new MessageEvent("message", { data: JSON.stringify(data) });
    this.listeners.get(name)?.forEach((listener) => listener(event));
  }
}

let scrollIntoView: ReturnType<typeof vi.fn>;
let originalScrollIntoView: typeof Element.prototype.scrollIntoView;

const setMedia = (reduced: boolean) => {
  Object.defineProperty(window, "matchMedia", {
    configurable: true,
    writable: true,
    value: () => ({ matches: reduced, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {} }),
  });
};

/** jsdom reports zero for every scroll metric; give the container a geometry. */
function setScroll(el: HTMLElement, top: number, height = 1000, viewport = 400) {
  Object.defineProperty(el, "scrollHeight", { configurable: true, value: height });
  Object.defineProperty(el, "clientHeight", { configurable: true, value: viewport });
  Object.defineProperty(el, "scrollTop", { configurable: true, writable: true, value: top });
}

async function mountChat() {
  render(
    <LiveProvider>
      <Providers>
        <Home />
      </Providers>
    </LiveProvider>,
  );
  // Wait for the snapshot so the stream's delta has a view to merge into.
  await screen.findByText("earlier answer");
  scrollIntoView.mockClear();
}

const stream = (data: unknown) => act(async () => FakeEventSource.last().emit("activity", data));
const pill = () => screen.queryByRole("button", { name: "Scroll to new reply" });

beforeEach(() => {
  FakeEventSource.all = [];
  vi.stubGlobal("EventSource", FakeEventSource);
  Object.defineProperty(document, "visibilityState", { configurable: true, get: () => "visible" });
  history.replaceState(null, "", "/?view=chat");
  setMedia(false);
  scrollIntoView = vi.fn();
  originalScrollIntoView = Element.prototype.scrollIntoView;
  Element.prototype.scrollIntoView = scrollIntoView as typeof Element.prototype.scrollIntoView;
  stubFetch({
    "/api/ui/session": () => json(session()),
    "/api/ui/activity": () => json(emptyActivity([req({ ID: "old", Transcript: "earlier question", Reply: "earlier answer" })])),
    // A send stays pending, so justSent can be observed in isolation.
    "/activity/ask": () => new Promise<Response>(() => {}),
  });
});

afterEach(() => {
  Element.prototype.scrollIntoView = originalScrollIntoView;
});

describe("Home chat scroll anchor", () => {
  it("holds the reader's position and shows a jump pill when a reply lands while scrolled up", async () => {
    await mountChat();
    const el = screen.getByTestId("chat-scroller");
    // 1000 - 300 - 400 = 300px from the end: comfortably scrolled away.
    setScroll(el, 300);
    fireEvent.scroll(el);

    await stream(req({ ID: "from-pi", Transcript: "new voice turn", Reply: "fresh reply" }));

    expect(scrollIntoView).not.toHaveBeenCalled();
    expect(el.scrollTop).toBe(300);
    expect(pill()?.textContent).toContain("New reply");

    scrollIntoView.mockClear();
    await userEvent.click(pill()!);
    expect(scrollIntoView).toHaveBeenCalledWith({ block: "end", behavior: "smooth" });
    expect(pill()).toBeNull();
  });

  it("still follows a streamed reply when the reader is at the bottom", async () => {
    await mountChat();
    const el = screen.getByTestId("chat-scroller");
    setScroll(el, 600); // 1000 - 600 - 400 = 0: at the end
    fireEvent.scroll(el);
    scrollIntoView.mockClear();

    await stream(req({ ID: "from-pi", Transcript: "new voice turn", Reply: "fresh reply" }));

    expect(scrollIntoView).toHaveBeenCalledWith({ block: "end", behavior: "smooth" });
    expect(pill()).toBeNull();
  });

  it("follows this tab's own send even when scrolled up", async () => {
    await mountChat();
    const el = screen.getByTestId("chat-scroller");
    setScroll(el, 300);
    fireEvent.scroll(el);
    scrollIntoView.mockClear();

    const user = userEvent.setup();
    await user.type(screen.getByRole("textbox", { name: "Message" }), "a question");
    await user.click(screen.getByRole("button", { name: "Send" }));

    expect(scrollIntoView).toHaveBeenCalledWith({ block: "end", behavior: "smooth" });
    expect(pill()).toBeNull();
  });

  it("jumps instantly for a reader who prefers reduced motion", async () => {
    setMedia(true);
    await mountChat();
    const el = screen.getByTestId("chat-scroller");
    setScroll(el, 600);
    fireEvent.scroll(el);
    scrollIntoView.mockClear();

    await stream(req({ ID: "from-pi", Transcript: "new voice turn", Reply: "fresh reply" }));

    expect(scrollIntoView).toHaveBeenCalledWith({ block: "end", behavior: "auto" });
  });
});
