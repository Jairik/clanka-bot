// Exercise Pi default capture, automatic progression, pause and partial-save recovery.
import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { SampleBatch } from "../pages/SampleBatch";
import type { VoiceSamplesView } from "../types";

const data = { PiRecordingAvailable: true, Phrase: "clanker", MaxTakes: 100, PositiveCount: 0, NegativeCount: 0, UploadPositive: "/keep-positive", UploadNegative: "/keep-negative" } as VoiceSamplesView;
const pcm = new Int16Array(48000);
for (let i = 6400; i < 16000; i++) pcm[i] = Math.sin(i * 0.17) * 6000;
const capture = () => ({ ok: true, arrayBuffer: async () => pcm.buffer });

beforeEach(() => {
  vi.useFakeTimers();
  vi.stubGlobal("fetch", vi.fn().mockImplementation(async () => capture()));
  URL.createObjectURL = vi.fn(() => "blob:take");
  URL.revokeObjectURL = vi.fn();
});
afterEach(() => { vi.useRealTimers(); vi.unstubAllGlobals(); });

it("defaults to Pi and captures two takes without per-take confirmation", async () => {
  const saved = vi.fn().mockResolvedValue(undefined);
  render(<SampleBatch data={data} kind="positive" onSaved={saved} onBusy={vi.fn()} onDirty={vi.fn()} />);
  expect((screen.getByLabelText("Batch microphone") as HTMLSelectElement).value).toBe("pi");
  fireEvent.change(screen.getByLabelText("Batch take count"), { target: { value: "2" } });
  fireEvent.click(screen.getByText("Start batch"));
  await act(async () => { await vi.advanceTimersByTimeAsync(1500); });
  expect(fetch).toHaveBeenCalledTimes(2);
  expect(screen.getAllByText(/No acoustic issues found/)).toHaveLength(2);
  fireEvent.click(screen.getByText("Save batch to Pi"));
  await act(async () => {});
  expect(fetch).toHaveBeenCalledTimes(4);
  expect(saved).toHaveBeenCalledOnce();
});

it("pauses before the cue without requesting or keeping audio", async () => {
  render(<SampleBatch data={data} kind="negative" onSaved={vi.fn()} onBusy={vi.fn()} onDirty={vi.fn()} />);
  expect((screen.getByLabelText("Batch take count") as HTMLInputElement).value).toBe("5");
  fireEvent.click(screen.getByText("Start batch"));
  fireEvent.click(screen.getByText("Pause / stop take"));
  await act(async () => { await vi.advanceTimersByTimeAsync(800); });
  expect(fetch).not.toHaveBeenCalled();
  expect((screen.getByText("Continue batch") as HTMLButtonElement).disabled).toBe(false);
});

it("retries only unsaved takes after a partial save failure", async () => {
  const saved = vi.fn().mockResolvedValue(undefined);
  render(<SampleBatch data={data} kind="positive" onSaved={saved} onBusy={vi.fn()} onDirty={vi.fn()} />);
  fireEvent.change(screen.getByLabelText("Batch take count"), { target: { value: "2" } });
  fireEvent.click(screen.getByText("Start batch"));
  await act(async () => { await vi.advanceTimersByTimeAsync(1500); });
  vi.mocked(fetch).mockResolvedValueOnce({ ok: true } as Response).mockResolvedValueOnce({ ok: false, json: async () => ({ Error: "Store unavailable" }) } as Response);
  fireEvent.click(screen.getByText("Save batch to Pi"));
  await act(async () => {});
  expect(screen.getByRole("alert").textContent).toBe("Store unavailable");
  expect(fetch).toHaveBeenCalledTimes(4);
  fireEvent.click(screen.getByText("Save batch to Pi"));
  await act(async () => {});
  expect(fetch).toHaveBeenCalledTimes(5);
  expect(saved).toHaveBeenCalledOnce();
});

it("aborts a Pi take on pause and retains it as pending for continue", async () => {
  let aborted = false;
  vi.mocked(fetch).mockImplementation((_url, init) => new Promise((_resolve, reject) => {
    init?.signal?.addEventListener("abort", () => { aborted = true; reject(new DOMException("Aborted", "AbortError")); });
  }));
  render(<SampleBatch data={data} kind="positive" onSaved={vi.fn()} onBusy={vi.fn()} onDirty={vi.fn()} />);
  fireEvent.click(screen.getByText("Start batch"));
  await act(async () => { await vi.advanceTimersByTimeAsync(800); });
  fireEvent.click(screen.getByText("Pause / stop take"));
  await act(async () => {});
  expect(aborted).toBe(true);
  expect((screen.getByText("Continue batch") as HTMLButtonElement).disabled).toBe(false);
});
