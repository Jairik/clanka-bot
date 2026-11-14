"""Convert the trained ONNX model to a float32 TFLite and check it agrees numerically.

onnx2tf treats a 3-D ONNX input as (batch, channels, width) and transposes it to
(batch, width, channels), which would hand the wake word service a detector that
wants (batch, 96, 16). openWakeWord feeds (batch, 16 frames, 96 features), so the
ONNX gets an extra transposed input first; onnx2tf then transposes it back and the
TFLite input comes out as (1, 16, 96), matching the upstream models.
"""

import argparse
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

import numpy as np
import onnx
import onnxruntime as ort
from onnx import TensorProto, helper


def wrap_input(onnx_path, wrapped_path):
    """Add a (1, 96, 16) input that transposes into the model's (1, 16, 96) input."""
    model = onnx.load(str(onnx_path))
    original = model.graph.input[0]
    shape = [dim.dim_value for dim in original.type.tensor_type.shape.dim]
    if len(shape) != 3:
        raise SystemExit(f"unexpected ONNX input shape {shape}")
    wrapped_shape = [shape[0], shape[2], shape[1]]
    model.graph.node.insert(
        0, helper.make_node("Transpose", ["features"], [original.name], perm=[0, 2, 1])
    )
    del model.graph.input[:]
    model.graph.input.append(helper.make_tensor_value_info("features", TensorProto.FLOAT, wrapped_shape))
    onnx.checker.check_model(model)
    onnx.save(model, str(wrapped_path))
    return shape


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--onnx", required=True)
    p.add_argument("--out", required=True)
    args = p.parse_args()
    onnx_path = Path(args.onnx)
    input_name = onnx.load(str(onnx_path)).graph.input[0].name
    sess = ort.InferenceSession(str(onnx_path))

    # onnx2tf lives in the conversion venv, which is not on PATH when this
    # script runs under that venv's own interpreter.
    onnx2tf = Path(sys.executable).with_name("onnx2tf")
    if not onnx2tf.exists():
        found = shutil.which("onnx2tf")
        if not found:
            raise SystemExit("onnx2tf not found; run setup to create venv-convert")
        onnx2tf = Path(found)

    with tempfile.TemporaryDirectory() as tmp:
        wrapped = Path(tmp) / "wrapped.onnx"
        shape = wrap_input(onnx_path, wrapped)
        subprocess.run([str(onnx2tf), "-i", str(wrapped), "-o", tmp, "-osd"], check=True)
        tflite_path = Path(tmp) / "wrapped_float32.tflite"

        import ai_edge_litert.interpreter as litert

        interp = litert.Interpreter(model_path=str(tflite_path))
        interp.allocate_tensors()
        detail = interp.get_input_details()[0]
        if list(detail["shape"]) != shape:
            raise SystemExit(f"tflite input shape {detail['shape']} != onnx {shape}")

        rng = np.random.default_rng(0)
        worst = 0.0
        for _ in range(5):
            # openWakeWord feeds (1, 16, 96); the ONNX itself is fed the same
            # layout, since the transpose only exists in the wrapped copy.
            x = rng.random(tuple(shape), dtype=np.float32)
            expected = sess.run(None, {input_name: x})[0].ravel()
            interp.set_tensor(detail["index"], x)
            interp.invoke()
            got = interp.get_tensor(interp.get_output_details()[0]["index"]).ravel()
            worst = max(worst, float(np.max(np.abs(expected - got))))
            if not np.allclose(expected, got, atol=1e-4):
                raise SystemExit(f"tflite mismatch: onnx {expected} tflite {got}")
        shutil.copy(tflite_path, args.out)
    print(f"wrote {args.out} input {shape}, max |onnx - tflite| {worst:.2e}")


if __name__ == "__main__":
    main()
