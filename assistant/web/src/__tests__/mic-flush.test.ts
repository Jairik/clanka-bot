// Stop must wait for the audio thread, including when delivery takes longer than 40 ms.
import { afterEach, expect, it, vi } from "vitest";
import { startRecording } from "../mic";

// Restore browser globals and timers after each device-boundary test.
afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

// Simulate delayed worklet messages without requiring microphone hardware.
it.each([true, false])("waits for acknowledgement or reports a flush timeout (acknowledged: %s)", async (acknowledge) => {
  vi.useFakeTimers();
  const disconnect = vi.fn();
  const close = vi.fn();
  const stopTrack = vi.fn();
  const port = { onmessage: null as ((event: { data: unknown }) => void) | null, postMessage: vi.fn() };
  vi.stubGlobal("navigator", { mediaDevices: { getUserMedia: vi.fn().mockResolvedValue({ getTracks: () => [{ stop: stopTrack }] }) } });
  vi.stubGlobal("AudioContext", class {
    sampleRate = 16000;
    audioWorklet = { addModule: vi.fn().mockResolvedValue(undefined) };
    destination = {};
    close = close;
    createMediaStreamSource = () => ({ connect: vi.fn(), disconnect });
    createGain = () => ({ gain: { value: 0 }, connect: vi.fn() });
  });
  vi.stubGlobal("AudioWorkletNode", class {
    port = port;
    connect = (sink: unknown) => sink;
    disconnect = vi.fn();
  });
  const recording = await startRecording(vi.fn());
  let finished = false;
  const result = recording.stop().then((pcm) => { finished = true; return pcm; });
  expect(port.postMessage).toHaveBeenCalledWith({ type: "flush" });
  await vi.advanceTimersByTimeAsync(100);
  expect(finished).toBe(false);
  expect(close).not.toHaveBeenCalled();
  if (!acknowledge) {
    const rejection = expect(result).rejects.toThrow("Microphone flush timed out");
    await vi.advanceTimersByTimeAsync(2000);
    await rejection;
    expect(close).toHaveBeenCalledOnce();
    expect(stopTrack).toHaveBeenCalledOnce();
    return;
  }
  port.onmessage?.({ data: new Float32Array([0.5, 0.25, -0.5]) });
  port.onmessage?.({ data: { type: "flushed" } });
  expect((await result).byteLength).toBe(6);
  expect(close).toHaveBeenCalledOnce();
  expect(stopTrack).toHaveBeenCalledOnce();
});
