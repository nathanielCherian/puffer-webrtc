#!/usr/bin/env python3
"""Rebuild the congestion controller's inputs from a session's cc.jsonl.

Joins every "sent" record (server send time, size, transport-wide sequence
number) with the TWCC "feedback" records (receive time on the browser's clock)
into one row per packet: exactly what a send-side congestion controller sees.

    tools/cc_replay.py logs/<session>/            # summary
    tools/cc_replay.py logs/<session>/ --csv packets.csv

CSV columns: twccSeq, ssrc, rtpSeq, pt, rtx, sizeBytes (header+payload),
payloadBytes, sendMs (server clock), queueMs (pacer), recvMs (browser TWCC
clock, empty if lost), fbArrivalMs (server clock, when the feedback carrying
this packet arrived), fbIndex, targetAfterFb.

recvMs and sendMs are on different clocks; their difference minus its minimum is
the queueing-delay variation GCC's delay estimator works from.
"""
import argparse
import csv
import json
import os
import statistics
import sys


def load(path):
    with open(path) as f:
        for line in f:
            line = line.strip()
            if line:
                yield json.loads(line)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("session_dir")
    ap.add_argument("--csv", help="write one row per packet to this file")
    args = ap.parse_args()
    cc_path = os.path.join(args.session_dir, "cc.jsonl")
    if not os.path.exists(cc_path):
        sys.exit(f"no cc.jsonl in {args.session_dir}")

    config, streams, sent, feedback, targets = None, [], [], [], []
    for rec in load(cc_path):
        k = rec["kind"]
        if k == "config":
            config = rec["data"]
        elif k == "stream":
            streams.append(rec["data"])
        elif k == "sent":
            sent.append(rec)
        elif k == "feedback":
            feedback.append(rec)
        elif k == "target":
            targets.append(rec)

    # Unwrap 16-bit transport-wide sequence numbers in send order.
    by_seq = {}
    unwrapped = 0
    last = None
    for rec in sent:
        d = rec["data"]
        if d.get("twcc") is None:
            continue
        s = d["twcc"]
        if last is None:
            unwrapped = s
        else:
            delta = (s - (last & 0xFFFF) + 0x8000) % 0x10000 - 0x8000
            unwrapped += delta
        last = unwrapped
        by_seq[unwrapped] = rec

    rows = []
    acked = 0
    lost = 0
    dup = 0
    unknown = 0
    seen = set()
    fb_last = None
    for i, fb in enumerate(feedback):
        for pkt in fb["data"]["packets"]:
            if pkt.get("type") != "twcc":
                continue
            base = pkt["baseSeq"]
            # Unwrap the base relative to the previous feedback.
            if fb_last is None:
                base_u = base
            else:
                base_u = fb_last + (base - (fb_last & 0xFFFF) + 0x8000) % 0x10000 - 0x8000
            fb_last = base_u
            for j, p in enumerate(pkt["pkts"]):
                seq_u = base_u + j
                s = by_seq.get(seq_u)
                if s is None:
                    unknown += 1
                    continue
                if seq_u in seen:
                    dup += 1
                    continue
                seen.add(seq_u)
                d = s["data"]
                if p["recvMs"] is None and p["status"] == 0:
                    lost += 1
                else:
                    acked += 1
                rows.append({
                    "twccSeq": seq_u, "ssrc": d["ssrc"], "rtpSeq": d["seq"], "pt": d["pt"], "rtx": int(d["rtx"]),
                    "sizeBytes": d["hdr"] + d["len"], "payloadBytes": d["len"], "sendMs": s["t"], "queueMs": d["qMs"],
                    "recvMs": "" if p["recvMs"] is None else p["recvMs"],
                    "fbArrivalMs": fb["t"], "fbIndex": i, "targetAfterFb": fb["data"]["targetAfter"],
                })

    print(f"session: {args.session_dir}")
    if config:
        print(f"controller: {config.get('algorithm')} initial={config.get('initialBitrate')} min={config.get('minBitrate')} max={config.get('maxBitrate')}")
    for st in streams:
        print(f"stream: ssrc={st['ssrc']} {st['mimeType']} pt={st['payloadType']} twccExtId={st['twccExtId']}")
    print(f"sent packets: {len(sent)} ({len(by_seq)} with TWCC seq); feedback packets: {len(feedback)}; target changes: {len(targets)}")
    print(f"reported: {acked} received, {lost} lost, {dup} duplicate reports, {unknown} reported but never logged as sent")
    never = len(by_seq) - len(seen)
    print(f"sent but not (yet) reported: {never}")
    if sent:
        span = (sent[-1]["t"] - sent[0]["t"]) / 1000
        total = sum(r["data"]["hdr"] + r["data"]["len"] for r in sent)
        print(f"send rate over {span:.1f}s: {total * 8 / span / 1000:.0f} kbps; pacer queue p50/p95/max = "
              f"{statistics.median(r['data']['qMs'] for r in sent):.2f}/{pct([r['data']['qMs'] for r in sent], 0.95):.2f}/{max(r['data']['qMs'] for r in sent):.2f} ms")
    deltas = [r["recvMs"] - r["sendMs"] for r in rows if r["recvMs"] != ""]
    if deltas:
        base = min(deltas)
        q = [x - base for x in deltas]
        print(f"one-way delay variation (recv - send - min): p50={statistics.median(q):.2f} p95={pct(q, 0.95):.2f} max={max(q):.2f} ms "
              f"(clocks differ by a constant; min offset {base:.1f} ms)")
    if targets:
        vals = [t["data"]["bitrate"] for t in targets]
        print(f"target: first={vals[0]} min={min(vals)} max={max(vals)} last={vals[-1]} bps")
    if args.csv:
        with open(args.csv, "w", newline="") as f:
            w = csv.DictWriter(f, fieldnames=list(rows[0].keys()) if rows else ["twccSeq"])
            w.writeheader()
            w.writerows(rows)
        print(f"wrote {len(rows)} rows to {args.csv}")


def pct(v, p):
    v = sorted(v)
    return v[int(p * (len(v) - 1))] if v else float("nan")


if __name__ == "__main__":
    main()
