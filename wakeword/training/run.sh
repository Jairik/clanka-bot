#!/usr/bin/env bash
# Reproduce a wakeword/models/<phrase>.tflite detector from pinned public
# assets, Pocket TTS clips and (once recorded) the owner's own takes.
#
# Usage: wakeword/training/run.sh [setup|assets|clips|acceptance|real|train|convert|evaluate|fixtures|all]
#        PHRASE=volume_up wakeword/training/run.sh all
# PHRASE defaults to clanker. Everything lands in $WORK (default
# ~/.cache/clanka/wakeword-clanker); nothing is uploaded.
set -euo pipefail

REPO=$(cd "$(dirname "$0")/../.." && pwd)
TRAIN="$REPO/wakeword/training"
WORK=${WORK:-$HOME/.cache/clanka/wakeword-clanker}
PHRASE=${PHRASE:-clanker}
# Smaller starter models can override the synthetic clip counts.
N_TRAIN=${N_TRAIN:-10000}
N_TEST=${N_TEST:-2000}
# Workers per clip-generation set. Lower this on a small-VRAM or shared box.
GEN_WORKERS=${GEN_WORKERS:-4}
OWW_REV=368c03716d1e92591906a84949bc477f3a834455
VOICES=alba,anna,azelma,bill_boerst,charles,cosette,daan,eponine,estelle,eve,fantine,george,giovanni,jane,javert,jean,juergen,lola,marius,mary,michael,paul,peter_yearsley,rafael,vera
HELD_OUT=caro_davy,stuart_bell

mkdir -p "$WORK"/{data,clips,background,logs}
cd "$WORK"

# phrase_file resolves a clip-list name for PHRASE. A phrase-specific list lives
# under training/phrases/<phrase>/; clanker keeps the original top-level files.
phrase_file() {
  local kind=$1
  local specific="$TRAIN/phrases/$PHRASE/$kind.txt"
  if [ -f "$specific" ]; then
    printf '%s\n' "$specific"
  else
    printf '%s\n' "$TRAIN/$kind.txt"
  fi
}

MODEL_DIR="clips/$PHRASE"
ACCEPT_DIR="$MODEL_DIR/acceptance"

setup() {
  uv venv --python 3.11 venv
  VIRTUAL_ENV="$WORK/venv" uv pip install --python venv/bin/python torch --index-url https://download.pytorch.org/whl/cpu
  VIRTUAL_ENV="$WORK/venv" uv pip install --python venv/bin/python onnxruntime ai-edge-litert scipy scikit-learn tqdm pyyaml mutagen torchinfo torchmetrics \
    speechbrain "audiomentations>=0.33,<1" torch-audiomentations acoustics pronouncing "datasets>=2.14,<4" soundfile pyarrow huggingface_hub onnx onnxscript tensorflow
  [ -d openWakeWord ] || git clone https://github.com/dscripka/openWakeWord openWakeWord
  git -C openWakeWord checkout "$OWW_REV"
  VIRTUAL_ENV="$WORK/venv" uv pip install --python venv/bin/python -e openWakeWord
  venv/bin/python -c "from openwakeword.utils import download_models; download_models()"
  uv venv --python 3.11 venv-tts
  VIRTUAL_ENV="$WORK/venv-tts" uv pip install --python venv-tts/bin/python torch --index-url https://download.pytorch.org/whl/cu126
  VIRTUAL_ENV="$WORK/venv-tts" uv pip install --python venv-tts/bin/python "pocket-tts==4.0.1" scipy
  uv venv --python 3.11 venv-convert
  VIRTUAL_ENV="$WORK/venv-convert" uv pip install --python venv-convert/bin/python onnx2tf
  mkdir -p stub_piper
  printf 'def generate_samples(**kwargs):\n    raise RuntimeError("clips come from Pocket TTS")\n' > stub_piper/generate_samples.py
}

assets() {
  venv/bin/python "$TRAIN/prepare_assets.py" --dir "$WORK"
  venv/bin/python "$TRAIN/acav_subset.py" --src data/openwakeword_features_ACAV100M_2000_hrs_16bit.npy --out data/acav_subset.npy
}

gen() {
  local device=cpu
  command -v nvidia-smi >/dev/null && device=cuda
  HF_HOME="$WORK/hf-tts" venv-tts/bin/python "$TRAIN/generate_clips.py" --device "$device" "$@"
}

clips() {
  mkdir -p "$MODEL_DIR"
  gen --out-dir "$MODEL_DIR/positive_train" --phrases "$(phrase_file positives)" --voices "$VOICES" --count "$N_TRAIN" --workers "$GEN_WORKERS" --speed-probability 0.25 --seed 1
  gen --out-dir "$MODEL_DIR/positive_test" --phrases "$(phrase_file positives)" --voices "$VOICES" --count "$N_TEST" --workers "$GEN_WORKERS" --speed-probability 0.25 --seed 2
  gen --out-dir "$MODEL_DIR/negative_train" --phrases "$(phrase_file negative_phrases)" --voices "$VOICES" --count "$N_TRAIN" --workers "$GEN_WORKERS" --speed-probability 0.15 --seed 3
  gen --out-dir "$MODEL_DIR/negative_test" --phrases "$(phrase_file negative_phrases)" --voices "$VOICES" --count "$N_TEST" --workers "$GEN_WORKERS" --speed-probability 0.15 --seed 4
}

acceptance() {
  mkdir -p "$ACCEPT_DIR"
  gen --out-dir "$ACCEPT_DIR/positive" --phrases "$(phrase_file positives)" --voices "$HELD_OUT" --count 60 --workers "$GEN_WORKERS" --speed-probability 0.5 --seed 7
  gen --out-dir "$ACCEPT_DIR/negative" --phrases "$(phrase_file near_misses)" --voices "$HELD_OUT" --count 80 --workers "$GEN_WORKERS" --speed-probability 0.5 --seed 11
  gen --out-dir "$ACCEPT_DIR/speech" --phrases "$(phrase_file ordinary_phrases)" --voices "$HELD_OUT" --count 40 --workers "$GEN_WORKERS" --speed-probability 0.3 --seed 13
}

real() {
  venv/bin/python "$TRAIN/prep_real.py" --work "$WORK" --model "$PHRASE"
}

train() {
  # wakeword/train may have written a per-run config (steps, clip counts) into
  # the work directory; only fall back to the checked-in phrase config when it
  # has not. A direct `run.sh train` therefore keeps #114's behaviour.
  if [ ! -f "$WORK/$PHRASE.yml" ]; then
    [ -f "$TRAIN/$PHRASE.yml" ] || { echo "run.sh: missing $TRAIN/$PHRASE.yml" >&2; exit 2; }
    cp "$TRAIN/$PHRASE.yml" "$WORK/$PHRASE.yml"
  fi
  venv/bin/python openWakeWord/openwakeword/train.py --training_config "$WORK/$PHRASE.yml" --augment_clips
  # openWakeWord then always attempts its own onnx -> tflite conversion (its
  # --convert_to_tflite default is the non-empty string "False"), which needs
  # onnx-tf. The ONNX is written before that, and convert does the conversion,
  # so a non-zero exit is expected once the ONNX exists.
  # output_dir is ./clips, so export_model writes clips/<phrase>.onnx next to
  # the clips/<phrase>/ directory rather than inside it.
  rm -f "clips/$PHRASE.onnx"
  venv/bin/python openWakeWord/openwakeword/train.py --training_config "$WORK/$PHRASE.yml" --train_model || true
  [ -s "clips/$PHRASE.onnx" ] || { echo "training did not write clips/$PHRASE.onnx" >&2; exit 1; }
}

convert() {
  venv-convert/bin/python "$TRAIN/convert_model.py" --onnx "clips/$PHRASE.onnx" --out "$REPO/wakeword/models/$PHRASE.tflite"
}

evaluate() {
  local real_positive=() real_negative=()
  if [ -d "$MODEL_DIR/real_holdout/positive" ] && [ -n "$(ls -A "$MODEL_DIR/real_holdout/positive")" ]; then
    real_positive=(--real-positive "$MODEL_DIR/real_holdout/positive")
  fi
  if [ -d "$MODEL_DIR/real_holdout/negative" ] && [ -n "$(ls -A "$MODEL_DIR/real_holdout/negative")" ]; then
    real_negative=(--real-negative "$MODEL_DIR/real_holdout/negative")
  fi
  # Commands run at 0.7 by default; clanker keeps the deployed 0.5. The
  # near-miss phrase breakdown uses the phrase actually trained for.
  local threshold=${EVAL_THRESHOLD:-0.5} target
  if [ "$PHRASE" != clanker ] && [ -z "${EVAL_THRESHOLD:-}" ]; then
    threshold=0.7
  fi
  target=$(head -n 1 "$(phrase_file positives)")
  venv/bin/python "$TRAIN/evaluate.py" --model "$REPO/wakeword/models/$PHRASE.tflite" \
    --threshold "$threshold" --target "$target" \
    --positives "$MODEL_DIR/positive_test" \
    --negatives "$MODEL_DIR/negative_test" \
    --voices "$ACCEPT_DIR/positive" \
    --near-misses "$ACCEPT_DIR/negative" \
    --speech "$ACCEPT_DIR/speech" \
    --background "$WORK/background" --background-max "${BACKGROUND_MAX:-400}" \
    "${real_positive[@]}" "${real_negative[@]}" \
    --json-out "$WORK/logs/eval-$PHRASE.json"
}

fixtures() {
  if [ "$PHRASE" != clanker ]; then
    echo "fixtures are the clanker detection-test set; there is no fixture set for $PHRASE" >&2
    exit 2
  fi
  local pos="$REPO/wakeword/tests/fixtures/positive" neg="$REPO/wakeword/tests/fixtures/negative"
  mkdir -p "$pos" "$neg"
  rm -f "$pos"/*.wav "$neg"/*.wav
  local i=0
  for f in $(ls clips/acceptance/positive/*.wav | head -3); do
    cp "$f" "$pos/clanker_$(printf '%02d' $i).wav"; i=$((i+1))
  done
  # One clip per phrase, chosen by phrase rather than by filename so the set is
  # stable across reruns: two near misses, one adjacent word, one ordinary
  # request. Phrases the model still confuses (clunker, clanger, cranker) are
  # reported in training/README.md instead of being hidden here.
  local phrases=("clank" "clinic" "blanker" "turn on the lights") phrase name
  i=0
  for phrase in "${phrases[@]}"; do
    name=$(awk -F'\t' -v p="$phrase" '$2==p{print $1; exit}' clips/acceptance/negative/manifest*.tsv)
    if [ -n "$name" ]; then
      cp "clips/acceptance/negative/$name" "$neg/$(printf '%02d' $i)_${phrase// /_}.wav"
      i=$((i+1))
    fi
  done
  echo "wrote synthetic detection fixtures under wakeword/tests/fixtures"
}

case "${1:-all}" in
  setup|assets|clips|acceptance|real|train|convert|evaluate|fixtures) "$1" ;;
  all) setup; assets; clips; acceptance; real; train; convert; evaluate; if [ "$PHRASE" = clanker ]; then fixtures; fi ;;
  *) echo "usage: $0 [setup|assets|clips|acceptance|real|train|convert|evaluate|fixtures|all] (PHRASE=<name>)" >&2; exit 2 ;;
esac
