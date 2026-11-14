// The Agent wording preview follows the editor's unsaved draft, so these drive
// the real inputs: what is on screen is what a save would speak or match.
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";
import { SettingsPage } from "../pages/Settings";
import type { Setting, SettingsView } from "../types";
import { json, stubFetch } from "./harness";

const wav = () =>
  new Response(new Blob([new Uint8Array([82, 73, 70, 70])], { type: "audio/wav" }), {
    status: 200,
    headers: { "Content-Type": "audio/wav" },
  });

/** jsdom has neither blob URLs nor media playback; the player uses both. */
const stubAudio = (onPlay: (src: string) => void = () => {}, onPause: () => void = () => {}) => {
  Object.defineProperty(URL, "createObjectURL", { configurable: true, value: () => "blob:wording-preview" });
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

const setting = (over: Partial<Setting>): Setting => ({
  Key: "agent.filler",
  Anchor: "agent-filler",
  Label: "Filler",
  Description: "",
  FormKind: "text",
  Value: "",
  Edit: "",
  Display: "",
  Bool: false,
  Source: "code",
  SourceLabel: "Code default",
  LowerLabel: "code default",
  Restart: false,
  Override: false,
  SaveAction: "/settings/agent.filler",
  ResetAction: "/settings/agent.filler/reset",
  Target: "",
  PreviewAction: "",
  PreviewKind: "",
  ...over,
});

const spoken = (over: Partial<Setting> = {}) =>
  setting({ ...over, PreviewAction: "/settings/wording/preview", PreviewKind: "spoken" });

const view = (agent: Setting[], over: Partial<SettingsView> = {}): SettingsView => ({
  Owner: "owner@example.com",
  Page: "settings",
  Flash: "",
  Error: "",
  HasSettings: true,
  Tools: [],
  Agent: agent,
  ToolWording: [],
  Quiet: [],
  Live: [],
  Restart: [],
  Voice: "alba",
  CanPreview: true,
  PreviewMaxRunes: 240,
  Wakeword: { Enabled: false, Addr: "", Models: [], Note: "" },
  ...over,
});

/** The page also mounts the Equalizer, which loads its own view. */
const serve = (settings: SettingsView, extra: Parameters<typeof stubFetch>[0] = {}) =>
  stubFetch({
    "/api/ui/settings": () => json(settings),
    "/api/ui/equalizer": () =>
      json({ Profile: { Preset: "Flat", Bands: Array(10).fill(0) }, Custom: { Preset: "Custom", Bands: Array(10).fill(0) }, Error: "" }),
    ...extra,
  });

const openRow = (name: RegExp) => userEvent.click(screen.getByRole("button", { name }));

const preview = (name: RegExp = /Wording preview for Filler/i) => screen.getByRole("group", { name });

/** Every spoken row names its label in the button, so two open rows differ. */
const playButton = (label: string) => screen.getByRole("button", { name: `Play the preview of ${label}` });
const stopButton = (label: string) => screen.getByRole("button", { name: `Stop the preview of ${label}` });
const blankNote = "A blank draft can't be saved, so there's nothing to preview.";

it("previews the effective value and follows a new draft", async () => {
  serve(view([spoken({ Edit: "One moment.", Display: "One moment." })]));
  render(<SettingsPage />);
  await userEvent.click(await screen.findByRole("button", { name: /Agent wording/ }));
  await openRow(/agent\.filler/);

  expect(within(preview()).getByText("One moment.")).toBeTruthy();
  expect(within(preview()).getByText("Preview · alba")).toBeTruthy();

  const editor = screen.getByRole("textbox", { name: "Filler" });
  await userEvent.clear(editor);
  await userEvent.type(editor, "Two moments.");
  expect(within(preview()).getByText("Two moments.")).toBeTruthy();
});

it("plays the unsaved draft through the preview action", async () => {
  const draft = "I'm here & ready? yes";
  const url = "/settings/wording/preview?text=" + encodeURIComponent(draft);
  const played: string[] = [];
  stubAudio((src) => played.push(src));
  let requested = "";
  // The harness normalises a URL before matching, so routes are keyed by the
  // serialised form while the wrapper records the exact string the app fetched.
  const route = new URL(url, "http://localhost");
  const stub = serve(view([spoken()]), { [route.pathname + route.search]: () => wav() });
  globalThis.fetch = (input, init) => {
    requested = String(input);
    return stub(input, init);
  };
  render(<SettingsPage />);
  await userEvent.click(await screen.findByRole("button", { name: /Agent wording/ }));
  await openRow(/agent\.filler/);

  fireEvent.change(screen.getByRole("textbox", { name: "Filler" }), { target: { value: draft } });
  expect(within(preview()).getByText(draft)).toBeTruthy();
  await userEvent.click(playButton("Filler"));

  await waitFor(() => expect(played).toHaveLength(1));
  expect(requested).toBe(url);
});

it("stops the clip when the draft changes", async () => {
  const played: string[] = [];
  const paused = vi.fn();
  stubAudio((src) => played.push(src), paused);
  serve(view([spoken({ Edit: "One moment." })]), { "/settings/wording/preview": () => wav() });
  render(<SettingsPage />);
  await userEvent.click(await screen.findByRole("button", { name: /Agent wording/ }));
  await openRow(/agent\.filler/);

  await userEvent.click(playButton("Filler"));
  await waitFor(() => expect(played).toHaveLength(1));
  expect(stopButton("Filler")).toBeTruthy();

  fireEvent.change(screen.getByRole("textbox", { name: "Filler" }), { target: { value: "New words." } });
  await waitFor(() => expect(paused).toHaveBeenCalled());
  expect(playButton("Filler")).toBeTruthy();
});

it("reports a refused preview inline without playing anything", async () => {
  const played: string[] = [];
  stubAudio((src) => played.push(src));
  let status = 400;
  let message = "Wording previews are limited to 240 characters.";
  serve(view([spoken({ Edit: "One moment." })]), {
    "/settings/wording/preview": () => new Response(message, { status }),
  });
  render(<SettingsPage />);
  await userEvent.click(await screen.findByRole("button", { name: /Agent wording/ }));
  await openRow(/agent\.filler/);

  await userEvent.click(playButton("Filler"));
  expect(await within(preview()).findByText(message)).toBeTruthy();

  status = 502;
  message = "Wording preview unavailable.";
  await userEvent.click(playButton("Filler"));
  expect(await within(preview()).findByText(message)).toBeTruthy();
  expect(played).toHaveLength(0);
});

it("keeps the text preview but drops Play without the speech service", async () => {
  serve(view([spoken({ Edit: "One moment." })], { CanPreview: false }));
  render(<SettingsPage />);
  await userEvent.click(await screen.findByRole("button", { name: /Agent wording/ }));
  await openRow(/agent\.filler/);

  expect(within(preview()).getByText("One moment.")).toBeTruthy();
  expect(screen.queryByRole("button", { name: /Play the preview/ })).toBeNull();
  expect(within(preview()).getByText("Preview needs the speech service.")).toBeTruthy();
});

it("shows match prefixes as a sample request and never offers Play", async () => {
  const prefixes = setting({
    Key: "agent.request_prefixes",
    Anchor: "agent-request-prefixes",
    Label: "Agent request prefixes",
    FormKind: "lines",
    Edit: "Hey Agent?\nask the agent.\nHey   Agent",
    PreviewKind: "match",
  });
  serve(view([prefixes]));
  render(<SettingsPage />);
  await userEvent.click(await screen.findByRole("button", { name: /Agent wording/ }));
  await openRow(/agent\.request_prefixes/);

  const block = preview(/Wording preview for Agent request prefixes/i);
  expect(within(block).getByText("hey agent, what time is it")).toBeTruthy();
  expect(within(block).getByText("ask the agent, what time is it")).toBeTruthy();
  expect(within(block).getAllByText("→ Agent path")).toHaveLength(2);
  expect(screen.queryByRole("button", { name: /Play the preview/ })).toBeNull();
});

it("disables Play for a blank draft and for one over the limit", async () => {
  serve(view([spoken({ Edit: "One moment." })]));
  render(<SettingsPage />);
  await userEvent.click(await screen.findByRole("button", { name: /Agent wording/ }));
  await openRow(/agent\.filler/);

  const editor = screen.getByRole("textbox", { name: "Filler" });
  await userEvent.clear(editor);
  const play = () => playButton("Filler");
  expect(play().hasAttribute("disabled")).toBe(true);
  const blank = within(preview()).getByText(blankNote);
  expect(play().getAttribute("aria-describedby")).toBe(blank.id);

  fireEvent.change(editor, { target: { value: "a".repeat(241) } });
  expect(play().hasAttribute("disabled")).toBe(true);
  expect(within(preview()).getByText("241 / 240 characters")).toBeTruthy();
  const limit = within(preview()).getByText("Too long for a preview; shorten the wording.");
  expect(play().getAttribute("aria-describedby")).toBe(limit.id);
});

it("refuses a blank draft for the unavailable message too", async () => {
  const unavailable = spoken({
    Key: "agent.unavailable_message",
    Anchor: "agent-unavailable",
    Label: "Unavailable message",
    Edit: "Try again in a moment.",
  });
  serve(view([unavailable]));
  render(<SettingsPage />);
  await userEvent.click(await screen.findByRole("button", { name: /Agent wording/ }));
  await openRow(/agent\.unavailable_message/);
  await userEvent.clear(screen.getByRole("textbox", { name: "Unavailable message" }));

  const block = preview(/Wording preview for Unavailable message/i);
  expect(within(block).getByText(blankNote)).toBeTruthy();
  // Neither the stored line nor a client-side "default" is claimed as spoken.
  expect(within(block).queryByText("Try again in a moment.")).toBeNull();
  expect(within(block).queryByText("Sorry, I can't answer that right now.")).toBeNull();
  expect(playButton("Unavailable message").hasAttribute("disabled")).toBe(true);
});

it("renders no preview for settings without a preview kind", async () => {
  const model = setting({ Key: "agent.model", Anchor: "agent-model", Label: "Agent model", Edit: "gpt-4" });
  const control = setting({ Key: "tools.clock.control_phrases", Anchor: "clock-control", Label: "Control phrases", FormKind: "lines", Edit: "time" });
  serve(
    view([model], {
      ToolWording: [{ Name: "clock", Anchor: "tool-clock", Control: control, Examples: null }],
    }),
  );
  render(<SettingsPage />);
  await userEvent.click(await screen.findByRole("button", { name: /Agent wording/ }));
  await openRow(/agent\.model/);

  expect(screen.getByRole("textbox", { name: "Agent model" })).toBeTruthy();
  expect(screen.queryByRole("group", { name: /Wording preview/ })).toBeNull();

  await userEvent.click(screen.getByRole("button", { name: /Per-Tool wording/ }));
  await openRow(/tools\.clock\.control_phrases/);
  expect(screen.queryByRole("group", { name: /Wording preview/ })).toBeNull();
});
