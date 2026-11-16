// The 16 kHz downsampler is pure arithmetic; these tests run it directly so an
// off-by-one in the box filter, a missing clamp or a bad tail read is caught.
import { describe, expect, it } from "vitest";
import { toPCM16 } from "../pcm";

const chunk = (n: number, fill = 0) => new Float32Array(n).fill(fill);
const view = (buf: ArrayBuffer) => new DataView(buf);

describe("toPCM16", () => {
  it("passes 16 kHz through at one sample per output", () => {
    expect(view(toPCM16([chunk(16000)], 16000, 30)).byteLength).toBe(16000 * 2);
  });

  it("downsamples 48 kHz to a third of the concatenated input length", () => {
    expect(view(toPCM16([chunk(48000)], 48000, 30)).byteLength).toBe(16000 * 2);
    // Chunks are concatenated before conversion, so two halves behave the same.
    expect(view(toPCM16([chunk(24000, 0.5), chunk(24000, 0.5)], 48000, 30)).byteLength).toBe(16000 * 2);
  });

  it("handles 44.1 kHz's fractional ratio without an undefined tail read", () => {
    const out = view(toPCM16([chunk(44100, 0.5)], 44100, 30));
    expect(out.byteLength).toBe(16000 * 2);
    // Every window is filled with 0.5, so no sample may be zero. A read past
    // the end of the input would yield undefined -> NaN -> a silent sample.
    for (let i = 0; i < 16000; i++) expect(out.getInt16(i * 2, true)).toBeGreaterThan(0);
  });

  it("keeps all-zero input silent", () => {
    const out = view(toPCM16([chunk(8000), chunk(8000)], 16000, 30));
    expect(out.byteLength).toBe(16000 * 2);
    for (let i = 0; i < 16000; i++) expect(out.getInt16(i * 2, true)).toBe(0);
  });

  it("maps +1.0 to 32767 and -1.0 to -32768, clamping beyond the range", () => {
    expect(view(toPCM16([chunk(16000, 1)], 16000, 30)).getInt16(0, true)).toBe(32767);
    expect(view(toPCM16([chunk(16000, -1)], 16000, 30)).getInt16(0, true)).toBe(-32768);
    expect(view(toPCM16([chunk(16000, 2)], 16000, 30)).getInt16(0, true)).toBe(32767);
    expect(view(toPCM16([chunk(16000, -2)], 16000, 30)).getInt16(0, true)).toBe(-32768);
  });

  it("truncates the output to maxSeconds", () => {
    const out = view(toPCM16([chunk(48000 * 5)], 48000, 2));
    expect(out.byteLength).toBe(16000 * 2 * 2);
  });

  it("yields a valid buffer for a single short partial chunk", () => {
    const out = view(toPCM16([chunk(10, 0.5)], 48000, 30));
    expect(out.byteLength).toBe(3 * 2); // floor(10 / 3) == 3 samples
    expect(out.getInt16(0, true)).toBeGreaterThan(0);
  });

  it("keeps the a + 1 window guard even when the input rate is below 16 kHz", () => {
    // ratio = 0.5: for even i, floor((i+1) * ratio) === a, so without the
    // max(a + 1, ...) guard the window would be empty and the sample zero.
    const out = view(toPCM16([chunk(8000, 0.5)], 8000, 30));
    expect(out.byteLength).toBe(16000 * 2);
    for (let i = 0; i < 16000; i++) expect(out.getInt16(i * 2, true)).toBeGreaterThan(0);
  });
});
