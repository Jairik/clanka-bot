// Browser mic capture for the Dashboard. The whole utterance is recorded, then
// resampled to 16 kHz mono 16-bit PCM, the format the STT service expects, so
// the server never decodes a codec. Needs a secure context (HTTPS or
// localhost); `tailscale serve` provides HTTPS on the tailnet.
import workletUrl from "./recorder-worklet.js?url";
import { toPCM16 } from "./pcm";

export const MIC_MAX_SECONDS = 30;

export const micSupported = () => typeof window !== "undefined" && window.isSecureContext && !!navigator.mediaDevices?.getUserMedia;

export type Recording = {
  /** Resolves with 16 kHz s16le PCM. */
  stop: () => Promise<ArrayBuffer>;
  cancel: () => void;
};

export type CaptureOptions = {
  /** Optional local cue after device setup and before capturing samples. */
  beforeCapture?: () => Promise<void>;
  /** Max captured seconds; defaults to MIC_MAX_SECONDS. */
  maxSeconds?: number;
  /**
   * Processing the browser applies. Chat keeps the defaults; a training take
   * turns them off so the clip keeps the room and the mic's real level.
   */
  processing?: { echoCancellation: boolean; noiseSuppression: boolean; autoGainControl: boolean };
};

export async function startRecording(onLevel: (rms: number) => void, options: CaptureOptions = {}): Promise<Recording> {
  const processing = options.processing ?? { echoCancellation: true, noiseSuppression: true, autoGainControl: true };
  const stream = await navigator.mediaDevices.getUserMedia({
    audio: { channelCount: 1, ...processing },
  });
  const ctx = new AudioContext();
  try {
    await ctx.audioWorklet.addModule(workletUrl);
    await options.beforeCapture?.();
  } catch (e) {
    stream.getTracks().forEach((t) => t.stop());
    void ctx.close();
    throw e;
  }
  const source = ctx.createMediaStreamSource(stream);
  const node = new AudioWorkletNode(ctx, "clanka-recorder");
  const chunks: Float32Array[] = [];
  let flushed: (() => void) | undefined;
  // One port preserves ordering between audio blocks and the flush acknowledgement.
  const take = (e: MessageEvent<Float32Array | { type: "flushed" }>) => {
    if (!(e.data instanceof Float32Array)) {
      if (e.data.type === "flushed") flushed?.();
      return;
    }
    chunks.push(e.data);
    let sum = 0;
    for (const v of e.data) sum += v * v;
    onLevel(Math.sqrt(sum / e.data.length));
  };
  node.port.onmessage = take;
  source.connect(node);
  // A worklet only runs while connected to the graph; a muted gain keeps it
  // pulled without echoing the mic to the speakers.
  const sink = ctx.createGain();
  sink.gain.value = 0;
  node.connect(sink).connect(ctx.destination);

  const release = () => {
    node.port.onmessage = null;
    // Stop already disconnected the source so the tail could flush.
    try {
      source.disconnect();
    } catch {
      // The source was already disconnected.
    }
    node.disconnect();
    stream.getTracks().forEach((t) => t.stop());
    void ctx.close();
  };
  return {
    cancel: release,
    // Build PCM only after the audio thread confirms delivery of its final block.
    stop: async () => {
      const rate = ctx.sampleRate;
      try {
        await new Promise<void>((resolve, reject) => {
          const timeout = setTimeout(() => reject(new Error("Microphone flush timed out")), 2000);
          flushed = () => {
            clearTimeout(timeout);
            resolve();
          };
          source.disconnect();
          node.port.postMessage({ type: "flush" });
        });
        return toPCM16(chunks, rate, options.maxSeconds ?? MIC_MAX_SECONDS);
      } finally {
        release();
      }
    },
  };
}
