# puffer-webrtc-v2

A WebRTC downlink measurement rig. The server streams a pre-recorded video to a
browser with live, GCC-driven H.264 encoding, and records *everything* about the
connection, from both ends, into one directory per session. The goal is a
holistic set of logs that explains what the congestion controller saw, what it
decided, what the encoder produced, what the network did to it, and how the
browser experienced it.

```
mp4 ─ffmpeg (decode, real time, loop)─┬─ raw I420 ─► x264 (cgo) ─► H.264 track ─┐
                                      └─ Opus/Ogg ─────────────────► Opus track ─┤─► Pion ─► browser
GCC (TWCC feedback) ── target bitrate ──► x264_encoder_reconfig                  │
RTCP PLI/FIR ─► force IDR                                                        │
DataChannel "telemetry": hello, snapshots, events (server → page)                 │
DataChannel "owd" (unordered, unreliable): OWD beacons + pings (both ways) ───────┘
POST /client-log: the browser uploads its records every 2 s ◄── getStats, rVFC, events, OWD
```

Derived from [webrtc-test](https://github.com/nathanielCherian/webrtc-test), but
downlink-only, GCC-only, one video file, and rebuilt around logging:

- **Congestion controller trace**: every RTP packet handed to GCC (send time,
  transport-wide seq, size, pacer queueing) and every TWCC feedback packet
  (arrival time, per-packet receive times, raw bytes), plus every target
  change. `tools/cc_replay.py` joins them back into per-packet rows.
- **Every RTCP packet** the browser sends, decoded, with arrival time: NACKs
  (with the sequence numbers), receiver reports (loss, jitter, RTT), PLI/FIR,
  REMB, TWCC (summary; detail is in the CC trace), XR, and the retransmissions
  the NACK responder actually sent.
- **Per-frame encoder log**: size, keyframe, encode time, configured bitrate
  and the CC target at that moment, plus every reconfiguration.
- **One-way delay in both directions** on the *same path as the media*,
  measured over an unreliable DataChannel; the browser keeps every beacon and
  ping and uploads them.
- **Browser side**: `getStats()` samples with derived rates, discrete events
  from counter deltas (freezes, pauses, packet loss, retransmits received,
  NACK/PLI sent, frames dropped, keyframes, resolution/decoder changes, audio
  concealment), every rendered frame (`requestVideoFrameCallback`) with render
  stalls, `<video>` element events, and a QoE summary. All uploaded
  periodically to the server.
- **Shared simulcast source (optional)**: with `--simulcast`, viewers can
  instead receive pre-encoded renditions that arrive as MPEG-TS over UDP.
  Each viewer keeps its own PeerConnection and GCC, but nothing is encoded
  per viewer: GCC's target picks which rendition to forward, and the viewer
  pads so GCC can discover that a higher one fits. Same design as v1. See
  [Shared simulcast source](#shared-simulcast-source).

## Requirements

- Go 1.24+, a C toolchain, `libx264` with `pkg-config` (`brew install go x264 pkg-config`)
- `ffmpeg` (with `libopus`) on `PATH`; `ffprobe` is used if present

## Running

```sh
make run                 # builds bin/server, generates media/standin.mp4, serves :8080 + UDP 50000
./bin/server --video my.mp4 --http-addr :8080 --udp-port 50000
```

Open `http://<server>:8080`, press **Start**. Each session writes
`logs/<UTC time>_<session id>/`. Open TCP 8080 and UDP 50000 in a firewall.
On a cloud VM whose public IP is not on an interface, pass `--public-ip`.
Every flag can also be set as `PUFFER_<FLAG>` (e.g. `PUFFER_UDP_PORT=50010`).

| flag | default | |
|---|---|---|
| `--video` | `media/standin.mp4` | source file, looped forever |
| `--http-addr` | `:8080` | page, `/offer`, `/client-log` |
| `--udp-port` / `--udp-bind` | `50000` / all | single UDP port for all media |
| `--public-ip` | | IP(s) to advertise instead of interface IPs |
| `--stun` | | STUN URL(s) |
| `--start-bitrate` / `--min-bitrate` / `--max-bitrate` | 1M / 100k / 6M | GCC limits (bps) |
| `--audio-bitrate` | 64000 | Opus bitrate |
| `--headroom` | 0.9 | video target = `headroom × CC target − audio bitrate` |
| `--keyint` | 0 | seconds between periodic IDRs (0 = only on PLI/FIR) |
| `--x264-preset` / `--x264-threads` | veryfast / 0 | |
| `--snapshot-interval` | 250ms | server snapshot period |
| `--pc-stats-every` | 4 | include pion `GetStats()` every N snapshots |
| `--owd-interval` / `--owd-size` | 20ms / 13 | one-way-delay beacon period and size (bytes) |
| `--log-dir` | `logs` | |
| `--max-sessions` | 8 | each file session is one ffmpeg decode + one x264 encode (simulcast sessions encode nothing) |
| `--simulcast` | | renditions for the [shared simulcast source](#shared-simulcast-source): `name=host:port@bitrate,...` (MPEG-TS over UDP), e.g. `high=127.0.0.1:5001@2500k,mid=127.0.0.1:5002@800k,low=127.0.0.1:5003@250k` |
| `--simulcast-audio` | highest layer | rendition whose AAC audio is transcoded (once) to Opus; `none` for no audio |
| `--tls-cert` / `--tls-key` | | serve HTTPS |

Page settings: stats poll interval (default 250 ms), how often to keep the
*full* `getStats()` report (default every 4th poll), upload interval (2 s),
jitter buffer target, STUN, and whether to keep a copy of all records in the
page for **Download JSON**.

**Show stats** in the header switches between the full page (video, tiles,
charts, event list, raw tables) and a video-only view for participants who
should just watch. Measurement, recording and uploads are identical in both
modes; only the DOM work (chart drawing, tile and table rendering) is skipped
while hidden. The choice is remembered in the browser and can be forced with
`?view=video` or `?view=stats` in the URL. The mode is logged as
`settings.view` in `clientInfo` and as a `view` event on every change, so a
run can be filtered by what the viewer was looking at.

## Shared simulcast source

The default source encodes the video once per viewer. The simulcast source
encodes it once for everyone: an external encoder produces a few
constant-bitrate H.264 renditions with IDRs at the same timestamps, the server
ingests them over UDP, and every viewer forwards one of them. This is the v1
design (`internal/simulcast` and `internal/session/simulcast.go` are ports).

```
                    MPEG-TS/UDP                  ┌─ viewer A: PeerConnection + GCC ──► forwards high ─► browser
transcoder ─┬─ high 2500k :5001 ─┐               │
(ffmpeg or  ├─ mid   800k :5002 ─┼─► hub ────────┼─ viewer B: PeerConnection + GCC ──► forwards mid ──► browser
 sfu)       └─ low   250k :5003 ─┘ (demux once,  │
                                   AAC→Opus once)└─ viewer C: PeerConnection + GCC ──► forwards low ──► browser
```

- **Hub (`internal/simulcast`)** listens on each rendition's UDP port,
  demultiplexes the MPEG-TS, and reads each layer's resolution from its SPS.
  It shares every access unit with all viewers without copying. The AAC audio
  of one rendition goes through a single shared ffmpeg to become Opus, paced
  to its timestamps.
- **Per viewer (`internal/session/simulcast.go`)**: the viewer's own GCC gives
  a video budget (`headroom × target − audio`, as for the file source). The
  forwarded layer is the highest one whose nominal bitrate fits:
  - **Down:** switches as soon as the current layer no longer fits.
  - **Up:** switches once the budget has covered 1.1× the higher layer for 1 s.
  - **When:** at the target layer's next IDR. This relies on the renditions
    having IDRs at the same timestamps. All layers share one 90 kHz clock, so
    the RTP timestamps stay continuous across a switch.
  - **Packetisation:** each viewer packetises the frames itself on a
    `TrackLocalStaticRTP`, so the sequence numbers and timestamps are its own
    (`rtpTs` in `encoder.jsonl` is that timestamp, so the per-frame join still
    works).
- **Probing:** Pion's GCC never raises its estimate above ~1.5× the rate it
  sees acknowledged, so a viewer sending a fixed-rate low layer would never
  discover that a higher layer fits. As libwebrtc does, a viewer below the top
  layer pads its send rate up to its estimate (at most what the next layer
  needs). The padding is sent as RTP padding-only packets, which browsers
  discard. Pion's GCC only counts payload bytes, so probes go through GCC as
  zero-filled payloads and become padding just before SRTP
  (`internal/probe`). Padding pauses for 3 s after GCC lowers its target.
- **Fill padding (optional):** with `"padding": "fill"` the viewer instead
  pads all the way up to the GCC target, on every layer including the top
  one, so the send rate (video + audio + padding) matches the target the way a
  sender with a full encoder would. It is a token bucket: every 10 ms the
  viewer earns the target's bytes, pays for the video and audio it sent, and
  sends the rest as padding.
- **Keyframes:** the encoder is shared, so a viewer's PLI/FIR can't force an
  IDR. The viewer instead stops sending until the next periodic IDR of its
  layer, and the event is counted as `keyframeWaits`. The GOP length of the
  renditions therefore bounds startup time, how quickly a down-switch takes
  effect, and recovery after a PLI.

### Running it

```sh
# 1. a transcoder. scripts/simulcast-renditions.sh is a self-contained one:
#    ffmpeg decodes the file once (looped, real time) and sends three aligned
#    renditions (2 s GOP) to UDP 5001-5003. LAYERS/GOP/FPS/HOST are env knobs.
make renditions                      # or: ./scripts/simulcast-renditions.sh media/standin.mp4
#    (the sfu transcoder's UDP outputs work the same way)

# 2. this server, with the renditions as the simulcast source
./bin/server --simulcast high=127.0.0.1:5001@2500k,mid=127.0.0.1:5002@800k,low=127.0.0.1:5003@250k
```

On the page, **Settings → Video source** lists "Live simulcast (shared
encoder)" and selects it by default when the server has it. **Simulcast
padding** (shown for that source) switches between probing for the next layer
and filling up to the GCC target. The bitrate chart then also shows the
forwarded layer's nominal bitrate (as "encoder configured / simulcast
layer"), the padding, and the total send rate (video + audio + padding).
Layer switches appear in **Events**; the server snapshot has a `simulcast`
section and the `media` section shows the hub's per-layer status.

The API is `"source": "simulcast"` in the `/offer` body, plus
`"padding": "fill"` for fill padding. `GET /info` includes the hub status
under `simulcast`. The UDP ports are bound by this server.

In the logs (see [The logs](#the-logs)): `meta.json` has `source` and
`simulcast` (layers, padding, hysteresis); `events.jsonl` has `layer`
(switch happened) and `layerPending` (decided, waiting for an IDR); every
forwarded frame is an `encoder.jsonl` `frame` with `layer`, `pts`, `hubMs`
(hub arrival → forwarded) and `encMs = 0`; each switch is a `reconfig` with
`layer`; probes are `cc.jsonl` `sent` records with `probe: true` (GCC
counted `len` bytes; the wire carried them as padding); snapshots have a
`simulcast` section (layer, pending, budget, padding rate, total send rate,
switches, keyframe waits, frames per layer, startup) and the hub status as
`media`. The dashboard adds the budget, padding and total-send series, a
"layer" lane in the QoE plot, and a Simulcast panel.

## Log dashboard (separate service)

`bin/admin` is a second, independent binary: it only reads the session
directories, needs no cgo, x264, ffmpeg or UDP port, and can run on the
measurement host next to the streaming server or on any machine that holds a
copy of the log directory (e.g. `rsync -a vm:puffer/logs/ ./logs/`).

```sh
make run-admin                          # builds bin/admin, serves http://localhost:8081 reading ./logs
./bin/admin --log-dir /data/puffer/logs --http-addr :8081 --token "$(openssl rand -hex 16)"
```

| flag | default | |
|---|---|---|
| `--log-dir` | `logs` | root holding the `<time>_<id>/` session directories |
| `--http-addr` | `:8081` | page and API |
| `--token` | | if set, the page and API require it (`Authorization: Bearer` or `?token=`); the page asks once and remembers it for the tab |
| `--live-window` | 5s | a session without a close event whose files changed this recently is shown as live |
| `--tls-cert` / `--tls-key` | | serve HTTPS |

Every flag can also be set as `PUFFER_ADMIN_<FLAG>`. Set `--token` before
exposing the port: the logs contain client IPs and user agents.

The page lists every session (newest first, live ones marked) and shows the
selected one:

- **Overview**: stat tiles (bitrates, CC target range, freezes, stalls, loss,
  NACK/PLI, OWD, RTT, startup, encode time, ...), session/client/server facts,
  file sizes and record counts, and a timeline of every discrete event from all
  streams (server state, browser events, RTCP NACK/PLI, keyframes, GCC state
  changes, source loops). Clicking a timeline row zooms the charts around it.
- **QoE plot**: bitrate (CC target, server encoded rate, browser received)
  and one-way delay (both directions) on one time axis, with every
  QoE-relevant event drawn as a coloured line over both charts and in a lane
  strip: freezes/pauses, render stalls, packet loss seen by the browser,
  packet loss reported in TWCC feedback, NACKs and retransmissions,
  keyframes/PLI/FIR, audio concealment, and other changes (dropped frames,
  resolution, decoder, source loop, visibility, start/stop). Categories toggle
  on and off; the list below the charts zooms to an event on click.
- **Charts**: synced, zoomable time series, one unit per chart: bitrate (CC
  target, encoder configured/output, sent, received), network delay (OWD both
  ways, TWCC queueing delay, RTT), GCC delay detector, pacer queue, browser
  playout pipeline, frame rates, frame sizes, encode time, loss and repair,
  playback interruptions, jitter, QP, audio. Drag to zoom, double-click to
  reset, hover for values, `table` for the numbers.
- **Frames**: the per-frame latency join (same as `tools/frame_latency.py`):
  stage distributions with a stacked bar of the median split, and a table of
  every encoded frame in a time window (or the chart zoom) with encode, pacer
  wait/span, network, jitter buffer, decode, compositor and end-to-end time,
  packets lost, and why a frame was not rendered; sort by worst end-to-end.
  The Overview has the glass-to-glass tile and panel, the QoE plot a latency
  chart, and Charts has "Glass-to-glass latency", "Frame latency stages" and
  "Frame delivery". Needs `rtpTs` in `encoder.jsonl` (older sessions show
  everything else).
- **Records**: page through any `.jsonl` with a kind filter, substring search
  and a time range; click a row for the full JSON.
- **Meta**: `meta.json` and the SDP offer/answer.

The API is `GET /api/sessions`, `.../{dir}/summary`, `.../{dir}/series`,
`.../{dir}/frames?from=&to=&limit=&sort=t|e2e` (per-frame latency rows),
`.../{dir}/records?file=&kind=&q=&from=&to=&offset=&limit=` and
`.../{dir}/files/{name}` (`internal/admin`). Everything is computed on request
and cached until a file in the directory changes (a 35 MB session takes about
0.2 s), so sessions that are still being written can be watched live; the page
re-fetches live sessions every 5 s.

## The logs

`logs/<time>_<id>/` contains `meta.json` plus one JSON-lines file per concern.
Every line is `{"t": <server unix ms, µs resolution>, "kind": ..., "data": ...}`.
Server timestamps are the event time (e.g. when the packet was actually sent or
received), not when the line was written.

| file | kinds | what |
|---|---|---|
| `meta.json` | | session id, client info, media, config, host (versions), SSRCs, `source` (`file`/`simulcast`) and `simulcast` (layers, padding mode), offer/answer SDP, this table |
| `events.jsonl` | `session`, `pcState`, `iceState`, `iceGatheringState`, `signalingState`, `dataChannel`, `media`, `keyframeRequest`, `layer`, `layerPending`, `closed` | server-side state changes (`layer*`: simulcast sessions) |
| `rtcp.jsonl` | `rr`, `sr`, `nack`, `pli`, `fir`, `remb`, `twcc`, `xr`, `sdes`, `bye`, `other`, `retransmit` | every RTCP packet from the browser, decoded (`raw` = base64 of the packet), logged once even when pion delivers it to both senders; `via`/`viaKind` say which local SSRC's reader saw it. `nack` has `seqs` (expanded) and `pairs`. `rr.reports[]` has `fractionLost`, `totalLost`, `highestSeq`, `jitter`/`jitterMs`, `lsr`, `dlsr` and `rttMs` (from our SR). `retransmit` is a packet the NACK responder resent (`ssrc`, `seq`, `bytes`). |
| `cc.jsonl` | `config`, `stream`, `sent`, `feedback`, `target` | the congestion controller's inputs and outputs, see below |
| `encoder.jsonl` | `reconfig`, `frame`, `skipped` | `frame`: `n`, `bytes`, `key`, `forced` (IDR because of PLI/FIR), `encMs`, `writeMs` (WriteSample incl. packetisation), `cfgBps` (encoder target), `ccTarget` (GCC target at that moment), `durMs`, `rtpTs` (the RTP timestamp pion's packetizer gave the frame: joins it to `sent.ts` in `cc.jsonl` and to `frame.rtpTimestamp` in `client.jsonl`). `reconfig`: `ccTarget`, `videoTarget`, `applied`, `previous`. Simulcast sessions: `frame` adds `layer`, `pts`, `hubMs` (`encMs` is 0, `cfgBps` is the layer's nominal bitrate); `reconfig` has `layer` and is written at every switch. |
| `snapshots.jsonl` | `snapshot` | every `--snapshot-interval`: `cc` (target + GCC internals: loss/delay targets, delay measurement/estimate/threshold, usage, state), `encoder` (windowed output bitrate/fps, encode-time stats, keyframes, cumulative counts), `rtcp` counters per sender with the last RR, `rtp` (pion outbound and remote-inbound stats, RTT), `resends`, `owd` counters, `client` upload counters, `media` position, selected candidate pair, DTLS state; every `--pc-stats-every`th one also has `pcStats` = pion `GetStats()`. Simulcast sessions add `simulcast` (layer, pending, budget, padding, total send, switches, keyframe waits, frames per layer) and `media` is the hub's per-layer status |
| `owd.jsonl` | `start`, `ping` | server view of the OWD channel: each ping's client send time `t1` and server `t2`/`t3` |
| `client.jsonl` | `clientInfo`, `hello`, `stats`, `statsFull`, `event`, `frame`, `owdBeacon`, `owdPing`, `owdSync`, `qoe`, `bye` | everything the browser recorded. `t` = server receive time, `tClient` = browser `Date.now()` and `perf` = browser `performance.now()` when recorded. |

### `cc.jsonl`: replaying the congestion controller

Pion's send-side GCC has two inputs, and both are logged with the timestamps the
controller itself used:

- `sent` — one record per RTP packet as it left the pacer (this is when GCC
  stamps the send time): `ssrc`, `seq`, `twcc` (transport-wide seq), `pt`, `m`,
  `ts`, `hdr` (header bytes), `len` (payload bytes, which is what pion's GCC
  counts), `pad`, `rtx` (a NACK resend), `qMs` (time in the pacer queue),
  and in simulcast sessions `probe: true` for a padding probe (GCC counted
  `len` payload bytes; the packet left as RTP padding of the same size).
- `feedback` — one record per RTCP batch containing TWCC (or RFC 8888): the
  arrival time, each feedback decoded to `pkts: [{seq, status, recvMs}]`
  (`recvMs` on the browser's TWCC clock = reference time + cumulative deltas;
  `status` 0 = lost), the `raw` packet in base64 so pion can re-parse it
  bit-exactly, and `targetAfter`/`statsAfter`: what the estimator reported
  right after consuming it.
- `target` — every target-bitrate change with its time (GCC's delay path
  updates asynchronously, so this is the authoritative output timeline).
- `config` and `stream` describe the estimator limits, pion versions and the
  streams (SSRC, payload type, TWCC extension id).

```sh
tools/cc_replay.py logs/<session>/ --csv packets.csv
```

rebuilds one row per packet (`twccSeq, ssrc, rtpSeq, sizeBytes, sendMs, queueMs,
recvMs, fbArrivalMs, targetAfterFb, ...`) and prints a summary (send rate, pacer
queueing, one-way delay variation, loss). Feed the rows to your own controller
in the order of `fbArrivalMs` to replay the decisions.

`tools/session_summary.py logs/<session>/` prints the timeline: server events,
RTCP counts with every NACK, encoder totals, target range, client events
(freezes, loss, stalls, ...), the QoE summary and the OWD summary.

### Per-frame latency

```sh
tools/frame_latency.py logs/<session>/ --timeline 5 --worst 10 --csv frames.csv
```

follows every encoded video frame to the screen by joining the three logs on
the RTP timestamp (`encoder.frame.rtpTs` = `cc.sent.ts` =
`client.frame.rtpTimestamp`) and prints the stages on one timeline:

| stage | from | to |
|---|---|---|
| encode | `frame.t` (encode start) | + `encMs` |
| write/packetise | | + `writeMs` (packets are now in the pacer) |
| pacer wait | enqueued | first `sent` of the frame |
| pacer span | first `sent` | last `sent` |
| network | last `sent` | rVFC `receiveTime` (last packet arrived) |
| jitter buffer | `receiveTime` | `presentationTime` − decode |
| decode | | rVFC `processingDuration` |
| compositor | `presentationTime` | `expectedDisplayTime` |
| end-to-end | encode start | `expectedDisplayTime` |

Browser times are `performance.now()` values; they are moved onto the server
clock with `clientInfo.timeOrigin` plus the OWD clock offset in force at that
moment (`owdPing.offsetUsedMs`), so the network stage is a real one-way delay,
not a relative one. `network var (TWCC)` is an independent cross-check from
the TWCC receive times of the same packets (delay above the session minimum).
Frames that never rendered are classified as `packetsLost` (TWCC marked
packets lost and no resend covered them), `noPackets` (encoded after the
connection closed), `beforeFirstRender`/`afterLastRender` or `notPresented`
(delivered but skipped by the decoder/compositor). `--worst` lists the frames
with the highest end-to-end latency with their breakdown, `--timeline SEC`
prints per-bucket medians, `--csv` writes one row per encoded frame.

### Client records

- `stats` (every poll): the selected `getStats()` reports — inbound-rtp video
  and audio, remote-outbound-rtp, selected candidate pair, transport,
  media-playout, codecs — plus `derived`: receive bitrate, decoded/received
  fps, loss %, jitter, jitter-buffer delay/target/minimum per frame, decode /
  processing / assembly / inter-frame delay, QP, counter deltas (freezes,
  pauses, dropped, NACK/PLI/FIR sent, retransmits received), audio
  concealment, ICE RTT, available incoming bitrate, and the latest OWD values.
- `statsFull` (every Nth poll): the complete `getStats()` report.
- `event`: browser state changes and the derived events, with `data.kind` one
  of `freeze`, `pause`, `packetLoss` (`media` video/audio), `retransmitsReceived`,
  `nackSent`, `pliSent`, `firSent`, `framesDropped`, `keyframeDecoded`,
  `resolution`, `decoder`, `audioConcealment`, `candidatePairChange`,
  `renderStall`, `presentationSkipped`, `firstRenderedFrame`, `video.*`
  element events, `pcState`/`iceState`/..., `visibility`, `view` (stats /
  video-only toggle), `uploadError`.
- `frame`: one per rendered frame (`requestVideoFrameCallback` metadata:
  `presentedFrames`, `presentationTime`, `expectedDisplayTime`,
  `processingDuration`, `receiveTime`, `rtpTimestamp`, size).
- `owdBeacon`: `seq`, `tSend` (server clock), `tRecv` (browser clock),
  `owdMs` (server→client, using the current offset). `owdPing`: `t1..t4`,
  `rttMs`, `offsetMs` (this sample), `upMs` (client→server),
  `downMs` (server→client from the pong), `offsetUsedMs`. `owdSync`: a new
  best (lowest-RTT) ping took over as the clock-offset reference.
- `qoe` and `bye` at stop: startup/connect times, render stalls, cumulative
  counters, average bitrate, audio concealment, OWD loss/reordering.

### One-way delay

The `owd` DataChannel is opened by the browser as unordered with
`maxRetransmits: 0`, so SCTP neither retransmits nor reorders it: on the wire
it is a datagram on the same UDP 5-tuple as the RTP, through the same DTLS
session and the same bottleneck queue. Server beacons carry their send time;
browser pings are echoed with the server's receive and send times. The browser
takes the lowest-RTT ping of the last 10 s as its clock-offset reference
(error ≤ RTT/2), so a path asymmetry shifts both directions by a constant while
delay *changes* (queueing) are measured correctly. Beacons are stamped before
they are queued, and the server skips a tick rather than queue beacons behind
each other. All raw timestamps are logged, so the delays can be recomputed
offline with any offset model. The CC trace is a third delay source: `sent`
times joined with TWCC receive times give per-RTP-packet delay variation.

## Notes

- The client uploads over HTTP (not the DataChannel), so the SCTP association
  carries only low-rate control traffic. Uploads share the network path but
  not the RTP flow; GCC never sees them.
- Volume at the defaults: `cc.jsonl` is ~2 lines per RTP packet (about
  15 MB/min at 5 Mbps); `client.jsonl` about 2 MB/min. Raise
  `--snapshot-interval` or the page's "full report every N" to shrink them.
- Pion's GCC starts conservatively and ramps about 8 %/s; adjust
  `--start-bitrate`. On loopback it reaches `--max-bitrate` in ~30 s.
- Network emulation: Chrome DevTools throttling does not affect WebRTC. Use
  `tc netem` on Linux or Network Link Conditioner on macOS.
- Each session costs one ffmpeg decode and one x264 encode (about one core at
  720p30 with `veryfast`).
