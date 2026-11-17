"""Embed service: MiniLM sentence vectors over plain HTTP for the Fast path."""

import logging
import os
import threading
from contextlib import asynccontextmanager

import uvicorn
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel

log = logging.getLogger("embed")

# Model name is config so bge-small can be tried later (see issue #10).
MODEL_NAME = os.environ.get("EMBED_MODEL", "sentence-transformers/all-MiniLM-L6-v2")
PORT = int(os.environ.get("EMBED_PORT", "8085"))

# Defense-in-depth limits: bound batch size and individual text length to prevent
# swapping and out-of-memory crashes on memory-constrained devices.
MAX_BATCH_SIZE = 256
MAX_TEXT_LENGTH = 2000


# Manage application startup and model loading lifecycle.
@asynccontextmanager
async def lifespan(app: FastAPI):
    # Only load sentence_transformers if no model was pre-injected (e.g. by tests).
    if not hasattr(app.state, "model") or app.state.model is None:
        log.info("loading model %s", MODEL_NAME)
        from sentence_transformers import SentenceTransformer

        app.state.model = SentenceTransformer(MODEL_NAME)
        app.state.dim = app.state.model.get_sentence_embedding_dimension()
        log.info("model ready, dim=%d", app.state.dim)
    yield


app = FastAPI(lifespan=lifespan)
# Serialize encoding so concurrent callers queue instead of multiplying peak memory.
app.state.lock = threading.Lock()
app.state.pending = 0
app.state.pending_lock = threading.Lock()


class EmbedRequest(BaseModel):
    texts: list[str]


# Report service health, model dimensionality, and current pending request count.
@app.get("/healthz")
def healthz():
    # Ready once the lifespan has loaded the model and set the dimension.
    return {
        "status": "ok",
        "model": MODEL_NAME,
        "dim": getattr(app.state, "dim", None),
        "pending": getattr(app.state, "pending", 0),
    }


# Embed a bounded batch of texts, serializing model encoding to prevent OOM.
@app.post("/embed")
def embed(req: EmbedRequest):
    if not req.texts:
        raise HTTPException(status_code=400, detail="texts must not be empty")
    if len(req.texts) > MAX_BATCH_SIZE:
        raise HTTPException(
            status_code=413,
            detail=f"batch size {len(req.texts)} exceeds maximum of {MAX_BATCH_SIZE}",
        )
    for idx, text in enumerate(req.texts):
        if len(text) > MAX_TEXT_LENGTH:
            raise HTTPException(
                status_code=413,
                detail=f"text at index {idx} exceeds maximum length of {MAX_TEXT_LENGTH} characters",
            )

    # Track in-flight and queued encode requests for healthz observability.
    with app.state.pending_lock:
        app.state.pending += 1
    try:
        # Acquire lock to serialize encode execution across worker threads.
        with app.state.lock:
            # Normalise here so Go only has to take a dot product.
            vectors = app.state.model.encode(
                req.texts, normalize_embeddings=True, convert_to_numpy=True
            )
            # Support both numpy arrays and lists of vectors.
            if hasattr(vectors, "tolist"):
                vectors_list = vectors.tolist()
            else:
                vectors_list = list(vectors)
    finally:
        with app.state.pending_lock:
            app.state.pending -= 1

    return {"model": MODEL_NAME, "vectors": vectors_list}


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=PORT)
