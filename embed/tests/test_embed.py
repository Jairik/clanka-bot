"""Tests for the embed service bounds, serialization, and health reporting."""

import sys
import threading
from pathlib import Path

import pytest
from starlette.testclient import TestClient

# Add the embed package directory to sys.path so app is importable.
embed_dir = str(Path(__file__).resolve().parent.parent)
if embed_dir not in sys.path:
    sys.path.insert(0, embed_dir)

from app import MAX_BATCH_SIZE, MAX_TEXT_LENGTH, app


# Stand in for SentenceTransformer without requiring PyTorch.
class FakeModel:
    """Stands in for SentenceTransformer without requiring PyTorch."""

    def __init__(self, dim: int = 384):
        self.dim = dim
        self.calls = []

    # Return the simulated embedding dimension.
    def get_sentence_embedding_dimension(self) -> int:
        return self.dim

    # Produce dummy unit vectors for each input text.
    def encode(
        self,
        texts: list[str],
        normalize_embeddings: bool = True,
        convert_to_numpy: bool = True,
    ):
        self.calls.append(texts)
        return [[0.1] * self.dim for _ in texts]


# Stand in for SentenceTransformer with thread synchronization to test serialization.
class BlockingFakeModel:
    """Stands in for SentenceTransformer with thread synchronization."""

    def __init__(self, dim: int = 384):
        self.dim = dim
        self.started_event = threading.Event()
        self.release_event = threading.Event()
        self.active_calls = 0
        self.max_concurrent_calls = 0
        self.lock = threading.Lock()

    # Return the simulated embedding dimension.
    def get_sentence_embedding_dimension(self) -> int:
        return self.dim

    # Block until released while tracking concurrent executions.
    def encode(
        self,
        texts: list[str],
        normalize_embeddings: bool = True,
        convert_to_numpy: bool = True,
    ):
        with self.lock:
            self.active_calls += 1
            if self.active_calls > self.max_concurrent_calls:
                self.max_concurrent_calls = self.active_calls
        self.started_event.set()
        self.release_event.wait(timeout=5.0)
        with self.lock:
            self.active_calls -= 1
        return [[0.1] * self.dim for _ in texts]


# Provide a test client configured with a fresh FakeModel on app.state.
@pytest.fixture
def client():
    fake_model = FakeModel(dim=384)
    app.state.model = fake_model
    app.state.dim = fake_model.get_sentence_embedding_dimension()
    app.state.pending = 0
    app.state.lock = threading.Lock()
    app.state.pending_lock = threading.Lock()
    return TestClient(app)


# Verify healthz reports status, model name, dimension, and pending count.
def test_healthz_reports_dimension_and_pending_count(client):
    response = client.get("/healthz")
    assert response.status_code == 200
    data = response.json()
    assert data["status"] == "ok"
    assert data["dim"] == 384
    assert data["pending"] == 0
    assert "model" in data


# Verify that a normal batch returns one vector per text.
def test_normal_batch_returns_one_vector_per_text(client):
    texts = ["turn on the lights", "play some jazz", "what time is it"]
    response = client.post("/embed", json={"texts": texts})
    assert response.status_code == 200
    data = response.json()
    assert "vectors" in data
    assert len(data["vectors"]) == len(texts)
    for vec in data["vectors"]:
        assert len(vec) == 384
    # Ensure the model was called with the exact texts.
    assert app.state.model.calls == [texts]


# Verify that an empty text list is rejected with 400.
def test_empty_list_rejected_with_400(client):
    response = client.post("/embed", json={"texts": []})
    assert response.status_code == 400
    data = response.json()
    assert "detail" in data
    assert "empty" in data["detail"].lower()
    # Ensure encode was not called.
    assert app.state.model.calls == []


# Verify that a batch exceeding MAX_BATCH_SIZE is rejected with 413.
def test_over_limit_batch_rejected_with_413(client):
    texts = ["sample text"] * (MAX_BATCH_SIZE + 1)
    response = client.post("/embed", json={"texts": texts})
    assert response.status_code == 413
    data = response.json()
    assert "detail" in data
    assert str(MAX_BATCH_SIZE) in data["detail"]
    assert "batch size" in data["detail"].lower()
    # Ensure encode was not called on an over-limit batch.
    assert app.state.model.calls == []


# Verify that a batch exactly at MAX_BATCH_SIZE is accepted.
def test_batch_at_limit_accepted(client):
    texts = ["boundary test"] * MAX_BATCH_SIZE
    response = client.post("/embed", json={"texts": texts})
    assert response.status_code == 200
    data = response.json()
    assert len(data["vectors"]) == MAX_BATCH_SIZE


# Verify that any text exceeding MAX_TEXT_LENGTH is rejected with 413.
def test_over_long_text_rejected_with_413(client):
    long_text = "a" * (MAX_TEXT_LENGTH + 1)
    response = client.post("/embed", json={"texts": ["short", long_text]})
    assert response.status_code == 413
    data = response.json()
    assert "detail" in data
    assert str(MAX_TEXT_LENGTH) in data["detail"]
    assert "exceeds" in data["detail"].lower()
    # Ensure encode was not called on over-long input.
    assert app.state.model.calls == []


# Verify that a text exactly at MAX_TEXT_LENGTH is accepted.
def test_text_at_length_limit_accepted(client):
    boundary_text = "b" * MAX_TEXT_LENGTH
    response = client.post("/embed", json={"texts": [boundary_text]})
    assert response.status_code == 200
    data = response.json()
    assert len(data["vectors"]) == 1


# Verify that concurrent callers queue rather than running encode concurrently.
def test_concurrent_callers_serialize_and_update_pending_count(client):
    blocking_model = BlockingFakeModel(dim=384)
    app.state.model = blocking_model
    app.state.dim = blocking_model.get_sentence_embedding_dimension()

    results = []

    # Submit an embed request in a background thread.
    def call_embed():
        res = client.post("/embed", json={"texts": ["concurrent test"]})
        results.append(res)

    t1 = threading.Thread(target=call_embed)
    t2 = threading.Thread(target=call_embed)

    t1.start()
    # Wait until the first thread has entered encode.
    assert blocking_model.started_event.wait(timeout=3.0)

    # Dispatch second thread while the first is actively encoding.
    t2.start()

    # Pending count should reflect active and queued requests.
    health = client.get("/healthz").json()
    assert health["pending"] >= 1

    # Release the fake model to allow both requests to finish.
    blocking_model.release_event.set()
    t1.join(timeout=3.0)
    t2.join(timeout=3.0)

    assert len(results) == 2
    assert all(r.status_code == 200 for r in results)
    # Ensure encode was serialized so at most 1 was running at a time.
    assert blocking_model.max_concurrent_calls == 1

    # After completion, pending count returns to 0.
    final_health = client.get("/healthz").json()
    assert final_health["pending"] == 0
