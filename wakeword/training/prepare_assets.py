"""Fetch the pinned training assets and decode AudioSet shards to 16 kHz WAV."""

import argparse
import io
from pathlib import Path

import pyarrow.parquet as pq
import soundfile as sf
from huggingface_hub import hf_hub_download, snapshot_download
from scipy.signal import resample_poly

FEATURE_REPO = "davidscripka/openwakeword_features"
FEATURE_REV = "985bf1b47e7f19c07741af82bfe32d5a9dc56096"
RIR_REPO = "davidscripka/MIT_environmental_impulse_responses"
RIR_REV = "b824a1ef2821f112fda0b9cb26e4278c62b425bb"
AUDIOSET_REPO = "agkphysics/AudioSet"
AUDIOSET_REV = "0c609e8302cf139307f639c57652032af0a88041"
SHARDS = ["00", "01", "02", "03", "04", "05", "06", "07"]


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--dir", required=True)
    args = p.parse_args()
    root = Path(args.dir)
    data = root / "data"
    data.mkdir(parents=True, exist_ok=True)

    for name in ("openwakeword_features_ACAV100M_2000_hrs_16bit.npy", "validation_set_features.npy"):
        hf_hub_download(FEATURE_REPO, repo_type="dataset", revision=FEATURE_REV, filename=name, local_dir=data)
    snapshot_download(RIR_REPO, repo_type="dataset", revision=RIR_REV, allow_patterns=["16khz/*"], local_dir=data / "mit_rirs")

    background = root / "background"
    background.mkdir(exist_ok=True)
    for shard in SHARDS:
        path = hf_hub_download(AUDIOSET_REPO, repo_type="dataset", revision=AUDIOSET_REV, filename=f"data/bal_train/{shard}.parquet", local_dir=data / "audioset")
        table = pq.read_table(path, columns=["audio"])
        for i, item in enumerate(table.column("audio")):
            audio, sr = sf.read(io.BytesIO(item.as_py()["bytes"]), dtype="float32")
            if audio.ndim > 1:
                audio = audio.mean(axis=1)
            if sr != 16000:
                audio = resample_poly(audio, 16000, sr)
            sf.write(background / f"{shard}_{i:04d}.wav", audio, 16000, subtype="PCM_16")
        print("decoded", shard, flush=True)


if __name__ == "__main__":
    main()
