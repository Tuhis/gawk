#!/bin/sh
# Builds the sampler's minimal ffmpeg (docs/71 D5, §9.5) into the prefix given
# as the first argument: the h264, vp8 and vp9 decoders and nothing a
# keyframe-to-one-RGB-frame pipe does not need, so a decoder CVE has the
# smallest surface the three broadcaster codecs allow.
#
#   build-ffmpeg.sh <prefix> [<workdir>]
#
# LGPL-2.1-or-later only: no --enable-gpl, no --enable-nonfree, nothing
# autodetected. The sampler runs the result as a separate executable and never
# links it (D4). The image's Dockerfile runs this script; a developer runs it
# to get the ffmpeg the native tests need (GAWK_SAMPLER_FFMPEG).
#
# Besides D5's list, the pipe needs four pieces the CLI pulls in implicitly:
# the h264 parser (the raw h264 demuxer frames access units with it), the vp8
# and vp9 parsers, the vp9_superframe_split bitstream filter (the vp9 decoder
# inserts it itself) and the pad filter (§9.5's letterbox).
set -eu

FFMPEG_VERSION=9.0.2
# The release tarball's SHA-256, pinned: a swapped file must not build.
FFMPEG_SHA256=8c3850283eb25fa026482078a04051e0be17347b09ef81a0849bec15a96e002e
FFMPEG_URL="https://ffmpeg.org/releases/ffmpeg-${FFMPEG_VERSION}.tar.xz"

prefix=${1:?usage: build-ffmpeg.sh <prefix> [<workdir>]}
work=${2:-$(mktemp -d)}
mkdir -p "$prefix" "$work"

cd "$work"
if [ ! -f "ffmpeg-${FFMPEG_VERSION}.tar.xz" ]; then
	curl -fsSL -o "ffmpeg-${FFMPEG_VERSION}.tar.xz" "$FFMPEG_URL"
fi
echo "${FFMPEG_SHA256}  ffmpeg-${FFMPEG_VERSION}.tar.xz" | sha256sum -c -
rm -rf "ffmpeg-${FFMPEG_VERSION}"
tar -xJf "ffmpeg-${FFMPEG_VERSION}.tar.xz"
cd "ffmpeg-${FFMPEG_VERSION}"

# nasm buys the decoders their SIMD paths; without it the build still works,
# only slower per sample, which the effective-interval gauge would show.
asm=--disable-x86asm
if command -v nasm >/dev/null 2>&1; then
	asm=
fi

# shellcheck disable=SC2086 # $asm is empty or one word
./configure \
	--prefix="$prefix" \
	--disable-everything \
	--disable-autodetect \
	--disable-doc \
	--disable-debug \
	--disable-network \
	--disable-ffplay \
	--disable-ffprobe \
	--disable-avdevice \
	--disable-swresample \
	--enable-ffmpeg \
	--enable-avfilter \
	--enable-swscale \
	--enable-static \
	--disable-shared \
	--enable-decoder=h264,vp8,vp9 \
	--enable-parser=h264,vp8,vp9 \
	--enable-demuxer=h264,ivf \
	--enable-bsf=vp9_superframe_split \
	--enable-protocol=pipe \
	--enable-encoder=rawvideo \
	--enable-muxer=rawvideo \
	--enable-filter=scale,format,pad,buffer,buffersink \
	$asm

make -j"$(nproc)"
make install

# What the image must carry beside the binary to convey it under the LGPL:
# the licence text and where the exact source came from.
mkdir -p "$prefix/share/doc/ffmpeg"
cp COPYING.LGPLv2.1 "$prefix/share/doc/ffmpeg/"
{
	echo "ffmpeg ${FFMPEG_VERSION}, LGPL-2.1-or-later"
	echo "source: ${FFMPEG_URL}"
	echo "sha256: ${FFMPEG_SHA256}"
	echo "built by gawk-sampler/deploy/build-ffmpeg.sh; configure line:"
	sed -n 's/^FFMPEG_CONFIGURATION=//p' ffbuild/config.mak
} >"$prefix/share/doc/ffmpeg/BUILD-INFO"
