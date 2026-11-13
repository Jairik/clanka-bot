// Acoustic checks are independent of the wake model and cannot verify the spoken words.
import { trainingClip, TRAINING_RATE } from "./wake-sample";

export function assessSample(pcm: ArrayBuffer) {
  const clip = trainingClip(pcm);
  const samples = new Int16Array(pcm);
  const warnings: string[] = [];
  const frame = 320;
  const levels: number[] = [];
  let clipped = 0;
  for (let i = 0; i + frame <= samples.length; i += frame) {
    let sum = 0;
    for (let j = i; j < i + frame; j++) {
      const v = samples[j] / 32768;
      sum += v * v;
      if (Math.abs(v) >= 0.99) clipped++;
    }
    levels.push(Math.sqrt(sum / frame));
  }
  const sorted = [...levels].sort((a, b) => a - b);
  const floor = sorted[Math.floor(sorted.length * 0.1)] ?? 0;
  const gate = Math.max(10 ** (-45 / 20), floor * 10 ** (8 / 20));
  const first = levels.findIndex((v) => v > gate);
  let last = -1;
  for (let i = 0; i < levels.length; i++) if (levels[i] > gate) last = i;
  if (!clip.foundSpeech) warnings.push("No clear speech above room noise.");
  if (clip.peak < 0.02) warnings.push("Very quiet; check microphone placement.");
  if (clipped >= Math.max(3, samples.length * 0.001)) warnings.push("Possible clipping; reduce level or move back.");
  if (first >= 0 && (first < 3 || last >= levels.length - 3)) warnings.push("Speech touches the recording boundary; the phrase may be incomplete.");
  // Reject truncation instead of silently teaching the model a partial phrase.
  const trimStart = Math.max(0, first * frame - Math.round(0.15 * TRAINING_RATE));
  const truncated = first >= 0 && (last + 1) * frame > trimStart + Math.round(1.9 * TRAINING_RATE);
  if (truncated) warnings.push("Speech exceeds the training clip length; say the complete phrase more compactly.");
  const blocked = !clip.foundSpeech || truncated || (first >= 0 && (first < 3 || last >= levels.length - 3));
  return { ...clip, warnings, blocked };
}
