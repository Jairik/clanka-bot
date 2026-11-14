// One recorded wake-phrase take → the exact training clip the desktop expects.
// The mic worklet already returns 16 kHz mono s16le; this trims to the speech
// with record.py's margins, caps at 1.9 s so openWakeWord's two-second
// augmentation buffer fits, and writes a canonical WAV header.
export const TRAINING_RATE = 16000;
export const MAX_CLIP_SECONDS = 1.9;
const FRAME = 0.02; // 20 ms RMS frames, the same gate record.py uses.
const PRE_SECONDS = 0.15;
const POST_SECONDS = 0.3;
const FLOOR_DBFS = -45;
const MARGIN_DB = 8;

export type TrainingClip = {
  /** Canonical 16 kHz mono 16-bit WAV. */
  wav: ArrayBuffer;
  seconds: number;
  /** False when nothing rose above the room noise. */
  foundSpeech: boolean;
  /** Peak sample magnitude, 0–1; below 0.02 the take is very quiet. */
  peak: number;
};

/** Trim and encode one capture. Silence is kept whole and reported. */
export function trainingClip(pcm: ArrayBuffer): TrainingClip {
  const samples = new Int16Array(pcm);
  const frame = Math.round(FRAME * TRAINING_RATE);
  const frames = Math.floor(samples.length / frame);
  let clip = samples;
  let foundSpeech = false;
  if (frames > 0) {
    const db = new Float64Array(frames);
    for (let i = 0; i < frames; i++) {
      let sum = 0;
      for (let j = i * frame; j < (i + 1) * frame; j++) {
        const v = samples[j] / 32768;
        sum += v * v;
      }
      db[i] = 20 * Math.log10(Math.sqrt(sum / frame) + 1e-12);
    }
    const floor = percentile(db, 10);
    const threshold = Math.max(FLOOR_DBFS, floor + MARGIN_DB);
    let first = -1;
    let last = -1;
    for (let i = 0; i < frames; i++) {
      if (db[i] > threshold) {
        if (first < 0) first = i;
        last = i;
      }
    }
    if (first >= 0) {
      foundSpeech = true;
      // Keep the onset; the trailing post-roll is the cheapest to lose.
      const start = Math.max(0, first * frame - Math.round(PRE_SECONDS * TRAINING_RATE));
      const end = Math.min(samples.length, (last + 1) * frame + Math.round(POST_SECONDS * TRAINING_RATE));
      clip = samples.subarray(start, Math.min(end, start + Math.round(MAX_CLIP_SECONDS * TRAINING_RATE)));
    }
  }
  if (clip.length > MAX_CLIP_SECONDS * TRAINING_RATE) clip = clip.subarray(0, Math.round(MAX_CLIP_SECONDS * TRAINING_RATE));
  let peak = 0;
  for (let i = 0; i < clip.length; i++) peak = Math.max(peak, Math.abs(clip[i]) / 32768);
  return { wav: encodeWAV(clip), seconds: clip.length / TRAINING_RATE, foundSpeech, peak };
}

function percentile(values: ArrayLike<number>, percent: number): number {
  const sorted = Array.from(values).sort((a, b) => a - b);
  if (!sorted.length) return 0;
  return sorted[Math.floor((percent / 100) * (sorted.length - 1))];
}

// Encode canonical PCM16 WAV for training or full-capture replay.
export function encodeWAV(samples: Int16Array): ArrayBuffer {
  const buffer = new ArrayBuffer(44 + samples.length * 2);
  const view = new DataView(buffer);
  const text = (offset: number, value: string) => {
    for (let i = 0; i < value.length; i++) view.setUint8(offset + i, value.charCodeAt(i));
  };
  text(0, "RIFF");
  view.setUint32(4, 36 + samples.length * 2, true);
  text(8, "WAVE");
  text(12, "fmt ");
  view.setUint32(16, 16, true);
  view.setUint16(20, 1, true); // PCM
  view.setUint16(22, 1, true); // mono
  view.setUint32(24, TRAINING_RATE, true);
  view.setUint32(28, TRAINING_RATE * 2, true);
  view.setUint16(32, 2, true);
  view.setUint16(34, 16, true);
  text(36, "data");
  view.setUint32(40, samples.length * 2, true);
  for (let i = 0; i < samples.length; i++) view.setInt16(44 + i * 2, samples[i], true);
  return buffer;
}
