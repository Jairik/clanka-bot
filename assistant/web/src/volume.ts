// The speaker bar's step maths, kept pure so the rounding is testable.

export const VOL_STEP = 5;

/** Round a slider value to the nearest 5% and clamp it to 0–100. */
export const snapVolume = (v: number) => Math.max(0, Math.min(100, Math.round(v / VOL_STEP) * VOL_STEP));
