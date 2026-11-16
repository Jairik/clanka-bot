// Box-filter downsample to 16 kHz mono s16le PCM, the format the STT service
// expects. Pure arithmetic, kept separate from the browser mic capture so it
// can be tested without a device or AudioContext.
export const TARGET_RATE = 16000;

/** Box-filter downsample (a cheap low-pass that is plenty for speech) and quantize. */
export function toPCM16(chunks: Float32Array[], inRate: number, maxSeconds: number): ArrayBuffer {
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
