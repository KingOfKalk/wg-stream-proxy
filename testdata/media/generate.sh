#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Regenerates the media fixtures of the player tests (docs/testing.md, L4)
# in this directory. Run from anywhere on a host with Docker:
#
#   testdata/media/generate.sh
#
# It re-runs itself in the pinned image below with ffmpeg 8.1.3 from the
# Alpine 3.24 package repository. Alpine keeps only the latest package
# version, so when that pin stops resolving, bump IMAGE and FFMPEG to the
# current releases and regenerate everything. The output is not
# byte-for-byte reproducible across ffmpeg versions; the tests only need
# it to decode.
#
# All content is synthetic (test pattern and sine tones); key.bin is a
# test-only key.
set -eu

IMAGE=alpine:3.24.4
FFMPEG=ffmpeg=8.1.3-r0

if [ "${1:-}" != --in-container ]; then
	dir=$(cd "$(dirname "$0")" && pwd)
	exec docker run --rm -v "$dir":/media -w /media "$IMAGE" \
		sh -c "apk add --no-cache $FFMPEG >/dev/null && sh ./generate.sh --in-container $(id -u):$(id -g)"
fi
owner=$2

ffmpeg -hide_banner -version | head -n 1

rm -rf ts fmp4 aes demuxed radio dash
mkdir -p ts fmp4 aes demuxed/video demuxed/audio radio dash/seg

# Sources: 12 s of a 192x108 test pattern at 25 fps and a 440 Hz tone.
DURATION=12
VIDEO="testsrc2=size=192x108:rate=25:duration=$DURATION"
AUDIO="sine=frequency=440:sample_rate=48000:duration=$DURATION"

# H.264 with a keyframe every 2 s (50 frames), so every segment is
# exactly 2 s; AAC mono. Single-threaded and bitexact for stable output.
V="-c:v libx264 -preset veryfast -profile:v baseline -pix_fmt yuv420p -b:v 48k -maxrate 64k -bufsize 128k -g 50 -keyint_min 50 -sc_threshold 0 -threads 1"
A="-c:a aac -b:a 32k -ac 1"
COMMON="-fflags +bitexact -flags:v +bitexact -flags:a +bitexact"
# VOD media playlists; the fixture server derives the live window.
HLS="-f hls -hls_time 2 -hls_list_size 0 -hls_playlist_type vod"

run() {
	echo "+ ffmpeg $*"
	# shellcheck disable=SC2068 # word splitting of the option strings is intended
	ffmpeg -hide_banner -loglevel error -y $@
}

# ts: H.264 + AAC in MPEG-TS segments.
run -f lavfi -i "$VIDEO" -f lavfi -i "$AUDIO" $V $A $COMMON $HLS \
	-hls_segment_filename ts/seg%d.ts ts/index.m3u8

# fmp4: H.264 + AAC in fragmented MP4 with an init segment.
run -f lavfi -i "$VIDEO" -f lavfi -i "$AUDIO" $V $A $COMMON $HLS \
	-hls_segment_type fmp4 -hls_fmp4_init_filename init.mp4 \
	-hls_segment_filename fmp4/seg%d.m4s fmp4/index.m3u8

# aes: MPEG-TS segments encrypted with AES-128 (test-only key, explicit IV).
printf 'test-only-key-01' > aes/key.bin
keyinfo=$(mktemp)
printf 'key.bin\naes/key.bin\n00000000000000000000000000000001\n' > "$keyinfo"
run -f lavfi -i "$VIDEO" -f lavfi -i "$AUDIO" $V $A $COMMON $HLS \
	-hls_key_info_file "$keyinfo" \
	-hls_segment_filename aes/seg%d.ts aes/index.m3u8
rm -f "$keyinfo"

# demuxed: a video-only variant and an audio-only rendition (both TS).
run -f lavfi -i "$VIDEO" -an $V $COMMON $HLS \
	-hls_segment_filename demuxed/video/seg%d.ts demuxed/video/index.m3u8
run -f lavfi -i "$AUDIO" -vn $A $COMMON $HLS \
	-hls_segment_filename demuxed/audio/seg%d.ts demuxed/audio/index.m3u8

# radio: 10 s of a 660 Hz tone as MP3 without ID3 tag or Xing frame, so
# the fixture server can loop it as an endless Icecast-like stream.
run -f lavfi -i "sine=frequency=660:sample_rate=44100:duration=10" \
	-c:a libmp3lame -b:a 32k -ac 1 -id3v2_version 0 -write_xing 0 $COMMON radio/tone.mp3

# dash: H.264 + AAC as MPEG-DASH (VOD), shaped like a broadcaster's live
# packager: SegmentTemplate with $RepresentationID$ and $Time$, a
# SegmentTimeline, and the segments below a relative Period BaseURL.
tmp=$(mktemp -d)
echo "+ ffmpeg (dash)"
# shellcheck disable=SC2086 # word splitting of the option strings is intended
ffmpeg -hide_banner -loglevel error -y -f lavfi -i "$VIDEO" -f lavfi -i "$AUDIO" $V $A $COMMON \
	-avoid_negative_ts make_zero -f dash -seg_duration 2 -use_template 1 -use_timeline 1 \
	-init_seg_name 'init-$RepresentationID$.m4s' -media_seg_name 'chunk-$RepresentationID$-$Time$.m4s' \
	-adaptation_sets 'id=0,streams=v id=1,streams=a' "$tmp/manifest.mpd"
mv "$tmp"/*.m4s dash/seg/
sed 's|\(<Period[^>]*>\)|\1\n\t\t<BaseURL>seg/</BaseURL>|' "$tmp/manifest.mpd" > dash/manifest.mpd
rm -rf "$tmp"

# Master playlists, written by hand.
master() {
	printf '#EXTM3U\n#EXT-X-VERSION:%s\n#EXT-X-INDEPENDENT-SEGMENTS\n%s' "$2" "$3" > "$1/master.m3u8"
}
master ts 3 '#EXT-X-STREAM-INF:BANDWIDTH=200000,RESOLUTION=192x108
index.m3u8
'
master fmp4 7 '#EXT-X-STREAM-INF:BANDWIDTH=200000,RESOLUTION=192x108
index.m3u8
'
master aes 3 '#EXT-X-STREAM-INF:BANDWIDTH=200000,RESOLUTION=192x108
index.m3u8
'
master demuxed 3 '#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="Tone",LANGUAGE="en",DEFAULT=YES,AUTOSELECT=YES,URI="audio/index.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=200000,RESOLUTION=192x108,AUDIO="aud"
video/index.m3u8
'

chown -R "$owner" ts fmp4 aes demuxed radio dash
du -sh .
