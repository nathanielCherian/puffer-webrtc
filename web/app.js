'use strict';

// ---------------------------------------------------------------------------
// Session state. Every measurement becomes a record {t, perf, kind, data}; records
// are uploaded to the server in batches (POST /client-log) and, optionally,
// kept in memory for Download JSON.
// ---------------------------------------------------------------------------
const S = {
  pc: null, dc: null, owdDc: null, owd: null,
  sessionId: null, logDir: null, hello: null,
  t0Perf: 0, t0Wall: 0, timers: [], tick: 0,
  pending: [], all: [], keepLocal: true,
  prev: null, prevT: 0,
  qoe: null, lastServer: null, lastDerived: null,
  uploading: false, uploadErrors: 0, uploadedRecords: 0, uploadedBatches: 0,
  renderFps: null, renderFpsCount: 0, lastRenderFpsAt: 0,
  showStats: true,
};

const video = $('video');

const charts = {
  bitrate: new Chart($('c-bitrate'), [
    { key: 'ccTarget', name: 'server CC target' },
    { key: 'encCfg', name: 'encoder configured / simulcast layer' },
    { key: 'encOut', name: 'server encoder out' },
    { key: 'recv', name: 'client video recv' },
    { key: 'availIn', name: 'client avail. incoming' },
    { key: 'pad', name: 'server padding' },
    { key: 'sendTotal', name: 'server total send (video + audio + padding)' },
  ]),
  delay: new Chart($('c-delay'), [
    { key: 'owdDown', name: 'OWD server→client' },
    { key: 'owdUp', name: 'OWD client→server' },
    { key: 'rtt', name: 'ICE RTT' },
    { key: 'serverRtt', name: 'server RTCP RTT' },
    { key: 'jb', name: 'jitter buffer delay' },
  ]),
  frames: new Chart($('c-frames'), [
    { key: 'fpsDecoded', name: 'decoded fps' },
    { key: 'fpsRendered', name: 'rendered fps (rVFC)' },
    { key: 'fpsEncoded', name: 'server encoded fps' },
    { key: 'freezes', name: 'freezes (cumulative)' },
  ]),
};
function drawCharts() {
  if (!S.showStats) return;
  const now = Date.now();
  for (const c of Object.values(charts)) c.draw(now);
}

// ---------------------------------------------------------------------------
// View mode: "stats" (video + charts + tables) or "video" (just the player).
// Measurement, recording and uploads are identical in both; only the DOM work
// (chart drawing, tile/table rendering, event list) is skipped while hidden.
// Chosen with the header toggle, ?view=video|stats, or remembered from last time.
// ---------------------------------------------------------------------------
function initialShowStats() {
  const q = new URLSearchParams(location.search).get('view');
  if (q === 'video') return false;
  if (q === 'stats') return true;
  try { return localStorage.getItem('showStats') !== '0'; } catch (e) { return true; }
}
function setShowStats(on, why) {
  S.showStats = on;
  document.body.classList.toggle('video-only', !on);
  $('showStats').checked = on;
  try { localStorage.setItem('showStats', on ? '1' : '0'); } catch (e) { /* private mode etc. */ }
  if (on) { renderTiles(); drawCharts(); if (S.lastDerived) $('t-derived').textContent = JSON.stringify(S.lastDerived, null, 1); if (S.lastServer) $('t-server').textContent = JSON.stringify(S.lastServer, null, 1); }
  if (S.pc) logEvent('client', 'view', { mode: on ? 'stats' : 'video', why });
}

// ---------------------------------------------------------------------------
// Records and events
// ---------------------------------------------------------------------------
function record(kind, data) {
  const rec = { t: Date.now(), perf: performance.now(), kind, data };
  S.pending.push(rec);
  if (S.keepLocal) S.all.push(rec);
  return rec;
}

function logEvent(src, kind, value, extra) {
  if (src === 'client') record('event', value && typeof value === 'object' ? { kind, ...value, ...extra } : { kind, value, ...extra });
  if (!S.showStats) return;
  const li = document.createElement('li');
  const rel = S.t0Wall ? ((Date.now() - S.t0Wall) / 1000).toFixed(3) + 's' : '';
  li.textContent = `${rel} [${src}] ${kind}: ${typeof value === 'object' ? JSON.stringify(value) : value}`;
  if (src === 'server') li.className = 'server';
  if (/fail|stall|error|closed|freeze|waiting|loss|nack|pli|retrans|drop/i.test(`${kind} ${value}`)) li.classList.add('bad');
  const list = $('events');
  list.appendChild(li);
  while (list.children.length > 500) list.removeChild(list.firstChild);
  list.scrollTop = list.scrollHeight;
}

for (const name of ['loadedmetadata', 'loadeddata', 'canplay', 'playing', 'waiting', 'stalled',
  'pause', 'play', 'ended', 'error', 'resize', 'suspend', 'emptied']) {
  video.addEventListener(name, () => {
    const v = name === 'resize' ? `${video.videoWidth}x${video.videoHeight}` : video.currentTime.toFixed(3);
    logEvent('client', 'video.' + name, v);
    if (!S.qoe) return;
    if (name === 'waiting') S.qoe.waitingEvents++;
    if (name === 'loadeddata' && S.qoe.firstFrameMs === null) S.qoe.firstFrameMs = performance.now() - S.t0Perf;
  });
}
document.addEventListener('visibilitychange', () => {
  logEvent('client', 'visibility', document.visibilityState);
  if (S.qoe) { S.qoe.lastFrameAt = null; S.renderFpsCount = 0; S.lastRenderFpsAt = performance.now(); }
});
window.addEventListener('beforeunload', () => {
  // Best effort: the last few records go out with the page.
  if (S.sessionId && S.pending.length) {
    const body = JSON.stringify({ records: S.pending.slice(-500) });
    navigator.sendBeacon(`client-log?session=${S.sessionId}`, new Blob([body], { type: 'application/json' }));
  }
});

// ---------------------------------------------------------------------------
// Start / stop
// ---------------------------------------------------------------------------
$('start').onclick = start;
$('stop').onclick = () => stop('user stop');
$('download').onclick = download;
$('showStats').onchange = (e) => setShowStats(e.target.checked, 'toggle');
setShowStats(initialShowStats(), 'initial');
const sessionInputs = ['source', 'padMode', 'interval', 'fullEvery', 'uploadInterval', 'jbTarget', 'stun', 'keepLocal'];

function setStatus(s) { $('status').textContent = s; }

// ---------------------------------------------------------------------------
// Video source. "file" is always there; "simulcast" (the server forwards shared
// pre-encoded renditions and picks a layer per viewer) appears when the server
// was started with --simulcast, and is selected by default then, as in v1.
// The padding option only applies to simulcast viewers.
// ---------------------------------------------------------------------------
function simulcastSelected() { return $('source').value === 'simulcast'; }
function updateSourceUI() {
  for (const el of document.querySelectorAll('.pad-opt')) el.hidden = !simulcastSelected();
}
$('source').onchange = updateSourceUI;
async function loadSources() {
  let info;
  try { info = await (await fetch('info')).json(); } catch { return; }
  const sim = info.simulcast;
  if (!sim || !sim.layers) return;
  const o = document.createElement('option');
  o.value = 'simulcast';
  const live = sim.layers.some((l) => l.live);
  o.textContent = `Live simulcast (shared encoder) — ${sim.layers.map((l) => `${l.name} ${Math.round(l.nominalBps / 1000)}k`).join(' / ')}${live ? '' : ' — not receiving'}`;
  $('source').appendChild(o);
  $('source').value = 'simulcast';
  updateSourceUI();
}
loadSources();

function deviceInfo() {
  const c = navigator.connection || {};
  return {
    userAgent: navigator.userAgent,
    userAgentData: navigator.userAgentData ? { brands: navigator.userAgentData.brands, platform: navigator.userAgentData.platform, mobile: navigator.userAgentData.mobile } : null,
    hardwareConcurrency: navigator.hardwareConcurrency,
    deviceMemory: navigator.deviceMemory,
    screen: { w: screen.width, h: screen.height, dpr: window.devicePixelRatio },
    viewport: { w: innerWidth, h: innerHeight },
    connection: { effectiveType: c.effectiveType, downlink: c.downlink, rtt: c.rtt, type: c.type, saveData: c.saveData },
    timeOrigin: performance.timeOrigin,
  };
}

function settings() {
  return {
    statsIntervalMs: Math.max(50, Number($('interval').value) || 250),
    fullStatsEvery: Math.max(0, Number($('fullEvery').value) || 0),
    uploadIntervalMs: Math.max(250, Number($('uploadInterval').value) || 2000),
    jitterBufferTargetMs: $('jbTarget').value === '' ? null : Number($('jbTarget').value),
    stun: $('stun').value.trim() || null,
    keepLocal: $('keepLocal').checked,
    view: S.showStats ? 'stats' : 'video',
    source: $('source').value,
    padding: simulcastSelected() ? $('padMode').value : null,
  };
}

function newQoE() {
  return {
    startupMs: null, connectMs: null, firstFrameMs: null, firstPacketMs: null,
    renderedFrames: 0, renderStalls: 0, renderStallMs: 0, longestStallMs: 0,
    lastFrameAt: null, lastPresented: null, presentedGaps: 0, waitingEvents: 0,
    resolutionChanges: 0, lastRes: null,
  };
}

async function start() {
  $('start').disabled = true;
  $('download').disabled = false;
  for (const id of sessionInputs) $(id).disabled = true;
  const cfg = settings();
  Object.assign(S, {
    pending: [], all: [], keepLocal: cfg.keepLocal, prev: null, prevT: 0, tick: 0, qoe: newQoE(),
    sessionId: null, logDir: null, hello: null, lastServer: null, lastDerived: null,
    uploading: false, uploadErrors: 0, uploadedRecords: 0, uploadedBatches: 0,
    renderFps: null, renderFpsCount: 0,
  });
  $('events').innerHTML = '';
  for (const c of Object.values(charts)) c.reset();
  S.t0Perf = performance.now();
  S.t0Wall = Date.now();
  S.lastRenderFpsAt = S.t0Perf;
  logEvent('client', 'start', new Date(S.t0Wall).toISOString());
  record('clientInfo', { ...deviceInfo(), settings: cfg, startedAtWall: S.t0Wall });
  setStatus('connecting…');

  const pc = new RTCPeerConnection({ iceServers: cfg.stun ? [{ urls: cfg.stun }] : [] });
  S.pc = pc;
  pc.addTransceiver('video', { direction: 'recvonly' });
  pc.addTransceiver('audio', { direction: 'recvonly' });

  // Reliable channel: server hello, server snapshots/events for the page.
  const dc = pc.createDataChannel('telemetry', { ordered: true });
  S.dc = dc;
  dc.onopen = () => logEvent('client', 'dataChannel', 'telemetry open');
  dc.onclose = () => logEvent('client', 'dataChannel', 'telemetry closed');
  dc.onmessage = (e) => onServerMessage(e.data);
  // UDP-like channel on the same path: one-way delay beacons and pings.
  const owdDc = pc.createDataChannel('owd', { ordered: false, maxRetransmits: 0 });
  S.owdDc = owdDc;
  S.owd = new OwdProbe(owdDc, { record });
  owdDc.onopen = () => { logEvent('client', 'dataChannel', 'owd open'); S.owd.start(); };
  owdDc.onclose = () => { logEvent('client', 'dataChannel', 'owd closed'); S.owd.stop(); };

  pc.onconnectionstatechange = () => {
    logEvent('client', 'pcState', pc.connectionState);
    setStatus(pc.connectionState + (S.sessionId ? ` · session ${S.sessionId}` : ''));
    if (pc.connectionState === 'connected' && S.qoe.connectMs === null) S.qoe.connectMs = performance.now() - S.t0Perf;
    if (pc.connectionState === 'failed') stop('connection failed');
  };
  pc.oniceconnectionstatechange = () => logEvent('client', 'iceState', pc.iceConnectionState);
  pc.onicegatheringstatechange = () => logEvent('client', 'iceGatheringState', pc.iceGatheringState);
  pc.onsignalingstatechange = () => logEvent('client', 'signalingState', pc.signalingState);
  pc.onicecandidateerror = (e) => logEvent('client', 'iceCandidateError', `${e.errorCode} ${e.errorText} ${e.url || ''}`);
  pc.ontrack = (e) => {
    logEvent('client', 'track', e.track.kind);
    if (cfg.jitterBufferTargetMs !== null && 'jitterBufferTarget' in e.receiver) {
      e.receiver.jitterBufferTarget = cfg.jitterBufferTargetMs;
      logEvent('client', 'jitterBufferTarget', cfg.jitterBufferTargetMs);
    }
    if (video.srcObject !== e.streams[0]) {
      video.srcObject = e.streams[0];
      video.play().catch((err) => logEvent('client', 'playError', err.message));
    }
  };

  try {
    await pc.setLocalDescription(await pc.createOffer());
    await waitForGathering(pc, 3000);
    const res = await fetch('offer', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        sdp: pc.localDescription.sdp, type: pc.localDescription.type, client: { ...deviceInfo(), settings: cfg },
        source: cfg.source, padding: cfg.padding || undefined,
      }),
    });
    const body = await res.json();
    if (!res.ok) throw new Error(body.error || res.statusText);
    S.sessionId = body.sessionId;
    S.logDir = body.logDir;
    logEvent('client', 'answer', `session ${body.sessionId} (server log dir ${body.logDir})`);
    await pc.setRemoteDescription({ type: body.type, sdp: body.sdp });
  } catch (err) {
    logEvent('client', 'error', err.message);
    stop('signaling error: ' + err.message);
    return;
  }

  $('stop').disabled = false;
  if ('requestVideoFrameCallback' in HTMLVideoElement.prototype) video.requestVideoFrameCallback(onFrame);
  else logEvent('client', 'warning', 'requestVideoFrameCallback unsupported; render-stall metrics disabled');
  S.timers.push(setInterval(pollStats, cfg.statsIntervalMs));
  S.timers.push(setInterval(() => upload(false), cfg.uploadIntervalMs));
  S.timers.push(setInterval(drawCharts, 250));
}

function waitForGathering(pc, timeoutMs) {
  if (pc.iceGatheringState === 'complete') return Promise.resolve();
  return new Promise((resolve) => {
    const t = setTimeout(resolve, timeoutMs);
    pc.addEventListener('icegatheringstatechange', () => {
      if (pc.iceGatheringState === 'complete') { clearTimeout(t); resolve(); }
    });
  });
}

async function stop(reason) {
  for (const t of S.timers) clearInterval(t);
  S.timers = [];
  if (S.owd) S.owd.stop();
  logEvent('client', 'stop', reason);
  setStatus('stopping…');
  $('stop').disabled = true;
  if (S.pc) {
    const pc = S.pc;
    try { await pollStats(); } catch { /* ignore */ }
    record('qoe', qoeSummary());
    record('bye', { reason, owd: S.owd && S.owd.summary(), uploadedRecords: S.uploadedRecords, uploadErrors: S.uploadErrors });
    await upload(true);
    S.pc = null;
    pc.close();
  }
  setStatus('stopped: ' + reason + (S.logDir ? ` · server log ${S.logDir}` : ''));
  $('start').disabled = false;
  for (const id of sessionInputs) $(id).disabled = false;
}

// ---------------------------------------------------------------------------
// Upload: batches of records to the server, which appends them to client.jsonl.
// ---------------------------------------------------------------------------
async function upload(final) {
  if (!S.sessionId || !S.pending.length || (S.uploading && !final)) return;
  S.uploading = true;
  const recs = S.pending;
  S.pending = [];
  const chunks = [];
  for (let i = 0; i < recs.length; i += 1500) chunks.push(recs.slice(i, i + 1500));
  for (const chunk of chunks) {
    const body = JSON.stringify({ records: chunk });
    try {
      const res = await fetch(`client-log?session=${S.sessionId}`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body,
        keepalive: final && body.length < 60000,
      });
      if (!res.ok) throw new Error(res.statusText);
      S.uploadedRecords += chunk.length;
      S.uploadedBatches++;
    } catch (err) {
      S.uploadErrors++;
      S.pending.unshift(...chunk); // retry next time
      logEvent('client', 'uploadError', err.message);
      break;
    }
  }
  S.uploading = false;
}

// ---------------------------------------------------------------------------
// getStats polling: keep the interesting reports every tick, the full report
// every N ticks, and derive events (freezes, loss, NACKs, ...) from the deltas.
// ---------------------------------------------------------------------------
async function pollStats() {
  const pc = S.pc;
  if (!pc || pc.connectionState === 'closed') return;
  let report;
  try { report = await pc.getStats(); } catch { return; }
  const now = Date.now();
  const all = [];
  report.forEach((r) => all.push(r));
  const byId = Object.fromEntries(all.map((r) => [r.id, r]));
  const pick = {
    video: all.find((r) => r.type === 'inbound-rtp' && r.kind === 'video') || null,
    audio: all.find((r) => r.type === 'inbound-rtp' && r.kind === 'audio') || null,
    remoteOutboundVideo: all.find((r) => r.type === 'remote-outbound-rtp' && r.kind === 'video') || null,
    remoteOutboundAudio: all.find((r) => r.type === 'remote-outbound-rtp' && r.kind === 'audio') || null,
    transport: all.find((r) => r.type === 'transport') || null,
    playout: all.find((r) => r.type === 'media-playout') || null,
  };
  pick.pair = (pick.transport && byId[pick.transport.selectedCandidatePairId]) || all.find((r) => r.type === 'candidate-pair' && r.nominated) || null;
  pick.videoCodec = pick.video && pick.video.codecId ? byId[pick.video.codecId] : null;
  pick.audioCodec = pick.audio && pick.audio.codecId ? byId[pick.audio.codecId] : null;
  S.tick++;

  const dtSec = S.prevT ? (now - S.prevT) / 1000 : 0;
  const derived = derive(pick, S.prev, dtSec);
  const events = detectEvents(pick, S.prev);
  for (const ev of events) { const { kind, ...rest } = ev; logEvent('client', kind, rest); }
  record('stats', { tick: S.tick, ...pick, derived });
  const fullEvery = Math.max(0, Number($('fullEvery').value) || 0);
  if (fullEvery && S.tick % fullEvery === 0) record('statsFull', all);

  if (pick.video && S.qoe.firstPacketMs === null && pick.video.bytesReceived > 0) S.qoe.firstPacketMs = performance.now() - S.t0Perf;
  S.prev = pick;
  S.prevT = now;
  S.lastDerived = derived;
  charts.bitrate.push(now, { recv: derived.recvBitrateKbps, availIn: pick.pair && pick.pair.availableIncomingBitrate / 1000 });
  charts.delay.push(now, { rtt: derived.rttMs, jb: derived.jitterBufferDelayMs, owdDown: S.owd && S.owd.lastDown, owdUp: S.owd && S.owd.lastUp });
  charts.frames.push(now, { fpsDecoded: derived.fpsDecoded, fpsRendered: S.renderFps, freezes: pick.video && pick.video.freezeCount });
  renderTiles();
  if (S.showStats) $('t-derived').textContent = JSON.stringify(derived, null, 1);
}

const d = (cur, prev, key) => (cur && prev && cur[key] !== undefined && prev[key] !== undefined ? cur[key] - prev[key] : undefined);
const per = (num, den) => (num !== undefined && den ? num / den : undefined);

function derive(cur, prev, dt) {
  const v = cur.video, pv = prev && prev.video, a = cur.audio, pa = prev && prev.audio;
  const out = { dtSec: dt };
  if (v) {
    const frames = d(v, pv, 'framesDecoded');
    Object.assign(out, {
      recvBitrateKbps: per(d(v, pv, 'bytesReceived'), dt) * 8 / 1000,
      fpsDecoded: per(frames, dt), fpsReceived: per(d(v, pv, 'framesReceived'), dt), fpsReported: v.framesPerSecond,
      packetsPerSec: per(d(v, pv, 'packetsReceived'), dt),
      lossPct: (() => { const l = d(v, pv, 'packetsLost'), r = d(v, pv, 'packetsReceived'); return l !== undefined && r !== undefined && l + r > 0 ? 100 * l / (l + r) : 0; })(),
      jitterMs: v.jitter * 1000,
      jitterBufferDelayMs: per(d(v, pv, 'jitterBufferDelay'), d(v, pv, 'jitterBufferEmittedCount')) * 1000,
      jitterBufferTargetMs: per(d(v, pv, 'jitterBufferTargetDelay'), d(v, pv, 'jitterBufferEmittedCount')) * 1000,
      jitterBufferMinMs: per(d(v, pv, 'jitterBufferMinimumDelay'), d(v, pv, 'jitterBufferEmittedCount')) * 1000,
      decodeMs: per(d(v, pv, 'totalDecodeTime'), frames) * 1000,
      processingMs: per(d(v, pv, 'totalProcessingDelay'), frames) * 1000,
      assemblyMs: per(d(v, pv, 'totalAssemblyTime'), d(v, pv, 'framesAssembledFromMultiplePackets')) * 1000,
      interFrameDelayMs: per(d(v, pv, 'totalInterFrameDelay'), frames) * 1000,
      qp: per(d(v, pv, 'qpSum'), frames),
      freezes: d(v, pv, 'freezeCount'), freezeMs: (d(v, pv, 'totalFreezesDuration') || 0) * 1000,
      pauses: d(v, pv, 'pauseCount'), dropped: d(v, pv, 'framesDropped'),
      nacks: d(v, pv, 'nackCount'), plis: d(v, pv, 'pliCount'), firs: d(v, pv, 'firCount'),
      retransmittedPackets: d(v, pv, 'retransmittedPacketsReceived'), retransmittedBytes: d(v, pv, 'retransmittedBytesReceived'),
      keyFrames: d(v, pv, 'keyFramesDecoded'),
      width: v.frameWidth, height: v.frameHeight, decoder: v.decoderImplementation, powerEfficient: v.powerEfficientDecoder,
    });
  }
  if (a) {
    Object.assign(out, {
      audioBitrateKbps: per(d(a, pa, 'bytesReceived'), dt) * 8 / 1000,
      audioLossPct: (() => { const l = d(a, pa, 'packetsLost'), r = d(a, pa, 'packetsReceived'); return l !== undefined && r !== undefined && l + r > 0 ? 100 * l / (l + r) : 0; })(),
      audioJitterMs: a.jitter * 1000,
      audioJitterBufferMs: per(d(a, pa, 'jitterBufferDelay'), d(a, pa, 'jitterBufferEmittedCount')) * 1000,
      concealedPct: 100 * (per(d(a, pa, 'concealedSamples'), d(a, pa, 'totalSamplesReceived')) || 0),
      concealmentEvents: d(a, pa, 'concealmentEvents'),
      audioLevel: a.audioLevel,
    });
  }
  if (cur.pair) {
    out.rttMs = cur.pair.currentRoundTripTime !== undefined ? cur.pair.currentRoundTripTime * 1000 : undefined;
    out.availableIncomingKbps = cur.pair.availableIncomingBitrate !== undefined ? cur.pair.availableIncomingBitrate / 1000 : undefined;
    out.pairBitrateKbps = per(d(cur.pair, prev && prev.pair, 'bytesReceived'), dt) * 8 / 1000;
  }
  if (S.owd) {
    out.owdDownMs = S.owd.lastDown;
    out.owdUpMs = S.owd.lastUp;
    out.owdRttMs = S.owd.lastRtt;
  }
  out.renderFps = S.renderFps;
  for (const k of Object.keys(out)) if (Number.isNaN(out[k])) out[k] = undefined;
  return out;
}

// Counter increments between two polls become discrete events, so the log can
// be read as a timeline without diffing stats samples.
function detectEvents(cur, prev) {
  if (!prev) return [];
  const evs = [];
  const v = cur.video, pv = prev.video, a = cur.audio, pa = prev.audio;
  const inc = (c, p, key) => { const x = d(c, p, key); return x > 0 ? x : 0; };
  if (v && pv) {
    if (inc(v, pv, 'freezeCount')) evs.push({ kind: 'freeze', count: inc(v, pv, 'freezeCount'), durationMs: (d(v, pv, 'totalFreezesDuration') || 0) * 1000, totalFreezes: v.freezeCount });
    if (inc(v, pv, 'pauseCount')) evs.push({ kind: 'pause', count: inc(v, pv, 'pauseCount'), durationMs: (d(v, pv, 'totalPausesDuration') || 0) * 1000 });
    if (inc(v, pv, 'packetsLost')) evs.push({ kind: 'packetLoss', media: 'video', lost: inc(v, pv, 'packetsLost'), received: d(v, pv, 'packetsReceived'), totalLost: v.packetsLost });
    if (inc(v, pv, 'retransmittedPacketsReceived')) evs.push({ kind: 'retransmitsReceived', packets: inc(v, pv, 'retransmittedPacketsReceived'), bytes: d(v, pv, 'retransmittedBytesReceived') });
    if (inc(v, pv, 'nackCount')) evs.push({ kind: 'nackSent', count: inc(v, pv, 'nackCount'), total: v.nackCount });
    if (inc(v, pv, 'pliCount')) evs.push({ kind: 'pliSent', count: inc(v, pv, 'pliCount'), total: v.pliCount });
    if (inc(v, pv, 'firCount')) evs.push({ kind: 'firSent', count: inc(v, pv, 'firCount') });
    if (inc(v, pv, 'framesDropped')) evs.push({ kind: 'framesDropped', count: inc(v, pv, 'framesDropped'), total: v.framesDropped });
    if (inc(v, pv, 'keyFramesDecoded')) evs.push({ kind: 'keyframeDecoded', count: inc(v, pv, 'keyFramesDecoded'), total: v.keyFramesDecoded });
    if (inc(v, pv, 'fecPacketsReceived')) evs.push({ kind: 'fecReceived', count: inc(v, pv, 'fecPacketsReceived') });
    if (v.frameWidth && (v.frameWidth !== pv.frameWidth || v.frameHeight !== pv.frameHeight)) {
      evs.push({ kind: 'resolution', from: `${pv.frameWidth}x${pv.frameHeight}`, to: `${v.frameWidth}x${v.frameHeight}` });
      if (pv.frameWidth) S.qoe.resolutionChanges++;
    }
    if (v.decoderImplementation !== pv.decoderImplementation) evs.push({ kind: 'decoder', from: pv.decoderImplementation, to: v.decoderImplementation, powerEfficient: v.powerEfficientDecoder });
  }
  if (a && pa) {
    if (inc(a, pa, 'packetsLost')) evs.push({ kind: 'packetLoss', media: 'audio', lost: inc(a, pa, 'packetsLost'), received: d(a, pa, 'packetsReceived'), totalLost: a.packetsLost });
    if (inc(a, pa, 'concealmentEvents')) evs.push({ kind: 'audioConcealment', events: inc(a, pa, 'concealmentEvents'), concealedSamples: d(a, pa, 'concealedSamples'), silentConcealedSamples: d(a, pa, 'silentConcealedSamples') });
  }
  if (cur.pair && prev.pair && cur.pair.id !== prev.pair.id) evs.push({ kind: 'candidatePairChange', from: prev.pair.id, to: cur.pair.id });
  return evs;
}

// ---------------------------------------------------------------------------
// Per-frame metrics via requestVideoFrameCallback
// ---------------------------------------------------------------------------
function onFrame(now, meta) {
  const q = S.qoe;
  if (!q || !S.pc) return;
  record('frame', {
    now, mediaTime: meta.mediaTime, presentedFrames: meta.presentedFrames,
    expectedDisplayTime: meta.expectedDisplayTime, presentationTime: meta.presentationTime,
    width: meta.width, height: meta.height, processingDuration: meta.processingDuration,
    receiveTime: meta.receiveTime, captureTime: meta.captureTime, rtpTimestamp: meta.rtpTimestamp,
  });
  if (q.startupMs === null) {
    q.startupMs = now - S.t0Perf;
    logEvent('client', 'firstRenderedFrame', `${q.startupMs.toFixed(0)} ms after start`);
  }
  q.renderedFrames++;
  if (q.lastFrameAt !== null && document.visibilityState === 'visible') {
    const gap = now - q.lastFrameAt;
    const fpsHint = (S.lastDerived && S.lastDerived.fpsDecoded) || 30;
    const threshold = Math.max(150, 3 * 1000 / fpsHint);
    if (gap > threshold) {
      q.renderStalls++;
      q.renderStallMs += gap;
      q.longestStallMs = Math.max(q.longestStallMs, gap);
      logEvent('client', 'renderStall', { gapMs: Math.round(gap), thresholdMs: Math.round(threshold) });
    }
  }
  if (q.lastPresented !== null && meta.presentedFrames - q.lastPresented > 1) {
    const skipped = meta.presentedFrames - q.lastPresented - 1;
    q.presentedGaps += skipped;
    record('event', { kind: 'presentationSkipped', skipped });
  }
  q.lastFrameAt = now;
  q.lastPresented = meta.presentedFrames;
  S.renderFpsCount++;
  if (now - S.lastRenderFpsAt >= 1000) {
    S.renderFps = S.renderFpsCount * 1000 / (now - S.lastRenderFpsAt);
    S.renderFpsCount = 0;
    S.lastRenderFpsAt = now;
  }
  video.requestVideoFrameCallback(onFrame);
}

function qoeSummary() {
  const q = S.qoe || {};
  const v = S.prev && S.prev.video, a = S.prev && S.prev.audio;
  const durSec = (Date.now() - S.t0Wall) / 1000;
  return {
    durationSec: durSec, ...q,
    video: v && {
      bytesReceived: v.bytesReceived, avgBitrateKbps: v.bytesReceived * 8 / 1000 / durSec,
      framesReceived: v.framesReceived, framesDecoded: v.framesDecoded, framesDropped: v.framesDropped, keyFramesDecoded: v.keyFramesDecoded,
      freezeCount: v.freezeCount, totalFreezesDurationMs: (v.totalFreezesDuration || 0) * 1000,
      pauseCount: v.pauseCount, totalPausesDurationMs: (v.totalPausesDuration || 0) * 1000,
      packetsReceived: v.packetsReceived, packetsLost: v.packetsLost, nackCount: v.nackCount, pliCount: v.pliCount, firCount: v.firCount,
      retransmittedPacketsReceived: v.retransmittedPacketsReceived, retransmittedBytesReceived: v.retransmittedBytesReceived,
      jitterBufferDelayMeanMs: v.jitterBufferEmittedCount ? v.jitterBufferDelay / v.jitterBufferEmittedCount * 1000 : undefined,
      decoder: v.decoderImplementation, resolution: `${v.frameWidth}x${v.frameHeight}`,
    },
    audio: a && {
      packetsReceived: a.packetsReceived, packetsLost: a.packetsLost, concealedSamples: a.concealedSamples, totalSamplesReceived: a.totalSamplesReceived,
      concealedPct: a.totalSamplesReceived ? 100 * a.concealedSamples / a.totalSamplesReceived : undefined, concealmentEvents: a.concealmentEvents,
    },
    owd: S.owd && S.owd.summary(),
  };
}

// ---------------------------------------------------------------------------
// Server messages (telemetry DataChannel)
// ---------------------------------------------------------------------------
function onServerMessage(text) {
  let msg;
  try { msg = JSON.parse(text); } catch { return; }
  switch (msg.type) {
    case 'hello':
      S.hello = msg;
      record('hello', msg);
      logEvent('server', 'hello', `video ssrc ${msg.videoSsrc}, audio ssrc ${msg.audioSsrc}, ${msg.media.width}x${msg.media.height}@${fmtNum(msg.media.fps)}, source ${msg.source || 'file'}`);
      if (msg.simulcast) logEvent('server', 'simulcast', `layers ${msg.simulcast.layers.map((l) => `${l.name} ${Math.round(l.bitrateBps / 1000)}k`).join(' / ')}, padding ${msg.simulcast.padding}`);
      break;
    case 'server-stats': {
      S.lastServer = msg;
      if (S.keepLocal) S.all.push({ t: Date.now(), perf: performance.now(), kind: 'serverStats', data: msg });
      const now = Date.now();
      const sim = msg.simulcast;
      charts.bitrate.push(now, {
        ccTarget: msg.cc.targetBitrate / 1000, encOut: msg.encoder.outputBitrate / 1000, encCfg: msg.encoder.configuredBitrate / 1000,
        pad: sim ? sim.paddingBps / 1000 : undefined, sendTotal: sim ? sim.sentTotalBps / 1000 : undefined,
      });
      charts.frames.push(now, { fpsEncoded: msg.encoder.outputFps });
      charts.delay.push(now, { serverRtt: msg.rtcp.video && msg.rtcp.video.rttMs });
      if (S.showStats) $('t-server').textContent = JSON.stringify(msg, null, 1);
      renderTiles();
      break;
    }
    case 'server-event':
      logEvent('server', msg.event.kind, msg.event.value);
      break;
    default:
      break;
  }
}

function renderTiles() {
  if (!S.showStats) return;
  const dv = S.lastDerived || {}, sv = S.lastServer, v = S.prev && S.prev.video, q = S.qoe || {};
  const sim = sv && sv.simulcast;
  $('tiles').innerHTML = tilesHTML([
    ['CC target', sv ? sv.cc.targetBitrate / 1000 : null, 'kbps'],
    ...(sim ? [
      ['simulcast layer', `${sim.layer}${sim.pending ? ' → ' + sim.pending : ''}`, `${sim.switches} switches`],
      ['server padding', sim.paddingBps / 1000, `kbps (${sim.paddingMode})`],
      ['server total send', sim.sentTotalBps / 1000, 'kbps'],
    ] : []),
    ['encoder out', sv ? sv.encoder.outputBitrate / 1000 : null, 'kbps'],
    ['client recv', dv.recvBitrateKbps, 'kbps'],
    ['OWD ↓ server→client', dv.owdDownMs, 'ms'],
    ['OWD ↑ client→server', dv.owdUpMs, 'ms'],
    ['ICE RTT', dv.rttMs, 'ms'],
    ['jitter buffer', dv.jitterBufferDelayMs, 'ms'],
    ['decoded / rendered', `${fmtNum(dv.fpsDecoded)} / ${fmtNum(S.renderFps)}`, 'fps'],
    ['freezes', v ? `${v.freezeCount ?? '–'} (${fmtNum((v.totalFreezesDuration || 0) * 1000)} ms)` : '–', ''],
    ['render stalls', `${q.renderStalls || 0} (${fmtNum(q.renderStallMs || 0)} ms)`, ''],
    ['loss (interval)', dv.lossPct, '%'],
    ['packets lost', v ? v.packetsLost : null, 'total'],
    ['NACKs sent', v ? v.nackCount : null, 'total'],
    ['retransmits recv', v ? v.retransmittedPacketsReceived : null, 'total'],
    ['frames dropped', v ? v.framesDropped : null, 'total'],
    ['resolution', v ? `${v.frameWidth || '–'}x${v.frameHeight || '–'}` : '–', ''],
    ['server NACKs', sv && sv.rtcp.video ? `${sv.rtcp.video.nackPackets} (${sv.rtcp.video.nackedSeqs} seqs)` : '–', ''],
    ['uploaded', `${S.uploadedRecords} rec / ${S.uploadedBatches} batches`, S.uploadErrors ? `${S.uploadErrors} errors` : ''],
  ]);
}

function download() {
  const blob = new Blob([JSON.stringify({
    sessionId: S.sessionId, logDir: S.logDir, hello: S.hello, settings: settings(), device: deviceInfo(),
    qoe: S.qoe && qoeSummary(), records: S.all,
  })], { type: 'application/json' });
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = `client_${S.sessionId || 'session'}_${new Date(S.t0Wall || Date.now()).toISOString().replace(/[:.]/g, '-')}.json`;
  a.click();
  setTimeout(() => URL.revokeObjectURL(a.href), 1000);
}
