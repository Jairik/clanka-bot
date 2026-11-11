# Pocket TTS (Kyutai 100M) on CPU behind our Wyoming wrapper. Build natively
# for the target architecture: amd64 on the laptop, arm64 on the Pi 5.
FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    HF_HOME=/data/huggingface

WORKDIR /app

# Install the CPU build of torch first so pip never resolves the CUDA wheels.
RUN pip install --index-url https://download.pytorch.org/whl/cpu "torch>=2.5"

COPY requirements.txt .
RUN pip install -r requirements.txt

COPY pcm.py engine.py server.py healthcheck.py ./

# The model weights are licence-gated on Hugging Face and download on first
# start into HF_HOME (a named volume). Set HF_TOKEN in .env to allow it.
EXPOSE 10200

CMD ["python", "server.py"]
