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
  // One handler for the live blocks and the tail flushed when recording stops.
  const take = (e: MessageEvent<Float32Array>) => {
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
    // Disconnect the source so the worklet posts its partial block, and keep
    // the handler up for a few quanta so that tail is in the buffer before
    // the PCM is built. A block that already filled posts nothing extra.
    stop: async () => {
      const rate = ctx.sampleRate;
      source.disconnect();
      await new Promise<void>((resolve) => setTimeout(resolve, 40));
      release();
      return toPCM16(chunks, rate, options.maxSeconds ?? MIC_MAX_SECONDS);
    },
  };
}
