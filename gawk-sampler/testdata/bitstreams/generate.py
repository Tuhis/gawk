"""Regenerates the sampler's bitstream fixtures (docs/71 CF5).

One keyframe per codec the broadcasters send, each a few hundred bytes:

  quadrants.h264  H.264 Annex B (SPS, PPS, IDR), 160x96: red top-left,
                  green top-right, blue bottom-left, white bottom-right
  quadrants.vp8   one raw VP8 keyframe of the same picture
  quadrants.vp9   one raw VP9 keyframe of the same picture
  magenta.h264    H.264 Annex B, 160x90 solid magenta (the 90 exercises the
                  SPS cropping a 16:9 frame needs)

The raw VP8/VP9 frames are what a broadcaster puts in a StreamFrame payload:
the IVF container is stripped here and rebuilt by the sampler. The AVCC
form the web broadcaster sends is built from the Annex B fixtures by the Go
tests (the avcC record plus length-prefixed NAL units), so its conversion
back is tested against bytes the encoder wrote rather than bytes a test
invented.

    pip install pillow
    python3 generate.py <ffmpeg>

<ffmpeg> is any ffmpeg built with libx264 and libvpx (the sampler's own
minimal build has decoders only). SEI NAL units are removed so the H.264
fixtures carry no encoder banner.
"""

import os
import struct
import subprocess
import sys
import tempfile

from PIL import Image

HERE = os.path.dirname(os.path.abspath(__file__))


def quadrants(w, h):
    img = Image.new("RGB", (w, h))
    px = img.load()
    for y in range(h):
        for x in range(w):
            top, left = y < h // 2, x < w // 2
            px[x, y] = {
                (True, True): (255, 0, 0),
                (True, False): (0, 255, 0),
                (False, True): (0, 0, 255),
                (False, False): (255, 255, 255),
            }[(top, left)]
    return img


def encode(ffmpeg, png, args, out):
    subprocess.run([ffmpeg, "-y", "-loglevel", "error", "-i", png, "-frames:v", "1", "-pix_fmt", "yuv420p"] + args + [out], check=True)


def ivf_first_frame(path):
    with open(path, "rb") as f:
        data = f.read()
    assert data[:4] == b"DKIF", "not an IVF file"
    header_len = struct.unpack_from("<H", data, 6)[0]
    size = struct.unpack_from("<I", data, header_len)[0]
    start = header_len + 12
    return data[start : start + size]


def main():
    ffmpeg = sys.argv[1]
    h264 = ["-c:v", "libx264", "-profile:v", "baseline", "-preset", "ultrafast", "-x264-params", "annexb=1",
            "-bsf:v", "filter_units=remove_types=6", "-f", "h264"]
    with tempfile.TemporaryDirectory() as tmp:
        q = os.path.join(tmp, "q.png")
        quadrants(160, 96).save(q)
        m = os.path.join(tmp, "m.png")
        Image.new("RGB", (160, 90), (255, 0, 255)).save(m)

        encode(ffmpeg, q, h264, os.path.join(HERE, "quadrants.h264"))
        encode(ffmpeg, m, h264, os.path.join(HERE, "magenta.h264"))
        for codec, ext in (("libvpx", "vp8"), ("libvpx-vp9", "vp9")):
            ivf = os.path.join(tmp, "q." + ext + ".ivf")
            encode(ffmpeg, q, ["-c:v", codec, "-b:v", "200k", "-f", "ivf"], ivf)
            with open(os.path.join(HERE, "quadrants." + ext), "wb") as f:
                f.write(ivf_first_frame(ivf))
    for name in sorted(os.listdir(HERE)):
        if not name.endswith(".py") and not name.endswith(".md"):
            print(name, os.path.getsize(os.path.join(HERE, name)), "bytes")


if __name__ == "__main__":
    main()
