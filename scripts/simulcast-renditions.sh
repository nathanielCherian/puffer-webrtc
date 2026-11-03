#!/usr/bin/env bash
# Produce a shared simulcast source for testing without the sfu transcoder:
# one ffmpeg process decodes the file once (looped, real time) and encodes it
# into several H.264 renditions with IDRs at the same timestamps, each sent as
# MPEG-TS over UDP to its own port, which is what `./bin/server --simulcast`
# listens on. All renditions share the input's timeline (one 90 kHz clock), so
# the server can switch between them at a keyframe without a timestamp jump.
#
#   ./scripts/simulcast-renditions.sh media/standin.mp4
#   ./bin/server --simulcast high=127.0.0.1:5001@2500k,mid=127.0.0.1:5002@800k,low=127.0.0.1:5003@250k
#
# Environment:
#   LAYERS  name:WxH:bitrate:port,...   (default below; bitrate in ffmpeg units, e.g. 800k)
#   HOST    destination host             (default 127.0.0.1)
#   GOP     seconds between IDRs         (default 2; bounds startup, switch and PLI recovery time)
#   FPS     output frame rate            (default 30)
#   PRESET  x264 preset                  (default veryfast)
set -euo pipefail

SRC="${1:-media/standin.mp4}"
LAYERS="${LAYERS:-high:1280x720:2500k:5001,mid:640x360:800k:5002,low:426x240:250k:5003}"
HOST="${HOST:-127.0.0.1}"
GOP="${GOP:-2}"
FPS="${FPS:-30}"
PRESET="${PRESET:-veryfast}"

[ -f "$SRC" ] || { echo "no such file: $SRC (run scripts/make-standin.sh)" >&2; exit 1; }

IFS=',' read -r -a layers <<< "$LAYERS"
n=${#layers[@]}
gop_frames=$((GOP * FPS))

# split the decoded video n ways, scale each
split="[0:v]fps=${FPS},format=yuv420p,split=${n}"
scales=""
for i in "${!layers[@]}"; do
  IFS=':' read -r name size rate port <<< "${layers[$i]}"
  split+="[s$i]"
  scales+=";[s$i]scale=${size/x/:}[o$i]"
done
filter="${split}${scales}"

outputs=()
spec=""
for i in "${!layers[@]}"; do
  IFS=':' read -r name size rate port <<< "${layers[$i]}"
  outputs+=(
    -map "[o$i]" -map "0:a?"
    -c:v libx264 -preset "$PRESET" -tune zerolatency -profile:v baseline -level 3.1 -pix_fmt yuv420p
    -b:v "$rate" -minrate "$rate" -maxrate "$rate" -bufsize "$rate"
    -g "$gop_frames" -keyint_min "$gop_frames" -sc_threshold 0 -bf 0
    -force_key_frames "expr:gte(t,n_forced*${GOP})"
    -c:a aac -b:a 128k -ar 48000 -ac 2
    -f mpegts -mpegts_flags +initial_discontinuity "udp://${HOST}:${port}?pkt_size=1316"
  )
  spec+="${spec:+,}${name}=${HOST}:${port}@${rate}"
  echo "rendition ${name}: ${size} @ ${rate} -> udp://${HOST}:${port}" >&2
done
echo "server flag: --simulcast ${spec}" >&2

exec ffmpeg -hide_banner -loglevel warning -nostats \
  -re -stream_loop -1 -i "$SRC" \
  -filter_complex "$filter" \
  "${outputs[@]}"
