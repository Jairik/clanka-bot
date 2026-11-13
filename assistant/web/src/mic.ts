// Browser mic capture for the Dashboard. The whole utterance is recorded, then
// resampled to 16 kHz mono 16-bit PCM, the format the STT service expects, so
// the server never decodes a codec. Needs a secure context (HTTPS or
// localhost); `tailscale serve` provides HTTPS on the tailnet.
import workletUrl from "./recorder-worklet.js?url";

export const MIC_MAX_SECONDS = 30;
const TARGET_RATE = 16000;

export const micSupported = () => typeof window !== "undefined" && window.isSecureContext && !!navigator.mediaDevices?.getUserMedia;

export type Recording = {
  /** Resolves with 16 kHz s16le PCM. */
  stop: () => Promise<ArrayBuffer>;
  cancel: () => void;
};

export type CaptureOptions = {
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
  } catch (e) {
    stream.getTracks().forEach((t) => t.stop());
    void ctx.close();
    throw e;
  }
  const source = ctx.createMediaStreamSource(stream);
  const node = new AudioWorkletNode(ctx, "clanka-recorder");
  const chunks: Float32Array[] = [];
  node.port.onmessage = (e: MessageEvent<Float32Array>) => {
    chunks.push(e.data);
    let sum = 0;
    for (const v of e.data) sum += v * v;
    onLevel(Math.sqrt(sum / e.data.length));
  };
  source.connect(node);
  // A worklet only runs while connected to the graph; a muted gain keeps it
  // pulled without echoing the mic to the speakers.
  const sink = ctx.createGain();
  sink.gain.value = 0;
  node.connect(sink).connect(ctx.destination);

  const release = () => {
    node.port.onmessage = null;
    source.disconnect();
    node.disconnect();
    stream.getTracks().forEach((t) => t.stop());
    void ctx.close();
  };
  return {
    cancel: release,
    stop: async () => {
      const rate = ctx.sampleRate;
      release();
      return toPCM16(chunks, rate, options.maxSeconds ?? MIC_MAX_SECONDS);
    },
  };
}

/** Box-filter downsample (a cheap low-pass that is plenty for speech) and quantize. */
function toPCM16(chunks: Float32Array[], inRate: number, maxSeconds: number): ArrayBuffer {
  const total = chunks.reduce((n, c) => n + c.length, 0);
  const input = new Float32Array(total);
  let o = 0;
  for (const c of chunks) {
    input.set(c, o);
    o += c.length;
  }
  const ratio = inRate / TARGET_RATE;
  const outLen = Math.min(Math.floor(total / ratio), TARGET_RATE * maxSeconds);
  const out = new DataView(new ArrayBuffer(outLen * 2));
  for (let i = 0; i < outLen; i++) {
    const a = Math.floor(i * ratio);
    const b = Math.max(a + 1, Math.floor((i + 1) * ratio));
    let s = 0;
    for (let j = a; j < b; j++) s += input[j];
    const v = Math.max(-1, Math.min(1, s / (b - a)));
    out.setInt16(i * 2, v < 0 ? v * 0x8000 : v * 0x7fff, true);
  }
  return out.buffer;
}
