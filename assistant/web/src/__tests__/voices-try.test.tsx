// The Voices page's "Try it out" box: owner text, and where the sample plays.
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { VoicesPage } from "../pages/Voices";
import type { VoiceRow, VoicesView } from "../types";
import { json, stubFetch } from "./harness";

const row = (over: Partial<VoiceRow> = {}): VoiceRow => ({
  Name: "alba",
  Anchor: "voice-0",
  Origin: "Built-in",
  Status: "Built-in",
  StatusClass: "",
  Revision: "r1",
  Validated: "",
  Added: "",
  Note: "",
  LastError: "",
  StaleNotice: "",
  Builtin: true,
  Ready: true,
  Stale: false,
  Active: true,
  Unvalidated: false,
  PreviewURL: "/voices/audio/alba",
  SpeakAction: "/voices/speak/alba",
  SelectAction: "/voices/active/alba",
  NoteAction: "/voices/note/alba",
  ...over,
});

const voices = (over: Partial<VoicesView> = {}): VoicesView => ({
  Owner: "owner@example.com",
  Page: "voices",
  Flash: "",
  Error: "",
  HasLibrary: true,
  CanUpload: false,
  CanPreview: true,
  CanPlayOnPi: true,
  SampleText: "This is a short sample of my voice.",
  MaxSampleText: 200,
  MaxUpload: "32 MB",
  Revision: "r1",
  ActiveName: "alba",
  Default: "alba",
  Rate: "1",
  RateSource: "default",
  RateLabel: "default",
  RateLower: "",
  RateEdit: "1",
  HasRate: false,
  Voices: [row()],
  ...over,
});

/** Records every Audio the page creates instead of playing it. */
function stubAudio() {
  const sources: string[] = [];
  vi.stubGlobal(
    "Audio",
    class {
      onended: (() => void) | null = null;
      onerror: (() => void) | null = null;
      constructor(src: string) {
        sources.push(src);
      }
      play() {
        return Promise.resolve();
      }
      pause() {}
    },
  );
  return sources;
}

describe("Voices: try it out", () => {
  it("plays the typed text in this browser by default", async () => {
    stubFetch({ "/api/ui/voices": () => json(voices()) });
    const sources = stubAudio();
    const user = userEvent.setup();
    render(<VoicesPage />);

    await user.type(await screen.findByRole("textbox", { name: "Sample text" }), "Hello there");
    await user.click(screen.getByRole("button", { name: /Say it as alba/ }));

    expect(sources).toEqual(["/voices/audio/alba?text=Hello%20there"]);
  });

  it("sends the text to the Pi speaker when that is chosen", async () => {
    let posted = "";
    stubFetch({
      "/api/ui/voices": () => json(voices()),
      "/voices/speak/alba": (init) => ((posted = String(init?.body ?? "")), json(voices({ Flash: "Queued. The sample plays on the Pi speaker between turns." }))),
    });
    const sources = stubAudio();
    const user = userEvent.setup();
    render(<VoicesPage />);

    await user.type(await screen.findByRole("textbox", { name: "Sample text" }), "Kitchen test");
    await user.click(screen.getByRole("radio", { name: /Pi speaker/ }));
    await user.click(screen.getByRole("button", { name: /Say it as alba/ }));

    expect(new URLSearchParams(posted).get("text")).toBe("Kitchen test");
    expect(await screen.findByText(/plays on the Pi speaker between turns/)).toBeTruthy();
    expect(sources).toEqual([]);
  });

  it("offers no Pi option when the server cannot reach the speaker", async () => {
    stubFetch({ "/api/ui/voices": () => json(voices({ CanPlayOnPi: false })) });
    render(<VoicesPage />);
    await screen.findByRole("textbox", { name: "Sample text" });
    expect(screen.queryByRole("radio", { name: /Pi speaker/ })).toBeNull();
  });
});
