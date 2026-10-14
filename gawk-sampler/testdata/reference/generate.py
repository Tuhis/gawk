"""Regenerates the post-processing reference vectors (docs/71 CF5, §9.6).

For each photograph it runs NudeNet itself — its own pre-processing, its own
ONNX session, its own post-processing — and records two things:

  * the candidate rows of the raw model output: every anchor whose best class
    score is at least ROW_FLOOR (NudeNet keeps nothing under 0.2, so rows
    between ROW_FLOOR and 0.2 test that cut-off too), as
    [anchor, cx, cy, w, h, score_0 … score_17];
  * NudeNet's detections for the same image.

The Go test feeds the rows to the sampler's own post-processing and requires
the same detections, each score within 1e-3. Only these numbers are
committed: not the photographs (their sources are in SOURCES.md) and not the
model. This script uses NudeNet by calling it; none of its code is copied into
the repository (docs/71 §8.1).

    pip install nudenet==3.4.2 onnxruntime opencv-python-headless numpy
    python3 generate.py <photos-dir> <320n.onnx> <out-dir>

where <photos-dir> holds the photographs named <nasa_id>.jpg, and
<320n.onnx> is NudeNet's v3.4-weights 320n model (SHA-256
c15d8273adad2d0a92f014cc69ab2d6c311a06777a55545f2c4eb46f51911f0f).
"""

import json
import os
import sys

import numpy as np
from nudenet import NudeDetector
from nudenet import nudenet as nudenet_module

INPUT = 320
ROW_FLOOR = 0.15


def short(v):
    # The shortest decimal that round-trips the float32 the model produced.
    return float(np.format_float_positional(np.float32(v), unique=True, trim="-"))


def main():
    photos, model, out = sys.argv[1], sys.argv[2], sys.argv[3]
    detector = NudeDetector(model_path=model, inference_resolution=INPUT)
    names = sorted(n for n in os.listdir(photos) if n.endswith(".jpg"))
    for name in names:
        path = os.path.join(photos, name)
        blob, _, _, _, _, width, height = nudenet_module._read_image(path, INPUT)
        raw = detector.onnx_session.run(None, {detector.input_name: blob})[0][0]
        rows = []
        for anchor in range(raw.shape[1]):
            column = raw[:, anchor]
            if column[4:].max() >= ROW_FLOOR:
                rows.append([anchor] + [short(v) for v in column])
        detections = [
            {"label": d["class"], "score": short(d["score"]), "box": d["box"]}
            for d in detector.detect(path)
        ]
        doc = {
            "image": name[: -len(".jpg")],
            "width": int(width),
            "height": int(height),
            "inputSize": INPUT,
            "anchors": int(raw.shape[1]),
            "rows": rows,
            "detections": detections,
        }
        with open(os.path.join(out, name[: -len(".jpg")] + ".json"), "w") as f:
            json.dump(doc, f, separators=(",", ":"))
            f.write("\n")
        print(name, len(rows), "rows", len(detections), "detections")


if __name__ == "__main__":
    main()
