#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Renders the built-in error clips (docs/clips.md, sections 6.4 and 7)
# into segments/ in this directory. Run from anywhere on a host with Docker:
#
#   internal/clips/generate.sh
#
# It re-runs itself in the pinned image below with ffmpeg from the Alpine
# 3.24 package repository. Alpine keeps only the latest package version, so
# when that pin stops resolving, bump IMAGE and FFMPEG to the current
# releases and regenerate everything. The output is not byte-for-byte
# reproducible across ffmpeg versions, which is why it is committed.
#
# Font: Inter, fetched by version and checked against the SHA-256 sums
# below (SIL Open Font License 1.1). Only rendered pixels ship, not the
# font. To change a text, edit CLIPS below, rerun, commit the result and
# update the names in clips.go.
set -eu

IMAGE=alpine:3.24.4
FFMPEG=ffmpeg=8.1.3-r0

INTER_VERSION=4.1
INTER_URL=https://github.com/rsms/inter/releases/download/v$INTER_VERSION/Inter-$INTER_VERSION.zip
INTER_ZIP_SHA256=9883fdd4a49d4fb66bd8177ba6625ef9a64aa45899767dde3d36aa425756b11e
INTER_TTF=extras/ttf/Inter-Regular.ttf
INTER_TTF_SHA256=40d692fce188e4471e2b3cba937be967878f631ad3ebbbdcd587687c7ebe0c82

# One "name|text" per clip; the names are the file names and the names
# in clips.go. Texts: docs/clips.md, decision 3.
CLIPS='tunnel-unreachable|Tunnel not reachable
channel-not-available|Channel not available
channel-blocked|Channel blocked by configuration'

if [ "${1:-}" != --in-container ]; then
	dir=$(cd "$(dirname "$0")" && pwd)
	exec docker run --rm -v "$dir":/clips -w /clips "$IMAGE" \
		sh -c "apk add --no-cache $FFMPEG >/dev/null && sh ./generate.sh --in-container $(id -u):$(id -g)"
fi
owner=$2

ffmpeg -hide_banner -version | head -n 1

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Font, by version and checksum.
wget -q -O "$work/inter.zip" "$INTER_URL"
echo "$INTER_ZIP_SHA256  $work/inter.zip" | sha256sum -c -
unzip -q -o -j -d "$work" "$work/inter.zip" "$INTER_TTF"
font=$work/Inter-Regular.ttf
echo "$INTER_TTF_SHA256  $font" | sha256sum -c -

# Format (docs/clips.md, 6.4): 4.8 s = 120 frames at 25 fps = 230400 samples
# at 48 kHz = exactly 225 AAC frames of 1024 samples. The video is H.264
# High level 4.0 with one IDR frame and no B-frames, single-threaded and
# bitexact for stable output. Timestamps start at 10 s, so that the PCR,
# which the muxer starts 0.96 s earlier, never wraps below zero.
SIZE=1920x1080
DURATION=4.8
V="-c:v libx264 -preset slow -tune stillimage -crf 28 -profile:v high -level 4.0 -g 120 -keyint_min 120 -sc_threshold 0 -bf 0 -threads 1"
A="-c:a aac -b:a 64k -ac 2 -ar 48000"
COMMON="-fflags +bitexact -flags:v +bitexact -flags:a +bitexact"

run() {
	echo "+ ffmpeg $*"
	ffmpeg -nostdin -hide_banner -loglevel error -y "$@"
}

# probe TYPE ENTRIES FILE [ffprobe options]: "key=value" lines of the
# entries of the streams of one type (v or a).
probe() {
	ffprobe -v error -select_streams "$1" -show_entries "$2" -of default=nw=1 ${4:-} "$3"
}

fail() {
	echo "check failed: $*" >&2
	exit 1
}

# first_last KEY: the first and the last "KEY=value" line of stdin, joined.
first_last() {
	grep "^$1=" | sed -n '1p;$p' | tr '\n' ' '
}

# check NAME: the format of docs/clips.md 6.4 as ffprobe sees it.
check() {
	f=segments/$1.ts
	got=$(probe v stream=codec_name,profile,level,width,height,pix_fmt,r_frame_rate,has_b_frames "$f" | sort -u | tr '\n' ' ')
	[ "$got" = "codec_name=h264 has_b_frames=0 height=1080 level=40 pix_fmt=yuv420p profile=High r_frame_rate=25/1 width=1920 " ] || fail "$f video: $got"
	got=$(probe a stream=codec_name,profile,channels,sample_rate "$f" | sort -u | tr '\n' ' ')
	[ "$got" = "channels=2 codec_name=aac profile=LC sample_rate=48000 " ] || fail "$f audio: $got"
	# 120 video frames, exactly 225 AAC frames.
	got=$(probe v stream=nb_read_packets "$f" -count_packets | sort -u | tr '\n' ' ')
	[ "$got" = "nb_read_packets=120 " ] || fail "$f: video packets: $got, want 120"
	got=$(probe a stream=nb_read_packets "$f" -count_packets | sort -u | tr '\n' ' ')
	[ "$got" = "nb_read_packets=225 " ] || fail "$f: audio packets: $got, want 225"
	# Timestamps start at 10 s and both tracks end at 14.8 s (90 kHz ticks).
	got=$(probe v packet=pts,dts "$f" | first_last pts)
	[ "$got" = "pts=900000 pts=1328400 " ] || fail "$f video pts: $got"
	got=$(probe v packet=pts,dts "$f" | first_last dts)
	[ "$got" = "dts=900000 dts=1328400 " ] || fail "$f video dts: $got"
	got=$(probe a packet=pts,dts,duration "$f" | first_last pts)
	[ "$got" = "pts=900000 pts=1330080 " ] || fail "$f audio pts: $got"
	got=$(probe a packet=pts,dts,duration "$f" | first_last duration)
	[ "$got" = "duration=1920 duration=1920 " ] || fail "$f audio duration: $got"
	# One IDR (key) frame, the first one; the rest P-frames, no B-frames.
	got=$(probe v frame=key_frame,pict_type "$f" | paste -d' ' - - | sed -n 1p)
	[ "$got" = "key_frame=1 pict_type=I" ] || fail "$f: first frame: $got"
	got=$(probe v frame=key_frame,pict_type "$f" | paste -d' ' - - | sort | uniq -c | tr -s ' ' | tr '\n' ';')
	[ "$got" = " 119 key_frame=0 pict_type=P; 1 key_frame=1 pict_type=I;" ] || fail "$f frame types: $got"
	# SPS and PPS (NAL types 7 and 8) of every video frame; the sum must be
	# the same across the segments.
	sum=$(ffmpeg -v error -i "$f" -map 0:v -c:v copy -bsf:v filter_units=pass_types=7-8 -f h264 - | sha256sum | cut -d' ' -f1)
	if [ -z "${param_sum:-}" ]; then
		param_sum=$sum
	elif [ "$sum" != "$param_sum" ]; then
		fail "$f: SPS/PPS differ from the first segment"
	fi
	echo "ok $f $(wc -c < "$f") bytes"
}

mkdir -p segments
rm -f segments/*.ts

echo "$CLIPS" | while IFS='|' read -r name text; do
	# Render: the audio track has a priming frame, so the encoder emits 226
	# AAC frames for 120 frames of input, and the muxer starts both tracks one
	# AAC frame in.
	run -f lavfi -i "color=c=0x101010:size=$SIZE:rate=25:duration=$DURATION,drawtext=fontfile=$font:text='$text':fontcolor=0xE0E0E0:fontsize=64:x=(w-tw)/2:y=(h-th)/2,format=yuv420p" \
		-f lavfi -i "anullsrc=r=48000:cl=stereo" -t "$DURATION" \
		$V $A $COMMON -f mpegts "$work/$name.ts"
	# Cut: drop the priming AAC frame (the first 1024 samples, silence
	# either way) so that exactly 225 remain, starting at the same time as
	# the video; timestamps start at 10 s. -muxdelay lets the muxer pack the
	# AAC frames into few PES packets (and offsets all timestamps by twice
	# that delay, which -output_ts_offset takes out, and adds the 10 s).
	run -i "$work/$name.ts" -map 0 -c copy -ss 0.021333 $COMMON \
		-muxdelay 0.96 -muxpreload 0 -output_ts_offset 8.08 -avoid_negative_ts disabled \
		-f mpegts "segments/$name.ts"
done

# check runs in this shell (not in the pipe above) to keep param_sum.
for name in $(echo "$CLIPS" | cut -d'|' -f1); do
	check "$name"
done

chown -R "$owner" segments
du -ch segments/*.ts | tail -n 1
