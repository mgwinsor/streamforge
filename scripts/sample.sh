#!/usr/bin/env bash
set -euo pipefail
mkdir -p samples
ffmpeg -hide_banner -loglevel error -y -f lavfi -i 'testsrc2=size=1280x720:rate=30' -f lavfi -i 'sine=frequency=440:sample_rate=48000' -t 9 -c:v libx264 -threads 2 -preset ultrafast -pix_fmt yuv420p -c:a aac -movflags +faststart samples/demo.mp4
ffmpeg -hide_banner -loglevel error -y -i samples/demo.mp4 -c:v copy -an samples/silent.mp4
