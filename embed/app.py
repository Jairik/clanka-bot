"""Embed service: MiniLM sentence vectors over plain HTTP for the Fast path."""

import logging
import os
from contextlib import asynccontextmanager

import uvicorn
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel
from sentence_transformers import SentenceTransformer

log = logging.getLogger("embed")

# Model name is config so bge-small can be tried later (see issue #10).
MODEL_NAME = os.environ.get("EMBED_MODEL", "sentence-transformers/all-MiniLM-L6-v2")
PORT = int(os.environ.get("EMBED_PORT", "8085"))


@asynccontextmanager
async def lifespan(app: FastAPI):
    log.info("loading model %s", MODEL_NAME)
    app.state.model = SentenceTransformer(MODEL_NAME)
    app.state.dim = app.state.model.get_sentence_embedding_dimension()
    log.info("model ready, dim=%d", app.state.dim)
    yield


app = FastAPI(lifespan=lifespan)


class EmbedRequest(BaseModel):
    texts: list[str]


@app.get("/healthz")
def healthz():
    # Ready once the lifespan has loaded the model and set the dimension.
    return {"status": "ok", "model": MODEL_NAME, "dim": app.state.dim}


@app.post("/embed")
def embed(req: EmbedRequest):
    if not req.texts:
        raise HTTPException(status_code=400, detail="texts must not be empty")
    # Normalise here so Go only has to take a dot product.
    vectors = app.state.model.encode(
        req.texts, normalize_embeddings=True, convert_to_numpy=True
    )
    return {"model": MODEL_NAME, "vectors": vectors.tolist()}


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=PORT)
