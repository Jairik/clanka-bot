"""Split recorded takes into the training set and a held-out evaluation slice.

Runs are resumable and deterministic: takes are sorted by filename and every
fifth one (20%) is held out for evaluation. Numbering only grows, so a take
never moves between the two sets once recorded.

Real positives are copied into ``clips/<model>/positive_train`` so they get the
same augmentation as the synthetic clips; real negatives become hard negatives
in ``negative_train``. The held-out copies stay unaugmented for ``evaluate.py``.
The precomputed feature files are removed because the clip set changed.
"""

import argparse
import shutil
from pathlib import Path

HOLDOUT_EVERY = 5
FEATURES = (
    "positive_features_train.npy",
    "negative_features_train.npy",
    "positive_features_test.npy",
    "negative_features_test.npy",
)


def split(paths):
    held_out, training = [], []
    for index, path in enumerate(paths):
        (held_out if index % HOLDOUT_EVERY == 0 else training).append(path)
    return training, held_out


def copy_all(paths, directory, prefix):
    directory.mkdir(parents=True, exist_ok=True)
    for old in directory.glob(f"{prefix}*.wav"):
        old.unlink()
    for path in paths:
        shutil.copyfile(path, directory / f"{prefix}{path.name}")


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--work", required=True)
    p.add_argument("--model", default="clanker")
    p.add_argument("--keep-features", action="store_true", help="keep old feature files even though clips changed")
    args = p.parse_args()
    work = Path(args.work)
    model_dir = work / "clips" / args.model

    positives = sorted((work / "real" / "positive").glob("*.wav"))
    negatives = sorted((work / "real" / "negative").glob("*.wav"))
    if not positives and not negatives:
        print("no real recordings yet; training stays synthetic-only")
        print(f"record some with: {Path(__file__).resolve().parent / 'record.py'}")
        return

    pos_train, pos_holdout = split(positives)
    neg_train, neg_holdout = split(negatives)
    copy_all(pos_train, model_dir / "positive_train", "realp_")
    copy_all(neg_train, model_dir / "negative_train", "realn_")
    copy_all(pos_holdout, work / "clips" / "real_holdout" / "positive", "")
    copy_all(neg_holdout, work / "clips" / "real_holdout" / "negative", "")

    print(f"real positives: {len(pos_train)} for training, {len(pos_holdout)} held out")
    print(f"real negatives: {len(neg_train)} for training, {len(neg_holdout)} held out")

    if not args.keep_features:
        for name in FEATURES:
            (model_dir / name).unlink(missing_ok=True)
        print("removed the old feature files; the next train run re-augments every clip")


if __name__ == "__main__":
    main()
