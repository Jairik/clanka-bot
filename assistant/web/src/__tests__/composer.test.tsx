// The composer's recording states, driven through the real component.
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { Composer } from "../chat";

vi.mock("../mic", () => ({
  MIC_MAX_SECONDS: 30,
  micSupported: vi.fn(() => true),
  startRecording: vi.fn(),
}));

const { micSupported, startRecording } = await import("../mic");
const mockStart = vi.mocked(startRecording);
const mockSupported = vi.mocked(micSupported);

import { TOO_SHORT_ERROR } from "../recorder";
import { emptyActivity, json, Providers, session, stubFetch } from "./harness";

const recording = () => ({ stop: vi.fn().mockResolvedValue(new ArrayBuffer(4)), cancel: vi.fn() });

beforeEach(() => {
  mockStart.mockReset();
  mockSupported.mockReset();
  mockSupported.mockReturnValue(true);
});

function routes(onAudio?: (init?: RequestInit) => void) {
  return {
    "/api/ui/session": () => json(session()),
    "/api/ui/activity": () => json(emptyActivity()),
    "/api/ui/ask/audio": (init?: RequestInit) => {
      onAudio?.(init);
      return json(emptyActivity());
    },
    "/activity/ask": () => json(emptyActivity()),
  };
}

describe("Composer", () => {
  it("shows the recording state and sends the utterance on stop", async () => {
    const rec = recording();
    mockStart.mockResolvedValue(rec);
    let audio: RequestInit | undefined;
    stubFetch(routes((init) => (audio = init)));
    const user = userEvent.setup();

    render(
      <Providers>
        <Composer />
      </Providers>,
    );
    await user.click(await screen.findByRole("button", { name: "Talk" }));

    expect(startRecording).toHaveBeenCalledOnce();
    expect(await screen.findByRole("button", { name: "Cancel recording" })).toBeTruthy();
    const timer = screen.getByRole("timer");
    expect(timer.textContent).toContain("0s / 30s");
    expect(timer.getAttribute("aria-label")).toContain("Recording 0 of 30 seconds");

    await user.click(screen.getByRole("button", { name: "Stop and send" }));
    expect(rec.stop).toHaveBeenCalledOnce();
    expect(rec.cancel).not.toHaveBeenCalled();
    expect(audio?.headers).toMatchObject({ "Content-Type": "audio/L16; rate=16000; channels=1; endianness=little-endian" });
    expect(await screen.findByRole("button", { name: "Talk" })).toBeTruthy();
  });

  it("discards the utterance on cancel", async () => {
    const rec = recording();
    mockStart.mockResolvedValue(rec);
    stubFetch(routes());
    const user = userEvent.setup();

    render(
      <Providers>
        <Composer />
      </Providers>,
    );
    await user.click(await screen.findByRole("button", { name: "Talk" }));
    await user.click(await screen.findByRole("button", { name: "Cancel recording" }));

    expect(rec.cancel).toHaveBeenCalledOnce();
    expect(rec.stop).not.toHaveBeenCalled();
    expect(await screen.findByRole("button", { name: "Talk" })).toBeTruthy();
  });

  it("shows a blocked microphone as an error, not a silent no-op", async () => {
    mockStart.mockRejectedValue(new DOMException("denied", "NotAllowedError"));
    stubFetch(routes());
    const user = userEvent.setup();

    render(
      <Providers>
        <Composer />
      </Providers>,
    );
    await user.click(await screen.findByRole("button", { name: "Talk" }));

    expect(await screen.findByText("Microphone access was blocked.")).toBeTruthy();
  });

  it("holds to talk on touch: press starts, release sends", async () => {
    const rec = recording();
    mockStart.mockResolvedValue(rec);
    let audio: RequestInit | undefined;
    stubFetch(routes((init) => (audio = init)));
    render(
      <Providers>
        <Composer />
      </Providers>,
    );

    const talk = await screen.findByRole("button", { name: "Talk" });
    fireEvent.pointerDown(talk, { pointerType: "touch" });
    expect(startRecording).toHaveBeenCalledTimes(1);
    expect(await screen.findByRole("button", { name: "Stop and send" })).toBeTruthy();

    fireEvent.pointerUp(window);
    await waitFor(() => expect(rec.stop).toHaveBeenCalledOnce());
    expect(rec.cancel).not.toHaveBeenCalled();
    expect(audio?.headers).toMatchObject({ "Content-Type": expect.stringContaining("audio/L16") });
  });

  it("shows the wait and treats a release before the capture as too short", async () => {
    let release!: (r: ReturnType<typeof recording>) => void;
    mockStart.mockImplementation(() => new Promise((resolve) => (release = resolve)));
    const rec = recording();
    const onAudio = routes();
    stubFetch(onAudio);
    render(
      <Providers>
        <Composer />
      </Providers>,
    );

    const talk = await screen.findByRole("button", { name: "Talk" });
    fireEvent.pointerDown(talk, { pointerType: "touch" });
    expect(screen.getByRole("status").textContent).toContain("Starting the microphone…");

    fireEvent.pointerUp(window);
    await act(async () => release(rec));
    expect(rec.cancel).toHaveBeenCalledOnce();
    expect(rec.stop).not.toHaveBeenCalled();
    expect(await screen.findByText(TOO_SHORT_ERROR)).toBeTruthy();
  });

  it("names an insecure context instead of hiding the mic silently", async () => {
    mockSupported.mockReturnValue(false);
    stubFetch(routes());
    render(
      <Providers>
        <Composer />
      </Providers>,
    );
    expect(await screen.findByText(/The microphone needs HTTPS/)).toBeTruthy();
  });

  it("hides the composer when typed requests are not wired", async () => {
    stubFetch({
      "/api/ui/session": () => json(session({ Typed: false })),
      "/api/ui/activity": () => json(emptyActivity()),
    });

    render(
      <Providers>
        <Composer />
      </Providers>,
    );
    expect(await screen.findByText("Typed requests are not wired in this deployment.")).toBeTruthy();
  });
});
