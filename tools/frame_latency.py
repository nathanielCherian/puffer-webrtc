#!/usr/bin/env python3
"""Per-frame latency breakdown for one session.

Joins three logs on the RTP timestamp:

  encoder.jsonl  frame (rtpTs)        encode start, encode and write time
  cc.jsonl       sent (ts) + feedback  pacer entry/exit per packet, TWCC loss
  client.jsonl   frame (rtpTimestamp)  receive, decode, presentation, display

and converts the browser's performance.now() times to the server clock with
timeOrigin (clientInfo) and the OWD clock offset (owdPing.offsetUsedMs), so
every stage of one frame is on one timeline:

  encode -> write (packetise) -> pacer wait -> pacer span -> network ->
  jitter buffer -> decode -> compositor -> displayed

Usage:
  tools/frame_latency.py logs/<session>/ [--csv frames.csv] [--timeline 1]
                                          [--worst 10] [--no-summary]
"""
import argparse
import bisect
import csv
import json
import os
import statistics
import sys


def load(path):
    if not os.path.exists(path):
        return
    with open(path) as f:
        for line in f:
            line = line.strip()
            if line:
                try:
                    yield json.loads(line)
                except json.JSONDecodeError:
                    pass


def pct(xs, p):
    xs = sorted(x for x in xs if x is not None)
    if not xs:
        return None
    k = (len(xs) - 1) * p
    lo, hi = int(k), min(int(k) + 1, len(xs) - 1)
    return xs[lo] + (xs[hi] - xs[lo]) * (k - lo)


def fmt(x, w=8):
    return f"{x:{w}.2f}" if isinstance(x, (int, float)) else f"{'-':>{w}}"


class Unwrapper:
    """Turns 16-bit sequence numbers into a monotonic counter."""

    def __init__(self):
        self.last = None

    def __call__(self, v):
        if self.last is None:
            self.last = v
            return v
        d = (v - (self.last & 0xFFFF) + 0x8000) % 0x10000 - 0x8000
        self.last += d
        return self.last


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("session")
    ap.add_argument("--csv", help="write one row per encoded frame")
    ap.add_argument("--timeline", type=float, metavar="SEC", help="print per-bucket medians over the session")
    ap.add_argument("--worst", type=int, default=0, help="list the N frames with the highest end-to-end latency")
    ap.add_argument("--no-summary", action="store_true")
    a = ap.parse_args()
    d = a.session

    meta = json.load(open(os.path.join(d, "meta.json")))
    vssrc = meta.get("videoSsrc")

    # --- encoder frames -----------------------------------------------------
    frames = {}  # rtpTs -> frame dict
    order = []
    skipped = 0
    for r in load(os.path.join(d, "encoder.jsonl")):
        if r["kind"] == "skipped":
            skipped += 1
            continue
        if r["kind"] != "frame":
            continue
        x = r["data"]
        if "rtpTs" not in x:
            continue
        fr = {
            "n": x["n"], "rtpTs": x["rtpTs"], "key": x.get("key", False), "bytes": x["bytes"],
            "ccTarget": x.get("ccTarget"), "cfgBps": x.get("cfgBps"),
            "encodeStartMs": r["t"], "encMs": x["encMs"], "writeMs": x["writeMs"],
            "enqueuedMs": r["t"] + x["encMs"] + x["writeMs"],
            "pkts": 0, "rtxPkts": 0, "lostPkts": 0, "firstSendMs": None, "lastSendMs": None,
            "maxQueueMs": None, "twccFirstRecv": None, "twccLastRecv": None,
            "rendered": False, "offsetMs": None, "recvMs": None, "presentMs": None, "displayMs": None,
            "decodeMs": None, "jitterBufferMs": None, "compositorMs": None, "networkMs": None, "e2eMs": None,
            "pacerWaitMs": None, "pacerSpanMs": None, "twccDelayVarMs": None, "why": None,
        }
        # a repeated timestamp = never packetised (stale value after close): keep the first owner
        frames.setdefault(x["rtpTs"], fr)
        order.append(fr)
    if not frames:
        sys.exit("no encoder frame records with rtpTs (session recorded before per-frame RTP timestamps were added?)")

    # --- packets --------------------------------------------------------------
    unwrap_sent = Unwrapper()
    pkt_frame = {}  # unwrapped twcc seq -> (frame, sendMs, rtx)
    for r in load(os.path.join(d, "cc.jsonl")):
        if r["kind"] != "sent":
            continue
        x = r["data"]
        if vssrc is not None and x["ssrc"] != vssrc:
            continue
        fr = frames.get(x["ts"])
        if x.get("twcc") is not None:
            key = unwrap_sent(x["twcc"])
            if fr is not None:
                pkt_frame[key] = (fr, r["t"], x.get("rtx", False))
        if fr is None:
            continue
        if x.get("rtx"):
            fr["rtxPkts"] += 1
            continue
        fr["pkts"] += 1
        fr["firstSendMs"] = r["t"] if fr["firstSendMs"] is None else min(fr["firstSendMs"], r["t"])
        fr["lastSendMs"] = r["t"] if fr["lastSendMs"] is None else max(fr["lastSendMs"], r["t"])
        q = x.get("qMs")
        if q is not None:
            fr["maxQueueMs"] = q if fr["maxQueueMs"] is None else max(fr["maxQueueMs"], q)

    # --- TWCC feedback: loss and receive times per packet ---------------------
    unwrap_fb = Unwrapper()
    unwrap_fb.last = min(pkt_frame) if pkt_frame else None
    min_owd = None
    seen = set()
    for r in load(os.path.join(d, "cc.jsonl")):
        if r["kind"] != "feedback":
            continue
        for fb in r["data"].get("packets", []):
            for p in fb.get("pkts", []):
                key = unwrap_fb(p["seq"])
                if key in seen:
                    continue
                seen.add(key)
                ent = pkt_frame.get(key)
                if ent is None:
                    continue
                fr, send_ms, rtx = ent
                if p.get("recvMs") is None:
                    if not rtx:
                        fr["lostPkts"] += 1
                    continue
                if rtx:
                    continue
                rv = p["recvMs"]
                fr["twccFirstRecv"] = rv if fr["twccFirstRecv"] is None else min(fr["twccFirstRecv"], rv)
                fr["twccLastRecv"] = rv if fr["twccLastRecv"] is None else max(fr["twccLastRecv"], rv)
                owd = rv - send_ms
                min_owd = owd if min_owd is None else min(min_owd, owd)
    for fr in order:
        if fr["twccLastRecv"] is not None and fr["lastSendMs"] is not None and min_owd is not None:
            fr["twccDelayVarMs"] = (fr["twccLastRecv"] - fr["lastSendMs"]) - min_owd
        if fr["firstSendMs"] is not None:
            fr["pacerWaitMs"] = fr["firstSendMs"] - fr["enqueuedMs"]
            fr["pacerSpanMs"] = fr["lastSendMs"] - fr["firstSendMs"]

    # --- client: clock model and rendered frames ------------------------------
    time_origin = None
    offsets = []  # (tClient, offsetMs)
    client_frames = []
    for r in load(os.path.join(d, "client.jsonl")):
        k, x = r["kind"], r["data"]
        if k == "clientInfo" and x.get("timeOrigin"):
            time_origin = x["timeOrigin"]
        elif k == "owdPing" and x.get("offsetUsedMs") is not None:
            offsets.append((r.get("tClient", r["t"]), x["offsetUsedMs"]))
        elif k == "owdSync" and x.get("offsetMs") is not None:
            offsets.append((r.get("tClient", r["t"]), x["offsetMs"]))
        elif k == "frame":
            client_frames.append((r.get("tClient", r["t"]), x))
    offsets.sort()
    off_t = [t for t, _ in offsets]

    def offset_at(t):
        if not offsets:
            return 0.0
        i = bisect.bisect_right(off_t, t) - 1
        return offsets[max(i, 0)][1]

    rendered = 0
    unmatched_client = 0
    for t_client, x in client_frames:
        fr = frames.get(x.get("rtpTimestamp"))
        if fr is None:
            unmatched_client += 1
            continue
        if fr["rendered"]:
            continue  # rVFC fires once per presented frame; keep the first
        rendered += 1
        fr["rendered"] = True
        off = offset_at(t_client)
        fr["offsetMs"] = off
        # Chrome's rVFC times are performance.now(); timeOrigin + perf is the
        # browser wall clock the OWD offset was measured against.
        base = (time_origin if time_origin else (t_client - x.get("now", 0))) + off
        recv = x.get("receiveTime")
        pres = x.get("presentationTime")
        disp = x.get("expectedDisplayTime")
        dec = x.get("processingDuration")
        if dec is not None:
            fr["decodeMs"] = dec * 1000
        if recv is not None:
            fr["recvMs"] = base + recv
            if fr["lastSendMs"] is not None:
                fr["networkMs"] = fr["recvMs"] - fr["lastSendMs"]
        if pres is not None:
            fr["presentMs"] = base + pres
            if recv is not None:
                fr["jitterBufferMs"] = (pres - recv) - (fr["decodeMs"] or 0)
        if disp is not None:
            fr["displayMs"] = base + disp
            fr["e2eMs"] = fr["displayMs"] - fr["encodeStartMs"]
            if pres is not None:
                fr["compositorMs"] = disp - pres

    # why was a frame not rendered?
    first_r = next((f for f in order if f["rendered"]), None)
    last_r = next((f for f in reversed(order) if f["rendered"]), None)
    for fr in order:
        if fr["rendered"]:
            continue
        if fr["pkts"] == 0:
            fr["why"] = "noPackets"
        elif fr["lostPkts"] > fr["rtxPkts"]:
            fr["why"] = "packetsLost"
        elif first_r and fr["n"] < first_r["n"]:
            fr["why"] = "beforeFirstRender"
        elif last_r and fr["n"] > last_r["n"]:
            fr["why"] = "afterLastRender"
        else:
            fr["why"] = "notPresented"

    # --- output ----------------------------------------------------------------
    cols = ["n", "rtpTs", "key", "bytes", "ccTarget", "cfgBps", "encodeStartMs", "encMs", "writeMs",
            "pkts", "rtxPkts", "lostPkts", "firstSendMs", "lastSendMs", "maxQueueMs", "pacerWaitMs", "pacerSpanMs",
            "twccDelayVarMs", "rendered", "offsetMs", "recvMs", "networkMs", "jitterBufferMs", "decodeMs",
            "presentMs", "compositorMs", "displayMs", "e2eMs", "why"]
    if a.csv:
        with open(a.csv, "w", newline="") as f:
            w = csv.DictWriter(f, fieldnames=cols, extrasaction="ignore")
            w.writeheader()
            for fr in order:
                w.writerow({c: ("" if fr.get(c) is None else fr.get(c)) for c in cols})
        print(f"wrote {len(order)} rows to {a.csv}")

    if not a.no_summary:
        n = len(order)
        print(f"session {meta.get('sessionId')}  video ssrc {vssrc}")
        print(f"encoded frames {n}  skipped {skipped}  rendered {rendered} ({100*rendered/max(n,1):.1f}%)  "
              f"client frames without a server match {unmatched_client}")
        whys = {}
        for fr in order:
            if fr["why"]:
                whys[fr["why"]] = whys.get(fr["why"], 0) + 1
        if whys:
            print("not rendered: " + ", ".join(f"{k} {v}" for k, v in sorted(whys.items())))
        if offsets:
            offs = [o for _, o in offsets]
            print(f"clock offset (server - browser): {len(offs)} samples, min {min(offs):.2f} max {max(offs):.2f} ms"
                  f"{'' if time_origin else '  (no timeOrigin in clientInfo; using Date.now()-perf)'}")
        else:
            print("WARNING: no OWD offset samples; browser times are converted with offset 0")
        if min_owd is not None:
            print("network var (TWCC) = (last packet recv - send) minus the session minimum; the TWCC clock has no absolute origin")
        print()
        stages = [("encode", "encMs"), ("write/packetise", "writeMs"), ("pacer wait", "pacerWaitMs"),
                  ("pacer span", "pacerSpanMs"), ("network (rVFC)", "networkMs"), ("network var (TWCC)", "twccDelayVarMs"),
                  ("jitter buffer", "jitterBufferMs"), ("decode", "decodeMs"), ("compositor", "compositorMs"),
                  ("end-to-end", "e2eMs")]
        print(f"{'stage (ms)':<20}{'n':>7}{'p50':>9}{'p90':>9}{'p99':>9}{'max':>9}")
        for label, key in stages:
            xs = [fr[key] for fr in order if fr[key] is not None]
            if not xs:
                continue
            print(f"{label:<20}{len(xs):>7}{fmt(pct(xs,.5),9)}{fmt(pct(xs,.9),9)}{fmt(pct(xs,.99),9)}{fmt(max(xs),9)}")
        keys = [fr for fr in order if fr["key"] and fr["e2eMs"] is not None]
        if keys:
            print(f"\nkeyframes rendered {len(keys)}: e2e p50 {pct([f['e2eMs'] for f in keys],.5):.2f} ms, "
                  f"max {max(f['e2eMs'] for f in keys):.2f} ms, pacer span p50 {pct([f['pacerSpanMs'] for f in keys if f['pacerSpanMs'] is not None],.5):.2f} ms")

    if a.timeline:
        t0 = order[0]["encodeStartMs"]
        buckets = {}
        for fr in order:
            b = int((fr["encodeStartMs"] - t0) / (a.timeline * 1000))
            buckets.setdefault(b, []).append(fr)
        print(f"\n{'t(s)':>7}{'frames':>7}{'rend':>6}{'lost':>5}{'rtx':>5}{'kB/f':>7}{'target':>9}"
              f"{'pacer':>8}{'net':>8}{'jb':>8}{'dec':>8}{'e2e50':>8}{'e2emax':>8}")
        for b in sorted(buckets):
            fs = buckets[b]
            rend = [f for f in fs if f["rendered"]]
            med = lambda k: pct([f[k] for f in fs if f[k] is not None], .5)
            e2e = [f["e2eMs"] for f in rend if f["e2eMs"] is not None]
            tgt = pct([f["ccTarget"] for f in fs if f["ccTarget"]], .5)
            print(f"{b*a.timeline:>7.1f}{len(fs):>7}{len(rend):>6}{sum(f['lostPkts'] for f in fs):>5}"
                  f"{sum(f['rtxPkts'] for f in fs):>5}{statistics.mean(f['bytes'] for f in fs)/1000:>7.1f}"
                  f"{(tgt or 0)/1e6:>8.2f}M{fmt(med('pacerSpanMs'))}{fmt(med('networkMs'))}{fmt(med('jitterBufferMs'))}"
                  f"{fmt(med('decodeMs'))}{fmt(pct(e2e,.5))}{fmt(max(e2e) if e2e else None)}")

    if a.worst:
        worst = sorted((f for f in order if f["e2eMs"] is not None), key=lambda f: -f["e2eMs"])[:a.worst]
        print(f"\nworst {len(worst)} frames by end-to-end latency (ms):")
        print(f"{'n':>7}{'key':>4}{'kB':>7}{'enc':>7}{'pwait':>7}{'pspan':>7}{'net':>7}{'jb':>7}{'dec':>7}{'comp':>7}{'e2e':>8}{'lost':>5}")
        for f in worst:
            print(f"{f['n']:>7}{'K' if f['key'] else '':>4}{f['bytes']/1000:>7.1f}{fmt(f['encMs'],7)}{fmt(f['pacerWaitMs'],7)}"
                  f"{fmt(f['pacerSpanMs'],7)}{fmt(f['networkMs'],7)}{fmt(f['jitterBufferMs'],7)}{fmt(f['decodeMs'],7)}"
                  f"{fmt(f['compositorMs'],7)}{fmt(f['e2eMs'])}{f['lostPkts']:>5}")


if __name__ == "__main__":
    main()
