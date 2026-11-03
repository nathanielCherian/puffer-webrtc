#!/usr/bin/env bash
# Generate a stand-in test video: 1280x720@30 test pattern with an on-screen
# timecode + frame counter, and audio (a quiet tone plus a 1 kHz beep at the
# start of every second, aligned with the on-screen seconds for A/V sync checks).
set -euo pipefail

OUT="${1:-media/standin.mp4}"
DUR="${DUR:-60}"
mkdir -p "$(dirname "$OUT")"

# drawtext needs libfreetype; fall back to a plain pattern if it's missing.
if ffmpeg -hide_banner -filters 2>/dev/null | grep -q ' drawtext '; then
  VF="drawtext=text='%{pts\\:hms}  frame %{n}':x=40:y=40:fontsize=48:fontcolor=white:box=1:boxcolor=black@0.6:boxborderw=12"
else
  VF="null"
fi

ffmpeg -hide_banner -y \
  -f lavfi -i "testsrc2=size=1280x720:rate=30:duration=${DUR}" \
  -f lavfi -i "aevalsrc='0.1*sin(2*PI*220*t) + 0.5*sin(2*PI*1000*t)*lt(mod(t\,1)\,0.1)':s=48000:c=stereo:d=${DUR}" \
  -vf "$VF" \
  -c:v libx264 -preset medium -crf 18 -pix_fmt yuv420p -g 60 \
  -c:a aac -b:a 128k \
  -movflags +faststart \
  "$OUT"

echo "wrote $OUT"
