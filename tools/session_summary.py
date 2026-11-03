#!/usr/bin/env python3
"""Print an overview of one session's log directory.

    tools/session_summary.py logs/<session>/
"""
import collections
import json
import os
import sys


def load(path):
    if not os.path.exists(path):
        return
    with open(path) as f:
        for line in f:
            line = line.strip()
            if line:
                yield json.loads(line)


def main():
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    d = sys.argv[1]
    meta = json.load(open(os.path.join(d, "meta.json")))
    t0 = meta["createdMs"]
    rel = lambda t: f"{(t - t0) / 1000:8.3f}s"
    print(f"session {meta['sessionId']} from {meta['remoteAddr']} created {meta['createdAt']}")
    print(f"media {meta['media']['width']}x{meta['media']['height']}@{meta['media']['fps']} ssrc video={meta['videoSsrc']} audio={meta['audioSsrc']}")

    print("\n== server events ==")
    for r in load(os.path.join(d, "events.jsonl")):
        print(f"{rel(r['t'])} {r['kind']}: {r['data'].get('value')}")

    print("\n== rtcp received (by kind) ==")
    counts = collections.Counter()
    nacks = []
    retrans = 0
    for r in load(os.path.join(d, "rtcp.jsonl")):
        counts[r["kind"]] += 1
        if r["kind"] == "nack":
            nacks.append(r)
        elif r["kind"] == "retransmit":
            retrans += 1
    for k, v in sorted(counts.items()):
        print(f"  {k}: {v}")
    for r in nacks[:20]:
        print(f"{rel(r['t'])} NACK {r['data']['viaKind']} seqs={r['data']['seqs']}")
    if len(nacks) > 20:
        print(f"  ... {len(nacks) - 20} more NACKs")
    print(f"  retransmissions sent: {retrans}")

    print("\n== encoder ==")
    frames = 0
    nbytes = 0
    keys = 0
    reconf = 0
    first = last = None
    for r in load(os.path.join(d, "encoder.jsonl")):
        if r["kind"] == "frame":
            frames += 1
            nbytes += r["data"]["bytes"]
            keys += r["data"]["key"]
            first = first or r["t"]
            last = r["t"]
        elif r["kind"] == "reconfig":
            reconf += 1
    if frames and last > first:
        print(f"  {frames} frames, {keys} keyframes, {nbytes * 8 / (last - first):.0f} kbps average encoded, {reconf} reconfigurations")

    print("\n== congestion control ==")
    targets = [r["data"]["bitrate"] for r in load(os.path.join(d, "cc.jsonl")) if r["kind"] == "target"]
    if targets:
        print(f"  {len(targets)} target changes: first={targets[0]} min={min(targets)} max={max(targets)} last={targets[-1]} bps")

    print("\n== client ==")
    ckinds = collections.Counter()
    events = []
    qoe = None
    bye = None
    for r in load(os.path.join(d, "client.jsonl")):
        ckinds[r["kind"]] += 1
        if r["kind"] == "event":
            events.append(r)
        elif r["kind"] == "qoe":
            qoe = r["data"]
        elif r["kind"] == "bye":
            bye = r["data"]
    for k, v in sorted(ckinds.items()):
        print(f"  {k}: {v}")
    interesting = {"freeze", "pause", "packetLoss", "retransmitsReceived", "nackSent", "pliSent", "framesDropped",
                   "renderStall", "resolution", "decoder", "audioConcealment", "video.waiting", "video.stalled", "candidatePairChange"}
    print("  -- events --")
    for r in events:
        ev = r["data"]
        if ev.get("kind") in interesting or ev.get("kind", "").startswith(("pcState", "iceState", "start", "stop", "firstRendered")):
            print(f"{rel(r['tClient'])} {ev.get('kind')}: {json.dumps({k: v for k, v in ev.items() if k != 'kind'})}")
    if qoe:
        v = qoe.get("video") or {}
        print(f"  -- qoe -- startup {qoe.get('startupMs') and round(qoe['startupMs'])} ms, connect {qoe.get('connectMs') and round(qoe['connectMs'])} ms, "
              f"freezes {v.get('freezeCount')} ({v.get('totalFreezesDurationMs')} ms), render stalls {qoe.get('renderStalls')} ({round(qoe.get('renderStallMs', 0))} ms), "
              f"lost {v.get('packetsLost')} nack {v.get('nackCount')} pli {v.get('pliCount')} retrans {v.get('retransmittedPacketsReceived')}, "
              f"avg {v.get('avgBitrateKbps') and round(v['avgBitrateKbps'])} kbps")
        o = qoe.get("owd") or {}
        print(f"  -- owd -- beacons {o.get('beaconsReceived')} lost {o.get('beaconsLost')} reordered {o.get('reordered')}, "
              f"clock offset {o.get('clockOffsetMs')} ms (ping rtt {o.get('clockRttMs')} ms)")
    if bye:
        print(f"  -- bye -- {bye.get('reason')} ({bye.get('uploadedRecords')} records uploaded, {bye.get('uploadErrors')} upload errors)")


if __name__ == "__main__":
    main()
