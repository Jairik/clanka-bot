"""Copy evenly spaced AudioSet/ACAV feature blocks into a page-cache-sized subset."""

import argparse
from pathlib import Path

import numpy as np


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--src", required=True)
    p.add_argument("--out", required=True)
    p.add_argument("--samples", type=int, default=200000)
    p.add_argument("--block", type=int, default=1000)
    args = p.parse_args()
    src = np.load(args.src, mmap_mode="r")
    blocks = args.samples // args.block
    starts = np.linspace(0, len(src) - args.block, blocks, dtype=np.int64)
    out = np.empty((blocks * args.block,) + src.shape[1:], dtype=src.dtype)
    for i, start in enumerate(starts):
        out[i * args.block:(i + 1) * args.block] = src[start:start + args.block]
    np.save(args.out, out)
    print("wrote", args.out, out.shape, f"{Path(args.out).stat().st_size / 1e6:.0f} MB")


if __name__ == "__main__":
    main()
