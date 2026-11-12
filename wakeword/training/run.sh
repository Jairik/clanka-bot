#!/usr/bin/env bash
# Reproduce wakeword/models/clanker.tflite from pinned public assets, Pocket TTS
# clips and (once recorded) the owner's own takes.
#
# Usage: wakeword/training/run.sh [setup|assets|clips|acceptance|real|train|convert|evaluate|fixtures|all]
# Everything lands in $WORK (default ~/.cache/clanka/wakeword-clanker); nothing is uploaded.
set -euo pipefail

REPO=$(cd "$(dirname "$0")/../.." && pwd)
TRAIN="$REPO/wakeword/training"
WORK=${WORK:-$HOME/.cache/clanka/wakeword-clanker}
OWW_REV=368c03716d1e92591906a84949bc477f3a834455
VOICES=alba,anna,azelma,bill_boerst,charles,cosette,daan,eponine,estelle,eve,fantine,george,giovanni,jane,javert,jean,juergen,lola,marius,mary,michael,paul,peter_yearsley,rafael,vera
HELD_OUT=caro_davy,stuart_bell

mkdir -p "$WORK"/{data,clips,background,logs}
cd "$WORK"

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
  mkdir -p clips/clanker
  gen --out-dir clips/clanker/positive_train --phrases "$TRAIN/positives.txt" --voices "$VOICES" --count 10000 --workers 4 --speed-probability 0.25 --seed 1
  gen --out-dir clips/clanker/positive_test --phrases "$TRAIN/positives.txt" --voices "$VOICES" --count 2000 --workers 4 --speed-probability 0.25 --seed 2
  gen --out-dir clips/clanker/negative_train --phrases "$TRAIN/negative_phrases.txt" --voices "$VOICES" --count 10000 --workers 4 --speed-probability 0.15 --seed 3
  gen --out-dir clips/clanker/negative_test --phrases "$TRAIN/negative_phrases.txt" --voices "$VOICES" --count 2000 --workers 4 --speed-probability 0.15 --seed 4
}

acceptance() {
  mkdir -p clips/acceptance
  gen --out-dir clips/acceptance/positive --phrases "$TRAIN/positives.txt" --voices "$HELD_OUT" --count 60 --workers 2 --speed-probability 0.5 --seed 7
  gen --out-dir clips/acceptance/negative --phrases "$TRAIN/near_misses.txt" --voices "$HELD_OUT" --count 80 --workers 2 --speed-probability 0.5 --seed 11
  gen --out-dir clips/acceptance/speech --phrases "$TRAIN/ordinary_phrases.txt" --voices "$HELD_OUT" --count 40 --workers 2 --speed-probability 0.3 --seed 13
}

real() {
  venv/bin/python "$TRAIN/prep_real.py" --work "$WORK"
}

train() {
  cp "$TRAIN/clanker.yml" clanker.yml
  venv/bin/python openWakeWord/openwakeword/train.py --training_config clanker.yml --augment_clips
  # openWakeWord then always attempts its own onnx -> tflite conversion (its
  # --convert_to_tflite default is the non-empty string "False"), which needs
  # onnx-tf. The ONNX is written before that, and convert does the conversion,
  # so a non-zero exit is expected once the ONNX exists.
  # output_dir is ./clips, so export_model writes clips/clanker.onnx next to
  # the clips/clanker/ directory rather than inside it.
  rm -f clips/clanker.onnx
  venv/bin/python openWakeWord/openwakeword/train.py --training_config clanker.yml --train_model || true
  [ -s clips/clanker.onnx ] || { echo "training did not write clips/clanker.onnx" >&2; exit 1; }
}

convert() {
  venv-convert/bin/python "$TRAIN/convert_model.py" --onnx clips/clanker.onnx --out "$REPO/wakeword/models/clanker.tflite"
}

evaluate() {
  local real_positive=() real_negative=()
  if [ -d clips/real_holdout/positive ] && [ -n "$(ls -A clips/real_holdout/positive)" ]; then
    real_positive=(--real-positive clips/real_holdout/positive)
  fi
  if [ -d clips/real_holdout/negative ] && [ -n "$(ls -A clips/real_holdout/negative)" ]; then
    real_negative=(--real-negative clips/real_holdout/negative)
  fi
  venv/bin/python "$TRAIN/evaluate.py" --model "$REPO/wakeword/models/clanker.tflite" \
    --positives clips/clanker/positive_test \
    --negatives clips/clanker/negative_test \
    --voices clips/acceptance/positive \
    --near-misses clips/acceptance/negative \
    --speech clips/acceptance/speech \
    --background "$WORK/background" \
    "${real_positive[@]}" "${real_negative[@]}" \
    --json-out "$WORK/logs/eval.json"
}

fixtures() {
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
  all) setup; assets; clips; acceptance; real; train; convert; evaluate; fixtures ;;
  *) echo "usage: $0 [setup|assets|clips|acceptance|real|train|convert|evaluate|fixtures|all]" >&2; exit 2 ;;
esac
