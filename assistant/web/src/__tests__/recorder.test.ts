// The mic state machine: start, stop, cancel, the cap and the double-press guard.
import { act, renderHook } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { useRecorder } from "../recorder";

vi.mock("../mic", () => ({
  MIC_MAX_SECONDS: 30,
  micSupported: () => true,
  startRecording: vi.fn(),
}));

const { startRecording } = await import("../mic");
const mockStart = vi.mocked(startRecording);

const recording = () => ({ stop: vi.fn().mockResolvedValue(new ArrayBuffer(4)), cancel: vi.fn() });

beforeEach(() => {
  mockStart.mockReset();
  vi.useRealTimers();
});

describe("useRecorder", () => {
  it("records, then uploads the PCM on stop", async () => {
    const rec = recording();
    mockStart.mockResolvedValue(rec);
    const onSend = vi.fn();
    const { result } = renderHook(() => useRecorder({ onSend }));

    await act(async () => result.current.start());
    expect(result.current.phase).toBe("recording");
    expect(result.current.elapsed).toBe(0);

    await act(async () => result.current.stop());
    expect(rec.stop).toHaveBeenCalledOnce();
    expect(rec.cancel).not.toHaveBeenCalled();
    expect(onSend).toHaveBeenCalledOnce();
    expect(result.current.phase).toBe("idle");
  });

  it("cancels without uploading", async () => {
    const rec = recording();
    mockStart.mockResolvedValue(rec);
    const onSend = vi.fn();
    const { result } = renderHook(() => useRecorder({ onSend }));

    await act(async () => result.current.start());
    act(() => result.current.cancel());

    expect(rec.cancel).toHaveBeenCalledOnce();
    expect(rec.stop).not.toHaveBeenCalled();
    expect(onSend).not.toHaveBeenCalled();
    expect(result.current.phase).toBe("idle");
  });

  it("ignores a second start while the first is still asking for permission", async () => {
    let release!: (r: ReturnType<typeof recording>) => void;
    mockStart.mockImplementation(() => new Promise((resolve) => (release = resolve)));
    const { result } = renderHook(() => useRecorder({ onSend: vi.fn() }));

    // The regression: getUserMedia is still pending, so a second press used to
    // start a second capture and orphan the first stream.
    act(() => {
      result.current.start();
      result.current.start();
    });
    expect(mockStart).toHaveBeenCalledTimes(1);

    await act(async () => release(recording()));
    expect(result.current.phase).toBe("recording");
  });

  it("stops and sends itself at the 30 second cap", async () => {
    const rec = recording();
    mockStart.mockResolvedValue(rec);
    const onSend = vi.fn();
    vi.useFakeTimers();
    const { result } = renderHook(() => useRecorder({ onSend }));

    await act(async () => result.current.start());
    expect(result.current.phase).toBe("recording");

    act(() => {
      vi.setSystemTime(Date.now() + 30_000);
      vi.advanceTimersByTime(200);
    });
    await act(async () => {});

    expect(rec.stop).toHaveBeenCalledOnce();
    expect(onSend).toHaveBeenCalledOnce();
    expect(result.current.phase).toBe("idle");
  });

  it("shows the elapsed seconds while recording", async () => {
    mockStart.mockResolvedValue(recording());
    vi.useFakeTimers();
    const { result } = renderHook(() => useRecorder({ onSend: vi.fn() }));

    await act(async () => result.current.start());
    act(() => {
      vi.setSystemTime(Date.now() + 4_000);
      vi.advanceTimersByTime(200);
    });
    expect(result.current.elapsed).toBe(4);
  });

  it("names a blocked microphone", async () => {
    mockStart.mockRejectedValue(new DOMException("denied", "NotAllowedError"));
    const { result } = renderHook(() => useRecorder({ onSend: vi.fn() }));

    await act(async () => result.current.start());
    expect(result.current.phase).toBe("error");
    expect(result.current.error).toBe("Microphone access was blocked.");
  });

  it("names any other start failure without leaking the raw error", async () => {
    mockStart.mockRejectedValue(new Error("no device"));
    const { result } = renderHook(() => useRecorder({ onSend: vi.fn() }));

    await act(async () => result.current.start());
    expect(result.current.phase).toBe("error");
    expect(result.current.error).toBe("Could not start the microphone.");
  });

  it("releases the microphone when the page goes away", async () => {
    const rec = recording();
    mockStart.mockResolvedValue(rec);
    const { result, unmount } = renderHook(() => useRecorder({ onSend: vi.fn() }));

    await act(async () => result.current.start());
    unmount();
    expect(rec.cancel).toHaveBeenCalledOnce();
  });
});
