# Pocket TTS (Kyutai 100M) on CPU behind our Wyoming wrapper. Build natively
# for the target architecture: amd64 on the laptop, arm64 on the Pi 5.
FROM python:3.12-slim

# HF_HOME lives in the image, not a volume, so a container start needs no network.
ENV PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    HF_HOME=/opt/huggingface \
    HF_HUB_DISABLE_TELEMETRY=1

WORKDIR /app

# Install the CPU build of torch first so pip never resolves the CUDA wheels.
RUN pip install --index-url https://download.pytorch.org/whl/cpu "torch>=2.5"

COPY requirements.txt .
RUN pip install -r requirements.txt

# The same values compose passes at runtime; changing them needs a rebuild.
ARG TTS_LANGUAGE=english
ARG TTS_VOICES=alba
ARG TTS_VOICE=alba
ENV TTS_LANGUAGE=$TTS_LANGUAGE TTS_VOICES=$TTS_VOICES TTS_VOICE=$TTS_VOICE

# Weights and voices come from public Hugging Face repos with an anonymous
# request: no token is passed to this build, and nothing is downloaded later.
COPY prefetch.py .
RUN python prefetch.py \
    && HF_HUB_OFFLINE=1 python prefetch.py --check \
    && rm -f prefetch.py

# Startup resolves every file from HF_HOME, and a cache miss fails fast instead
# of reaching for the network.
ENV HF_HUB_OFFLINE=1

COPY pcm.py engine.py server.py healthcheck.py ./

EXPOSE 10200

CMD ["python", "server.py"]
