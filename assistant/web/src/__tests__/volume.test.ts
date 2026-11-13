// The speaker slider's 5% step maths.
import { describe, expect, it } from "vitest";
import { snapVolume, VOL_STEP } from "../volume";

describe("snapVolume", () => {
  it("steps in fives", () => {
    expect(VOL_STEP).toBe(5);
    expect(snapVolume(0)).toBe(0);
    expect(snapVolume(2)).toBe(0);
    expect(snapVolume(3)).toBe(5);
    expect(snapVolume(7)).toBe(5);
    expect(snapVolume(8)).toBe(10);
    expect(snapVolume(42)).toBe(40);
    expect(snapVolume(43)).toBe(45);
    expect(snapVolume(100)).toBe(100);
  });

  it("clamps a slider that runs past either end", () => {
    expect(snapVolume(-20)).toBe(0);
    expect(snapVolume(101)).toBe(100);
    expect(snapVolume(240)).toBe(100);
  });
});
