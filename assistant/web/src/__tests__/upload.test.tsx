// Upload ergonomics: limits, progress, cancel and retry without re-picking.
import { act, fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { parseSize, UploadRow, validateUpload } from "../upload";

class FakeXHR {
  static instances: FakeXHR[] = [];
  upload: { onprogress?: (e: { lengthComputable: boolean; loaded: number; total: number }) => void } = {};
  status = 200;
  responseText = JSON.stringify({ ok: true });
  onload: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onabort: (() => void) | null = null;
  open() {}
  setRequestHeader() {}
  send() {
    FakeXHR.instances.push(this);
  }
  abort() {
    this.onabort?.();
  }
}

beforeEach(() => {
  FakeXHR.instances = [];
  vi.stubGlobal("XMLHttpRequest", FakeXHR);
});

const wav = (name = "clip.wav", bytes = 8) => new File([new Uint8Array(bytes)], name, { type: "audio/wav" });

function view(over: Record<string, unknown> = {}) {
  const onView = vi.fn();
  const utils = render(<UploadRow action="/sounds/upload" maxUpload="10 MB" accept=".wav,audio/wav" label="Upload a sound" onView={onView} {...over} />);
  const input = utils.container.querySelector('input[type="file"]') as HTMLInputElement;
  return { onView, input, ...utils };
}

describe("upload helpers", () => {
  it("reads the server's size wording", () => {
    expect(parseSize("10 MB")).toBe(10 * 1024 ** 2);
    expect(parseSize("512 KB")).toBe(512 * 1024);
    expect(parseSize("2 GB")).toBe(2 * 1024 ** 3);
    expect(parseSize("nonsense")).toBe(0);
  });

  it("rejects the wrong type and an oversized file with the reason", () => {
    expect(validateUpload(wav(), ".wav,audio/wav", 10 * 1024 ** 2)).toBe("");
    expect(validateUpload(new File(["x"], "notes.txt", { type: "text/plain" }), ".wav,audio/wav", 10 * 1024 ** 2)).toContain(".wav");
    const big = new File([new Uint8Array(11 * 1024 ** 2)], "big.wav", { type: "audio/wav" });
    expect(validateUpload(big, ".wav,audio/wav", 10 * 1024 ** 2)).toContain("10 MB");
  });
});

describe("UploadRow", () => {
  it("refuses an oversized file before sending any bytes", async () => {
    const { input } = view();
    const big = new File([new Uint8Array(11 * 1024 ** 2)], "big.wav", { type: "audio/wav" });
    fireEvent.change(input, { target: { files: [big] } });

    expect((await screen.findByRole("alert")).textContent).toContain("10 MB");
    expect(FakeXHR.instances).toHaveLength(0);
  });

  it("shows progress, can be cancelled, and keeps the file for a retry", async () => {
    const { input, onView } = view();
    const user = userEvent.setup();
    fireEvent.change(input, { target: { files: [wav()] } });
    expect(screen.getByText("clip.wav")).toBeTruthy();

    await user.click(screen.getByRole("button", { name: "Upload" }));
    expect(FakeXHR.instances).toHaveLength(1);
    const first = FakeXHR.instances[0];

    const bar = screen.getByRole("progressbar");
    act(() => first.upload.onprogress?.({ lengthComputable: true, loaded: 50, total: 100 }));
    expect(bar.getAttribute("aria-valuenow")).toBe("50");

    await user.click(screen.getByRole("button", { name: "Cancel upload" }));
    expect(screen.getByRole("button", { name: "Upload" })).toBeTruthy();
    expect(onView).not.toHaveBeenCalled();

    // A failed attempt offers Retry, which re-sends the same file.
    await user.click(screen.getByRole("button", { name: "Upload" }));
    const second = FakeXHR.instances[1];
    second.status = 500;
    second.responseText = "That sound could not be stored.";
    act(() => second.onload?.());
    expect(screen.getByRole("alert").textContent).toContain("That sound could not be stored.");

    await user.click(screen.getByRole("button", { name: "Retry" }));
    expect(FakeXHR.instances).toHaveLength(3);
  });

  it("hands a successful response's view to the page", async () => {
    const { input, onView } = view();
    const user = userEvent.setup();
    fireEvent.change(input, { target: { files: [wav()] } });
    await user.click(screen.getByRole("button", { name: "Upload" }));

    const sent = FakeXHR.instances[0];
    sent.responseText = JSON.stringify({ HasLibrary: true });
    act(() => sent.onload?.());

    expect(onView).toHaveBeenCalledWith({ HasLibrary: true });
    expect(await screen.findByText("Uploaded.")).toBeTruthy();
  });
});
