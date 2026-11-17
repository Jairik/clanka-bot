// Drives the recorder worklet processor with render quanta (128 samples each).
// Verifies batching into 1024-sample blocks (8 quanta) and flushing on disconnect.
import { beforeEach, describe, expect, it, vi } from "vitest";

class FakeAudioWorkletProcessor {
  port = {
    postMessage: vi.fn(),
  };
}

(globalThis as unknown as { AudioWorkletProcessor: typeof FakeAudioWorkletProcessor }).AudioWorkletProcessor =
  FakeAudioWorkletProcessor;

const { Recorder } = await import("../recorder-worklet.js");

/** Creates a 128-sample Float32Array quantum filled with sample values. */
function quantum(fill = 0.5): Float32Array {
  return new Float32Array(128).fill(fill);
}

describe("Recorder AudioWorkletProcessor", () => {
  let recorder: InstanceType<typeof Recorder>;
  let postMessage: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    recorder = new Recorder();
    postMessage = (recorder as unknown as { port: { postMessage: ReturnType<typeof vi.fn> } }).port.postMessage;
    postMessage.mockClear();
  });

  it("accumulates 8 quanta (1024 samples) into exactly one posted message", () => {
    // 7 quanta (896 samples): no message posted yet.
    for (let i = 0; i < 7; i++) {
      const active = recorder.process([[quantum(0.1 * (i + 1))]]);
      expect(active).toBe(true);
      expect(postMessage).not.toHaveBeenCalled();
    }

    // 8th quantum (reaching 1024 samples): exactly one message posted.
    const active = recorder.process([[quantum(0.8)]]);
    expect(active).toBe(true);
    expect(postMessage).toHaveBeenCalledOnce();

    const posted = postMessage.mock.calls[0][0] as Float32Array;
    expect(posted).toBeInstanceOf(Float32Array);
    expect(posted.length).toBe(1024);
    // Verify first and last quantum samples in the 1024-sample block.
    expect(posted[0]).toBeCloseTo(0.1);
    expect(posted[127]).toBeCloseTo(0.1);
    expect(posted[1023]).toBeCloseTo(0.8);
  });

  it("hands off the filled block so subsequent writes do not mutate the posted buffer", () => {
    for (let i = 0; i < 8; i++) {
      recorder.process([[quantum(1.0)]]);
    }
    expect(postMessage).toHaveBeenCalledOnce();
    const firstBlock = postMessage.mock.calls[0][0] as Float32Array;
    expect(firstBlock[0]).toBeCloseTo(1.0);

    // 9th quantum writes into a fresh buffer with new values.
    recorder.process([[quantum(0.2)]]);
    // The previous block must remain untouched.
    expect(firstBlock[0]).toBeCloseTo(1.0);
  });

  it("flushes a partial block when the node is disconnected", () => {
    // Drive with 3 quanta (3 * 128 = 384 samples).
    for (let i = 0; i < 3; i++) {
      recorder.process([[quantum(0.5)]]);
    }
    expect(postMessage).not.toHaveBeenCalled();

    // Disconnect simulated by process with empty channel / inputs.
    const active = recorder.process([]);
    expect(active).toBe(false);
    expect(postMessage).toHaveBeenCalledOnce();

    const partial = postMessage.mock.calls[0][0] as Float32Array;
    expect(partial).toBeInstanceOf(Float32Array);
    expect(partial.length).toBe(384);
    expect(partial[0]).toBeCloseTo(0.5);
    expect(partial[383]).toBeCloseTo(0.5);

    // Further empty calls do not post again.
    postMessage.mockClear();
    recorder.process([]);
    expect(postMessage).not.toHaveBeenCalled();
  });

  it("does not flush an empty buffer if disconnected before any audio arrives", () => {
    const active = recorder.process([]);
    expect(active).toBe(true);
    expect(postMessage).not.toHaveBeenCalled();
  });
});
