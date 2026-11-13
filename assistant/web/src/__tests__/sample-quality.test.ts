// Quality decisions use raw acoustics rather than the detector being trained.
import { describe, expect, it } from "vitest";
import { assessSample } from "../sample-quality";

function speech(start = 0.4, end = 1.0, level = 0.2) {
  const pcm = new Int16Array(48000);
  for (let i = Math.round(start * 16000); i < Math.round(end * 16000); i++) pcm[i] = Math.sin(i * 0.17) * level * 32767;
  return pcm.buffer;
}

describe("sample quality", () => {
  it("allows a complete phrase with quiet margins", () => {
    const result = assessSample(speech());
    expect(result.warnings).toEqual([]);
    expect(result.blocked).toBe(false);
  });
  it("rejects silence and both capture boundaries", () => {
    expect(assessSample(new ArrayBuffer(96000)).blocked).toBe(true);
    expect(assessSample(speech(0, 0.7)).blocked).toBe(true);
    expect(assessSample(speech(2.4, 3)).blocked).toBe(true);
  });
  it("rejects speech the training cap would truncate", () => {
    const result = assessSample(speech(0.3, 2.5));
    expect(result.blocked).toBe(true);
    expect(result.warnings.join()).toContain("training clip length");
  });
  it("flags clipping and quiet speech for review", () => {
    expect(assessSample(speech(0.4, 1, 1)).warnings.join()).toContain("clipping");
    expect(assessSample(speech(0.4, 1, 0.01)).warnings.join()).toContain("quiet");
  });
});
