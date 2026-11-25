# Parakeet ONNX STT + Silero VAD behind our Wyoming server. Build natively for
# the target architecture: amd64 on the desktop, arm64 on the Pi 5.
FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    STT_MODEL_ROOT=/opt/stt

WORKDIR /app

# onnxruntime needs the OpenMP runtime; the wheel bundles the rest.
RUN apt-get update \
    && apt-get install -y --no-install-recommends libgomp1 \
    && rm -rf /var/lib/apt/lists/*

COPY requirements.txt .
RUN pip install -r requirements.txt

# Weights come from public Hugging Face ONNX repos anonymously and land in one
# early layer, before application code is copied. Note: parakeet_engine.py and
# vad.py are NOT copied yet so code edits do not bust the 660 MB weights layer;
# prefetch.py is self-contained for downloading and checking files.
ARG STT_MODELS=parakeet-tdt-0.6b-v2
ENV STT_MODELS=$STT_MODELS
COPY prefetch.py .
RUN python prefetch.py \
    && HF_HUB_OFFLINE=1 TRANSFORMERS_OFFLINE=1 python prefetch.py --check \
    && rm -rf prefetch.py /root/.cache/huggingface

# No runtime download path: the weights are baked above and these make any
# accidental hub lookup fail instead of reaching the network.
ENV HF_HUB_OFFLINE=1 \
    TRANSFORMERS_OFFLINE=1

COPY pcm16.py parakeet_engine.py vad.py stt_server.py healthcheck.py correct.py vocab.json ./

EXPOSE 10300

CMD ["python", "stt_server.py"]
