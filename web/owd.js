'use strict';

// ---------------------------------------------------------------------------
// One-way delay over the "owd" DataChannel (unordered, maxRetransmits 0, so it
// behaves like UDP on the same ICE/DTLS path as the media). The server sends a
// beacon every interval stamped with its send time; we ping it over the same
// channel to learn the clock offset NTP-style, keeping the lowest-RTT sample of
// the last 10 s (offset error at most rtt/2; path asymmetry shows up as a
// constant bias, not as changes in the delay).
//
//   server→client OWD = beacon receive time + offset − beacon send time
//   client→server OWD = ping t2 − (t1 + offset)
//
// Wire format, big-endian; server times are Unix microseconds:
//   beacon  [0]=1 [1:5] seq u32 [5:13] tSend i64 [13:] padding
//   ping    [0]=2 [1:5] id u32  [5:13] t1 f64 (our ms)
//   pong    [0]=3 [1:5] id u32  [5:13] t1 f64 [13:21] t2 i64 (received) [21:29] t3 i64 (sent)
// ---------------------------------------------------------------------------
class OwdProbe {
  // record(kind, data) is called for every beacon, ping and clock-sync change.
  constructor(dc, { record, pingIntervalMs = 250 } = {}) {
    this.dc = dc;
    this.record = record;
    this.pingIntervalMs = pingIntervalMs;
    this.recent = [];     // pings of the last 10 s
    this.best = null;     // lowest-RTT ping of the last 10 s
    this.firstSeq = null;
    this.maxSeq = -1;
    this.received = 0;
    this.reordered = 0;
    this.nextId = 1;
    this.timer = null;
    this.lastDown = null;
    this.lastUp = null;
    this.lastRtt = null;
    dc.binaryType = 'arraybuffer';
    dc.onmessage = (e) => this.onMessage(e.data);
  }

  // Wall-clock ms at sub-ms resolution; drifts from Date.now() over long runs,
  // but the offset is re-estimated from this same clock every few seconds.
  static now() { return performance.timeOrigin + performance.now(); }

  start() {
    this.ping();
    this.timer = setInterval(() => this.ping(), this.pingIntervalMs);
  }
  stop() { clearInterval(this.timer); this.timer = null; }

  ping() {
    if (this.dc.readyState !== 'open') return;
    const b = new DataView(new ArrayBuffer(13));
    b.setUint8(0, 2);
    b.setUint32(1, this.nextId++);
    b.setFloat64(5, OwdProbe.now());
    try { this.dc.send(b.buffer); } catch { /* closing */ }
  }

  onMessage(data) {
    const tRecv = OwdProbe.now();
    const d = new DataView(data);
    if (d.byteLength >= 13 && d.getUint8(0) === 1) this.onBeacon(d, tRecv);
    else if (d.byteLength >= 29 && d.getUint8(0) === 3) this.onPong(d, tRecv);
  }

  onBeacon(d, tRecv) {
    const seq = d.getUint32(1);
    const tSend = Number(d.getBigInt64(5)) / 1000;
    if (this.firstSeq === null) this.firstSeq = seq;
    if (seq < this.maxSeq) this.reordered++;
    this.maxSeq = Math.max(this.maxSeq, seq);
    this.received++;
    const owdMs = this.best ? tRecv + this.best.offsetMs - tSend : null;
    this.lastDown = owdMs;
    this.record('owdBeacon', { seq, tSend, tRecv, owdMs, size: d.byteLength });
  }

  onPong(d, t4) {
    const id = d.getUint32(1);
    const t1 = d.getFloat64(5);
    const t2 = Number(d.getBigInt64(13)) / 1000;
    const t3 = Number(d.getBigInt64(21)) / 1000;
    // offset = server clock − client clock; rtt excludes the server's turnaround.
    const s = { id, t1, t2, t3, t4, rttMs: (t4 - t1) - (t3 - t2), offsetMs: ((t2 - t1) + (t3 - t4)) / 2 };
    this.recent.push(s);
    while (t4 - this.recent[0].t4 > 10000) this.recent.shift();
    const best = this.recent.reduce((a, b) => (b.rttMs < a.rttMs ? b : a));
    if (best !== this.best) {
      this.best = best;
      this.record('owdSync', { offsetMs: best.offsetMs, rttMs: best.rttMs, pingId: best.id });
    }
    s.upMs = t2 - (t1 + this.best.offsetMs);
    s.downMs = t4 - (t3 + this.best.offsetMs);
    s.offsetUsedMs = this.best.offsetMs;
    this.lastUp = s.upMs;
    this.lastRtt = s.rttMs;
    this.record('owdPing', s);
  }

  summary() {
    const expected = this.received ? this.maxSeq - this.firstSeq + 1 : 0;
    return {
      beaconsReceived: this.received, beaconsLost: Math.max(0, expected - this.received), reordered: this.reordered,
      clockOffsetMs: this.best && this.best.offsetMs, clockRttMs: this.best && this.best.rttMs,
      lastDownMs: this.lastDown, lastUpMs: this.lastUp, lastRttMs: this.lastRtt,
    };
  }
}
