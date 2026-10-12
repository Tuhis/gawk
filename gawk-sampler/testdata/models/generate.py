"""Generates the sampler's test models (docs/71 §9.6).

CI and the dev stack never load NudeNet: they load these instead. Each has
NudeNet's input and output contract — input `images` [1,3,S,S] float32 RGB in
0-1, output `output0` [1,22,A] float32 in the YOLOv8 layout (4 box rows, then
one score row per label in NudeNet's label order) with A anchors for an S×S
input — so the sampler's real loading, inference and post-processing run
unchanged. Only anchor 0 is ever non-zero: a centred box whose
FEMALE_GENITALIA_EXPOSED (label index 4) score is the frame's magenta share,

    Σ R·B·(1−G) / Σ max(R, G, B)

which is ~1 for a solid magenta frame and 0 for black, and which the black
letterbox padding does not dilute (padding adds 0 to both sums).

The [1,22,A] output is built by padding a [1,22,1] tensor, so the files stay a
few hundred bytes whatever A is.

    python3 generate.py            # writes test-320.onnx and test-640.onnx here

Needs the `onnx` package (pip install onnx); nothing else.
"""

import os

import onnx
from onnx import TensorProto, helper

LABELS = 18  # NudeNet's label count; the sampler checks the output shape against it.
FLAGGED_INDEX = 4  # FEMALE_GENITALIA_EXPOSED in NudeNet's label order.


def anchors(size):
    return sum((size // stride) ** 2 for stride in (8, 16, 32))


def build(size):
    a = anchors(size)
    rows = 4 + LABELS
    half = float(size) / 2
    base = [half, half, half, half] + [0.0] * LABELS
    mask = [0.0] * rows
    mask[4 + FLAGGED_INDEX] = 1.0

    consts = [
        helper.make_tensor("idx_r", TensorProto.INT64, [], [0]),
        helper.make_tensor("idx_g", TensorProto.INT64, [], [1]),
        helper.make_tensor("idx_b", TensorProto.INT64, [], [2]),
        helper.make_tensor("one", TensorProto.FLOAT, [], [1.0]),
        helper.make_tensor("eps", TensorProto.FLOAT, [], [1e-6]),
        helper.make_tensor("base", TensorProto.FLOAT, [1, rows, 1], base),
        helper.make_tensor("mask", TensorProto.FLOAT, [1, rows, 1], mask),
        helper.make_tensor("pads", TensorProto.INT64, [6], [0, 0, 0, 0, 0, a - 1]),
    ]
    nodes = [
        helper.make_node("Gather", ["images", "idx_r"], ["r"], axis=1),
        helper.make_node("Gather", ["images", "idx_g"], ["g"], axis=1),
        helper.make_node("Gather", ["images", "idx_b"], ["b"], axis=1),
        helper.make_node("Sub", ["one", "g"], ["not_g"]),
        helper.make_node("Mul", ["r", "b"], ["rb"]),
        helper.make_node("Mul", ["rb", "not_g"], ["magenta"]),
        helper.make_node("Max", ["r", "g", "b"], ["value"]),
        helper.make_node("ReduceSum", ["magenta"], ["magenta_sum"], keepdims=0),
        helper.make_node("ReduceSum", ["value"], ["value_sum"], keepdims=0),
        helper.make_node("Add", ["value_sum", "eps"], ["value_safe"]),
        helper.make_node("Div", ["magenta_sum", "value_safe"], ["share"]),
        helper.make_node("Mul", ["mask", "share"], ["scored"]),
        helper.make_node("Add", ["base", "scored"], ["head"]),
        helper.make_node("Pad", ["head", "pads"], ["output0"], mode="constant"),
    ]
    graph = helper.make_graph(
        nodes,
        f"gawk-sampler-test-{size}",
        [helper.make_tensor_value_info("images", TensorProto.FLOAT, [1, 3, size, size])],
        [helper.make_tensor_value_info("output0", TensorProto.FLOAT, [1, rows, a])],
        initializer=consts,
    )
    model = helper.make_model(
        graph,
        producer_name="gawk-sampler testdata/models/generate.py",
        opset_imports=[helper.make_opsetid("", 17)],
    )
    # ONNX Runtime 1.29 reads IR version 10; pin it rather than inherit
    # whatever the installed onnx package writes by default.
    model.ir_version = 10
    onnx.checker.check_model(model, full_check=True)
    return model


def main():
    here = os.path.dirname(os.path.abspath(__file__))
    for size in (320, 640):
        path = os.path.join(here, f"test-{size}.onnx")
        onnx.save(build(size), path)
        print(path, os.path.getsize(path), "bytes")


if __name__ == "__main__":
    main()
