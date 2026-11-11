# Moonshine Voice STT behind our Wyoming server. Build natively for the target
# architecture: amd64 on the laptop, arm64 on the Pi 5.
FROM python:3.12-slim

# No runtime download path: the weights are baked below and these make any
# accidental hub lookup fail instead of reaching the network.
ENV PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    HF_HUB_OFFLINE=1 \
    TRANSFORMERS_OFFLINE=1 \
    MOONSHINE_MODEL_ROOT=/opt/moonshine

WORKDIR /app

# onnxruntime needs the OpenMP runtime; the wheel bundles the rest.
RUN apt-get update \
    && apt-get install -y --no-install-recommends libgomp1 \
    && rm -rf /var/lib/apt/lists/*

COPY requirements.txt .
RUN pip install -r requirements.txt

# Weights come from the public download.moonshine.ai CDN with an anonymous
# request and land in one early layer, before any application code is copied.
ARG STT_MODELS=small-streaming,tiny-streaming
ENV STT_MODELS=$STT_MODELS
COPY prefetch.py .
RUN python prefetch.py \
    && python prefetch.py --check \
    && rm -f prefetch.py

COPY pcm16.py moonshine_engine.py stt_server.py healthcheck.py ./

EXPOSE 10300

CMD ["python", "stt_server.py"]
