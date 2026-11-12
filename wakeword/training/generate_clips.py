"""Synthesize wake-word clips with the Pocket TTS model the tts/ service uses.

Clips are written flat into --out-dir with a worker prefix, because openWakeWord
reads them with a non-recursive glob. 16 kHz mono PCM16 is what the training
pipeline expects.
"""

import argparse
import multiprocessing as mp
import random
import wave
from pathlib import Path

import numpy as np
from scipy.signal import resample_poly

SR = 16000
# ``resample_poly`` ratios for the speed factors Pocket TTS clips are stretched by.
SPEED_FACTORS = {0.9: (9, 10), 0.95: (19, 20), 1.05: (21, 20), 1.1: (11, 10)}


def write_wav(path, audio):
    pcm = (np.clip(audio, -1, 1) * 32767).astype(np.int16)
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(SR)
        w.writeframes(pcm.tobytes())


def stretch(audio, factor):
    up, down = SPEED_FACTORS[factor]
    return resample_poly(audio, up, down)


def worker(args):
    worker_id, n_workers, phrases, voices, count, out_dir, seed, speed_p, device = args
    from pocket_tts import TTSModel

    rng = random.Random(seed + worker_id)
    model = TTSModel.load_model(language="english")
    if device == "cuda":
        model.flow_lm.to("cuda")
        model.mimi.to("cuda")
    states = {v: model.get_state_for_audio_prompt(v) for v in voices}
    out = Path(out_dir)
    out.mkdir(parents=True, exist_ok=True)
    # One manifest per worker: it records which phrase a clip came from, which
    # is what lets evaluate.py separate "clanker's" from a true near miss.
    manifest = open(out / f"manifest_w{worker_id}.tsv", "w")
    share = count // n_workers + (1 if worker_id < count % n_workers else 0)
    for i in range(share):
        text = rng.choice(phrases)
        audio = model.generate_audio(model_state=states[rng.choice(voices)], text_to_generate=text)
        audio = np.asarray(audio.cpu() if hasattr(audio, "cpu") else audio, dtype=np.float32).squeeze()
        if model.sample_rate != SR:
            audio = resample_poly(audio, 2, 3) if model.sample_rate == 24000 else audio
        if rng.random() < speed_p:
            audio = stretch(audio, rng.choice(sorted(SPEED_FACTORS)))
        name = f"w{worker_id}_{i:06d}.wav"
        write_wav(out / name, audio)
        manifest.write(f"{name}\t{text}\n")
        manifest.flush()
        if (i + 1) % 500 == 0:
            print(f"worker {worker_id}: {i + 1} clips", flush=True)
    manifest.close()


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--out-dir", required=True)
    p.add_argument("--phrases", required=True)
    p.add_argument("--voices", required=True)
    p.add_argument("--count", type=int, required=True)
    p.add_argument("--workers", type=int, default=4)
    p.add_argument("--speed-probability", type=float, default=0.0)
    p.add_argument("--device", default="cpu")
    p.add_argument("--seed", type=int, default=0)
    args = p.parse_args()
    phrases = [line.strip() for line in Path(args.phrases).read_text().splitlines() if line.strip()]
    voices = [v.strip() for v in args.voices.split(",") if v.strip()]
    jobs = [(i, args.workers, phrases, voices, args.count, args.out_dir, args.seed, args.speed_probability, args.device) for i in range(args.workers)]
    if args.workers == 1:
        worker(jobs[0])
    else:
        with mp.get_context("spawn").Pool(args.workers) as pool:
            pool.map(worker, jobs)


if __name__ == "__main__":
    main()
