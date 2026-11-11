# CPU-only MiniLM service. Weights are baked in so the Pi needs no download.
FROM python:3.12-slim

# CPU torch keeps the image small; the GPU wheel would pull CUDA.
RUN pip install --no-cache-dir torch==2.15.0 --index-url https://download.pytorch.org/whl/cpu
COPY requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir -r /tmp/requirements.txt

ARG EMBED_MODEL=sentence-transformers/all-MiniLM-L6-v2
ENV EMBED_MODEL=${EMBED_MODEL}
# Bake the model into the image; runtime uses this same cache directory.
ENV HF_HOME=/models
RUN python -c "from sentence_transformers import SentenceTransformer; SentenceTransformer('${EMBED_MODEL}')"

RUN useradd -m -u 10001 embed
USER embed
COPY app.py /app/app.py
WORKDIR /app
EXPOSE 8085
ENTRYPOINT ["python", "/app/app.py"]
