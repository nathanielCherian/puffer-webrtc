'use strict';

// Puffer log dashboard (served by cmd/admin): lists the session directories
// under --log-dir and shows one session as an overview, synced time-series
// charts, a paged record browser and meta.json. All heavy lifting (scanning
// cc.jsonl etc.) is done by the server in internal/admin; this page only renders.

const $ = (id) => document.getElementById(id);
const h = (tag, attrs = {}, ...kids) => {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') el.className = v;
    else if (k === 'html') el.innerHTML = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (v !== undefined && v !== null && v !== false) el.setAttribute(k, v === true ? '' : v);
  }
  for (const kid of kids.flat()) if (kid !== null && kid !== undefined && kid !== false) el.append(kid.nodeType ? kid : document.createTextNode(String(kid)));
  return el;
};

const state = {
  sessions: [], selected: null, tab: 'overview',
  summary: null, series: null, meta: null, loading: 0, error: null,
  zoom: null, hover: null, hidden: {}, markerSrc: { server: true, client: true, rtcp: true, encoder: true, cc: true }, markerLines: true,
  qoeCats: {}, qoeLines: true,
  rec: { file: 'events', kind: '', q: '', from: '', to: '', limit: 100, offset: 0, data: null, open: new Set() },
  fr: { from: '', to: '', sort: 't', limit: 500, data: null, error: null },
  token: sessionStorage.getItem('adminToken') || '',
  timers: {},
};

// ---------------------------------------------------------------------------
// API
// ---------------------------------------------------------------------------
async function api(path, params) {
  const url = new URL(path, location.origin);
  for (const [k, v] of Object.entries(params || {})) if (v !== '' && v !== null && v !== undefined) url.searchParams.set(k, v);
  for (;;) {
    const res = await fetch(url, { headers: state.token ? { Authorization: 'Bearer ' + state.token } : {} });
    if (res.status === 401) {
      const t = prompt('This dashboard requires a token (--token):', state.token);
      if (t === null) throw new Error('admin token required');
      state.token = t;
      sessionStorage.setItem('adminToken', t);
      continue;
    }
    if (!res.ok) {
      let msg = res.status + ' ' + res.statusText;
      try { msg = (await res.json()).error || msg; } catch (_) { /* not json */ }
      throw new Error(msg);
    }
    return res.json();
  }
}
const sessionURL = (dir, what) => `/api/sessions/${encodeURIComponent(dir)}/${what}`;
const fileURL = (dir, name) => sessionURL(dir, 'files/' + encodeURIComponent(name)) + (state.token ? '?token=' + encodeURIComponent(state.token) : '');

// ---------------------------------------------------------------------------
// Formatting
// ---------------------------------------------------------------------------
function fmtNum(v, digits) {
  if (v === null || v === undefined || Number.isNaN(v)) return '–';
  if (typeof v !== 'number') return String(v);
  if (digits !== undefined) return v.toFixed(digits);
  const a = Math.abs(v);
  if (a >= 1e6) return (v / 1e6).toFixed(2) + 'M';
  if (a >= 1e4) return (v / 1e3).toFixed(1) + 'k';
  if (a >= 100 || Number.isInteger(v)) return v.toFixed(0);
  if (a >= 10) return v.toFixed(1);
  if (a >= 1) return v.toFixed(2);
  return v.toFixed(3);
}
function fmtDur(sec) {
  if (sec === null || sec === undefined || Number.isNaN(sec)) return '–';
  if (sec < 60) return sec.toFixed(1) + ' s';
  const m = Math.floor(sec / 60), s = sec - m * 60;
  if (m < 60) return `${m}:${s.toFixed(0).padStart(2, '0')} min`;
  return `${Math.floor(m / 60)}h ${(m % 60).toString().padStart(2, '0')}m`;
}
function fmtT(sec) { // axis / timeline time
  if (sec < 0) return (sec).toFixed(2) + 's';
  if (sec < 120) return sec.toFixed(sec < 10 ? 2 : 1) + 's';
  const m = Math.floor(sec / 60), s = sec - m * 60;
  return `${m}:${s.toFixed(1).padStart(4, '0')}`;
}
function fmtBytes(b) {
  if (b === null || b === undefined) return '–';
  if (b < 1024) return b + ' B';
  if (b < 1048576) return (b / 1024).toFixed(1) + ' KB';
  if (b < 1073741824) return (b / 1048576).toFixed(1) + ' MB';
  return (b / 1073741824).toFixed(2) + ' GB';
}
function fmtWhen(iso) {
  if (!iso) return '–';
  const d = new Date(iso);
  return d.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' });
}
const pct = (num, den) => (den ? 100 * num / den : 0);
function flat(v, prefix = '') { // one-line "k=v k.k2=v" summary of a record's data
  const out = [];
  const walk = (x, p) => {
    if (x === null || typeof x !== 'object') { out.push(`${p}=${typeof x === 'number' ? fmtNum(x) : JSON.stringify(x)}`); return; }
    if (Array.isArray(x)) { out.push(`${p}=[${x.length}]`); return; }
    for (const [k, val] of Object.entries(x)) {
      if (k === 'raw') continue;
      if (val && typeof val === 'object' && !Array.isArray(val)) { if (out.length < 60) walk(val, p ? p + '.' + k : k); } else walk(val, p ? p + '.' + k : k);
    }
  };
  walk(v, prefix);
  return out.join('  ');
}

// ---------------------------------------------------------------------------
// Session list
// ---------------------------------------------------------------------------
async function loadSessions() {
  try {
    const d = await api('/api/sessions');
    state.sessions = d.sessions;
    $('root').textContent = `${d.sessions.length} session${d.sessions.length === 1 ? '' : 's'} in ${d.root}/` + (d.sessions.some((s) => s.live) ? ` · ${d.sessions.filter((s) => s.live).length} live` : '');
    renderList();
    if (state.selected && !state.sessions.some((s) => s.dir === state.selected)) { state.selected = null; renderDetail(); }
  } catch (e) {
    $('root').textContent = 'error: ' + e.message;
  }
}

function renderList() {
  const q = $('search').value.trim().toLowerCase();
  const sort = $('sort').value;
  let list = state.sessions.filter((s) => !q || [s.dir, s.id, s.remoteAddr, s.client.browser, s.client.platform, s.client.userAgent].join(' ').toLowerCase().includes(q));
  if (sort === 'dur') list = list.slice().sort((a, b) => b.durationSec - a.durationSec);
  else if (sort === 'size') list = list.slice().sort((a, b) => b.bytes - a.bytes);
  const ul = $('list');
  ul.replaceChildren();
  if (!list.length) { ul.append(h('li', { class: 'empty' }, state.sessions.length ? 'No sessions match.' : 'No sessions in the log directory yet.')); return; }
  for (const s of list) {
    ul.append(h('li', { class: s.dir === state.selected ? 'selected' : '', onclick: () => select(s.dir), title: s.dir },
      h('span', { class: 'when' }, fmtWhen(s.createdAt)),
      s.live ? h('span', { class: 'badge live' }, 'live') : h('span', { class: 'id mono' }, s.id.slice(0, 8)),
      h('span', { class: 'who' }, [s.client.browser, s.client.platform, s.remoteAddr && s.remoteAddr.replace(/^\[::1\]|^127\.0\.0\.1/, 'localhost')].filter(Boolean).join(' · ') || 'no client info'),
      h('span', { class: 'stats num' }, h('span', {}, fmtDur(s.durationSec)), h('span', {}, fmtBytes(s.bytes)), !s.ended && !s.live ? h('span', { class: 'badge' }, 'no close event') : null),
    ));
  }
}

// ---------------------------------------------------------------------------
// Selection & loading
// ---------------------------------------------------------------------------
async function select(dir, tab) {
  if (dir !== state.selected) {
    state.selected = dir;
    state.summary = state.series = state.meta = null;
    state.zoom = null; state.hover = null; state.error = null;
    state.rec = { ...state.rec, kind: '', q: '', from: '', to: '', offset: 0, data: null, open: new Set() };
    state.fr = { ...state.fr, from: '', to: '', data: null, error: null };
  }
  if (tab) state.tab = tab;
  location.hash = encodeURIComponent(dir) + '/' + state.tab;
  renderList();
  renderDetail();
  await refreshSelected();
}

async function refreshSelected() {
  const dir = state.selected;
  if (!dir) return;
  state.loading++;
  try {
    const [summary, series] = await Promise.all([api(sessionURL(dir, 'summary')), api(sessionURL(dir, 'series'))]);
    if (state.selected !== dir) return;
    state.summary = summary; state.series = series; state.error = null;
    if (state.tab === 'records' && !state.rec.data) loadRecords();
    if (state.tab === 'frames') loadFrames(true);
  } catch (e) {
    if (state.selected === dir) state.error = e.message;
  } finally {
    state.loading--;
  }
  renderDetail();
}

function scheduleAuto() {
  clearInterval(state.timers.auto);
  state.timers.auto = setInterval(() => {
    if (!$('auto').checked) return;
    loadSessions();
    const s = state.summary && state.summary.session;
    if (s && s.live && state.selected === s.dir) {
      refreshSelected();
      if (state.tab === 'records') loadRecords(true);
    }
  }, 5000);
}

// ---------------------------------------------------------------------------
// Detail rendering
// ---------------------------------------------------------------------------
function renderDetail() {
  const main = $('detail');
  const scrollY = main.scrollTop;
  main.replaceChildren();
  if (!state.selected) { main.append(h('div', { class: 'placeholder' }, 'Select a session on the left.')); return; }
  const info = state.sessions.find((s) => s.dir === state.selected) || (state.summary && state.summary.session);
  if (state.error) main.append(h('div', { class: 'error' }, 'Could not load this session: ' + state.error));
  if (!state.summary) { main.append(h('div', { class: 'placeholder' }, state.error ? '' : 'Loading…')); return; }
  const S = state.summary, s = S.session;
  main.append(h('div', { class: 'detail-head' },
    h('h2', {}, 'Session ', h('span', { class: 'mono' }, s.id)),
    s.live ? h('span', { class: 'badge live' }, 'live') : null,
    h('span', { class: 'sub' }, `${fmtWhen(s.createdAt)} · ${fmtDur(s.durationSec)} · ${[s.client.browser, s.client.platform].filter(Boolean).join(' on ') || 'unknown client'} · ${s.remoteAddr || ''}`),
    h('div', { class: 'actions' },
      h('a', { href: fileURL(s.dir, 'meta.json'), class: 'hint' }, 'meta.json'),
      ...S.files.map((f) => h('a', { href: fileURL(s.dir, f.name), class: 'hint', title: `${f.lines} lines, ${fmtBytes(f.bytes)}` }, f.name)),
    ),
  ));
  main.append(h('div', { class: 'tabs' }, ...[['overview', 'Overview'], ['qoe', 'QoE plot'], ['charts', 'Charts'], ['frames', 'Frames'], ['records', 'Records'], ['meta', 'Meta']].map(([id, label]) =>
    h('button', { class: id === state.tab ? 'active' : '', onclick: () => { state.tab = id; location.hash = encodeURIComponent(state.selected) + '/' + id; renderDetail(); if (id === 'records' && !state.rec.data) loadRecords(); if (id === 'frames' && !state.fr.data) loadFrames(); if (id === 'meta' && !state.meta) loadMeta(); } }, label))));
  const body = h('div');
  main.append(body);
  ({ overview: renderOverview, qoe: renderQoE, charts: renderCharts, frames: renderFrames, records: renderRecords, meta: renderMeta })[state.tab](body, S);
  main.scrollTop = scrollY;
  void info;
}

// -- Overview ---------------------------------------------------------------
function renderOverview(root, S) {
  const s = S.session, q = (S.client && S.client.qoe) || {}, qv = q.video || {}, qa = q.audio || {}, enc = S.encoder || {}, cc = S.cc || {}, rt = S.rtcp || {}, owd = S.owd || {};
  const d = (x) => x || {};
  const tile = (label, value, unit, note, level) => h('div', { class: 'tile' + (level ? ' ' + level : '') },
    h('div', { class: 'label' }, label), h('div', { class: 'value num' }, typeof value === 'string' ? value : fmtNum(value), unit ? h('small', {}, unit) : null), note ? h('div', { class: 'note' }, note) : null);
  const lossPct = pct(qv.packetsLost || 0, (qv.packetsLost || 0) + (qv.packetsReceived || 0));
  const tiles = [
    tile('Duration', fmtDur(s.durationSec), '', s.live ? 'still running' : S.endReason || (S.client.bye && S.client.bye.reason) || 'no close event'),
    tile('Received bitrate', qv.avgBitrateKbps, 'kbps', 'browser average'),
    tile('Encoder bitrate', enc.avgBitrateKbps, 'kbps', enc.frames ? `${fmtNum(enc.avgFps, 1)} fps · ${fmtNum(enc.frames)} frames` : 'no frames'),
    tile('CC target (last)', d(cc.target).lastBps / 1000, 'kbps', cc.target ? `${fmtNum(cc.target.minBps / 1000)}–${fmtNum(cc.target.maxBps / 1000)} kbps · ${cc.target.changes} changes` : ''),
    tile('Freezes', qv.freezeCount, '', `${fmtNum(qv.totalFreezesDurationMs)} ms total`, qv.freezeCount ? 'warn' : ''),
    tile('Render stalls', q.renderStalls, '', `${fmtNum(q.renderStallMs)} ms, longest ${fmtNum(q.longestStallMs)}`, q.renderStalls ? 'warn' : ''),
    tile('Packet loss (video)', lossPct, '%', `${fmtNum(qv.packetsLost)} of ${fmtNum((qv.packetsLost || 0) + (qv.packetsReceived || 0))}`, lossPct > 1 ? 'bad' : lossPct > 0 ? 'warn' : ''),
    tile('NACKs', d(rt.counts).nack || 0, '', `${rt.nackedSeqs || 0} seqs · ${rt.retransmits || 0} resent`, d(rt.counts).nack ? 'warn' : ''),
    tile('PLI / FIR', `${d(rt.counts).pli || 0} / ${d(rt.counts).fir || 0}`, '', `${enc.keyframes || 0} keyframes, ${enc.keyframesForced || 0} forced`, d(rt.counts).pli || d(rt.counts).fir ? 'warn' : ''),
    tile('OWD down', d(owd.beacons).median, 'ms', owd.beacons ? `p95 ${fmtNum(owd.beacons.p95)} · max ${fmtNum(owd.beacons.max)}` : 'no beacons'),
    tile('OWD up', d(owd.pingUp).median, 'ms', owd.pingUp ? `p95 ${fmtNum(owd.pingUp.p95)} · ${owd.pingUp.n} pings` : ''),
    tile('RTT (RR)', d(d(rt.rr).video).rttMs && rt.rr.video.rttMs.median, 'ms', d(d(rt.rr).video).rttMs ? `p95 ${fmtNum(rt.rr.video.rttMs.p95)}` : 'no receiver reports'),
    tile('Startup', q.startupMs, 'ms', q.firstFrameMs ? `first frame ${fmtNum(q.firstFrameMs)} ms, connect ${fmtNum(q.connectMs)} ms` : ''),
    tile('Jitter buffer', qv.jitterBufferDelayMeanMs, 'ms', 'mean per emitted frame'),
    tile('Glass-to-glass', d(d(d(S.frames).stages).e2e).median, 'ms', d(S.frames).available ? `encode start → display, per frame · p95 ${fmtNum(S.frames.stages.e2e.p95)} · max ${fmtNum(S.frames.stages.e2e.max)}` : 'needs rtpTs in encoder.jsonl'),
    tile('Frames rendered', d(S.frames).available ? S.frames.renderedPct : undefined, '%', d(S.frames).available ? `${fmtNum(S.frames.rendered)} of ${fmtNum(S.frames.encoded)} encoded` + (S.frames.notRendered.packetsLost ? ` · ${S.frames.notRendered.packetsLost} lost` : '') : '', d(S.frames).notRendered && S.frames.notRendered.packetsLost ? 'warn' : ''),
    tile('Audio concealed', qa.concealedPct, '%', `${qa.concealmentEvents || 0} events`, qa.concealedPct > 1 ? 'warn' : ''),
    tile('Pacer queue', d(cc.pacerQueueMs).p95, 'ms', cc.pacerQueueMs ? `p95 · max ${fmtNum(cc.pacerQueueMs.max)}` : ''),
    tile('TWCC loss', cc.feedbackLossPct, '%', `${fmtNum(cc.feedbackLost)} of ${fmtNum(cc.sentPackets)} packets`, cc.feedbackLossPct > 1 ? 'bad' : cc.feedbackLossPct > 0 ? 'warn' : ''),
    tile('Encode time', d(enc.encMs).p95, 'ms', enc.encMs ? `p95 · mean ${fmtNum(enc.encMs.mean)} · max ${fmtNum(enc.encMs.max)}` : ''),
  ];
  if (S.simulcast) {
    const sl = S.simulcast.last || {}, layers = (S.simulcast.config || {}).layers || [];
    tiles.unshift(
      tile('Simulcast layer', sl.layer || '–', '', `${layers.map((l) => `${l.name} ${fmtNum(l.bitrateBps / 1000)}k`).join(' / ')} · ${sl.switches || 0} switches · padding ${(S.simulcast.config || {}).padding}`),
      tile('Keyframe waits', sl.keyframeWaits || 0, '', `${fmtNum(sl.framesSkippedWaitingForKey)} frames skipped · startup ${fmtNum(sl.startupMs)} ms`, sl.keyframeWaits ? 'warn' : ''),
      tile('Padding', (cc.probeBytes || 0) * 8 / 1000 / Math.max(1, s.durationSec), 'kbps', `${fmtNum(cc.probePackets)} probe packets over the session`),
    );
  }
  root.append(h('div', { class: 'tiles' }, ...tiles));

  // Facts
  const m = S.meta || {}, cl = m.client || {}, host = m.host || {}, cfg = m.config || {}, media = m.media || s.media || {};
  const kv = (rows) => h('table', { class: 'kv' }, ...rows.filter((r) => r[1] !== undefined && r[1] !== null && r[1] !== '').map(([k, v]) => h('tr', {}, h('td', {}, k), h('td', { class: typeof v === 'number' ? 'num' : '' }, typeof v === 'number' ? fmtNum(v) : v))));
  root.append(h('div', { class: 'cols' },
    h('div', { class: 'panel' }, h('h3', {}, 'Session'), kv([
      ['directory', h('code', {}, s.dir)], ['created', `${fmtWhen(s.createdAt)} (${s.createdAt})`], ['remote', s.remoteAddr],
      ['ended', s.live ? 'running' : S.endReason || 'no close event'], ['client said', S.client.bye && S.client.bye.reason],
      ['video SSRC', m.videoSsrc && String(m.videoSsrc)], ['audio SSRC', m.audioSsrc && String(m.audioSsrc)],
      ['uploads', S.client.bye ? `${S.client.bye.uploadedRecords} records, ${S.client.bye.uploadErrors} errors` : undefined],
      ['log size', `${fmtBytes(s.bytes)} in ${S.files.length} streams`],
    ])),
    h('div', { class: 'panel' }, h('h3', {}, 'Client'), kv([
      ['browser', [s.client.browser, s.client.platform, s.client.mobile ? 'mobile' : ''].filter(Boolean).join(' · ')], ['user agent', h('span', { class: 'hint' }, cl.userAgent)],
      ['screen', cl.screen && `${cl.screen.w}×${cl.screen.h} @${cl.screen.dpr}x, viewport ${cl.viewport && cl.viewport.w}×${cl.viewport && cl.viewport.h}`],
      ['cpu / memory', cl.hardwareConcurrency && `${cl.hardwareConcurrency} threads, ${cl.deviceMemory || '?'} GB`],
      ['navigator.connection', cl.connection && `${cl.connection.effectiveType}, ${cl.connection.downlink} Mbps, rtt ${cl.connection.rtt} ms`],
      ['page settings', cl.settings && `poll ${cl.settings.statsIntervalMs} ms, full stats every ${cl.settings.fullStatsEvery}, upload ${cl.settings.uploadIntervalMs} ms, jitter target ${cl.settings.jitterBufferTargetMs ?? 'default'}`],
      ['decoded', S.client.resolution], ['rendered frames', S.client.renderedFrames],
    ])),
    h('div', { class: 'panel' }, h('h3', {}, 'Server'), kv([
      ['host', host.hostname && `${host.hostname} (${host.goos}/${host.goarch}, ${host.numCPU} cpu)`], ['go / pion', host.goVersion && `${host.goVersion}, webrtc ${(host.pion || {})['webrtc/v4']}, interceptor ${(host.pion || {}).interceptor}`],
      ['x264 / ffmpeg', host.x264Build && `build ${host.x264Build}; ${(host.ffmpeg || '').replace(' Copyright', ',').split(',')[0]}`],
      ['media', media.path && `${media.path}: ${media.width}×${media.height} @ ${media.fps} fps, ${media.durationSec} s, ${media.videoCodec}/${media.audioCodec || 'no audio'}`],
      ['GCC limits', cfg.StartBitrate && `start ${fmtNum(cfg.StartBitrate / 1000)} · min ${fmtNum(cfg.MinBitrate / 1000)} · max ${fmtNum(cfg.MaxBitrate / 1000)} kbps, headroom ${cfg.Headroom}`],
      ['encoder', cfg.X264Preset && `x264 ${cfg.X264Preset}, threads ${cfg.X264Threads || 'auto'}, keyint ${cfg.KeyIntSec || 'PLI only'}, audio ${fmtNum((cfg.AudioBitrate || 0) / 1000)} kbps`],
      ['intervals', cfg.SnapshotInterval && `snapshot ${cfg.SnapshotInterval / 1e6} ms, pc stats every ${cfg.PCStatsEvery}, OWD ${cfg.OWDInterval / 1e6} ms × ${cfg.OWDSize} B`],
      ['public IP / UDP', host.udpPort && `${host.publicIP || 'interface IPs'} : ${host.udpPort}`],
    ])),
    h('div', { class: 'panel' }, h('h3', {}, 'Files'), h('table', { class: 'grid' }, h('thead', {}, h('tr', {}, h('th', {}, 'file'), h('th', {}, 'lines'), h('th', {}, 'size'), h('th', {}, 'kinds'))),
      h('tbody', {}, ...S.files.map((f) => h('tr', {}, h('td', {}, h('a', { href: fileURL(s.dir, f.name) }, f.name)), h('td', { class: 'num' }, fmtNum(f.lines)), h('td', { class: 'num' }, fmtBytes(f.bytes)),
        h('td', { class: 'hint' }, Object.entries(f.kinds).sort((a, b) => b[1] - a[1]).map(([k, n]) => `${k} ${n}`).join(', '))))))),
  ));

  if (S.frames && S.frames.available) root.append(frameLatencyPanel(S.frames, true));

  // Timeline of markers
  const markers = (state.series && state.series.markers) || [];
  const srcs = ['server', 'client', 'rtcp', 'encoder', 'cc'];
  const counts = Object.fromEntries(srcs.map((x) => [x, markers.filter((mk) => mk.src === x).length]));
  const panel = h('div', { class: 'panel' }, h('h3', {}, `Timeline (${markers.length}${state.series && state.series.markersTotal > markers.length ? ' of ' + state.series.markersTotal : ''} events)`));
  panel.append(h('div', { class: 'chips' }, ...srcs.map((x) => h('span', { class: 'chip' + (state.markerSrc[x] ? ' on' : ''), onclick: () => { state.markerSrc[x] = !state.markerSrc[x]; renderDetail(); } }, x, h('span', { class: 'n' }, counts[x])))));
  const rows = markers.filter((mk) => state.markerSrc[mk.src]);
  panel.append(h('div', { class: 'scroll' }, h('table', { class: 'grid' }, h('thead', {}, h('tr', {}, h('th', {}, 't'), h('th', {}, 'source'), h('th', {}, 'kind'), h('th', {}, 'event'))),
    h('tbody', {}, ...rows.slice(0, 1500).map((mk) => h('tr', { onclick: () => { state.zoom = [Math.max(0, mk.t - 5), mk.t + 5]; state.hover = null; state.tab = 'charts'; renderDetail(); } , title: 'click to zoom the charts around this event' },
      h('td', { class: 't num' }, fmtT(mk.t)), h('td', {}, h('span', { class: 'badge src-' + mk.src }, mk.src)), h('td', {}, mk.kind), h('td', {}, mk.text)))))));
  if (rows.length > 1500) panel.append(h('div', { class: 'hint' }, `showing the first 1500 of ${rows.length}; use Records for the rest`));
  root.append(panel);

  const pre = (title, obj) => obj ? h('div', { class: 'panel' }, h('details', {}, h('summary', {}, title), h('pre', {}, JSON.stringify(obj, null, 1)))) : null;
  root.append(h('div', { class: 'cols' },
    pre('QoE summary (client)', S.client.qoe), pre('Client bye', S.client.bye), pre('Last client derived stats', S.client.lastStats && S.client.lastStats.derived),
    pre('Last server snapshot', S.snapshots && S.snapshots.last), pre('Simulcast (layers, forwarding, hub)', S.simulcast), pre('Encoder', S.encoder), pre('Congestion controller', S.cc), pre('RTCP', S.rtcp), pre('One-way delay', S.owd), pre('Frame latency', S.frames),
  ));
}

// -- Frame latency (per-frame join on the RTP timestamp) --------------------
// Stage order and fixed colours; `write` is folded into encode in the bar.
const FR_STAGES = [
  ['encode', 'encode', 4], ['pacerWait', 'pacer wait', 7], ['pacerSpan', 'pacer span', 5], ['network', 'network', 1],
  ['jitterBuffer', 'jitter buffer', 2], ['decode', 'decode', 3], ['compositor', 'compositor', 6],
];
const FR_EXTRA = [['write', 'write / packetise'], ['twccVar', 'network variation (TWCC cross-check)'], ['e2e', 'end-to-end (glass-to-glass)']];
const FR_WHY = { packetsLost: 'packets lost (no resend covered them)', noPackets: 'never sent (encoded after the connection closed)', beforeFirstRender: 'before the first rendered frame', afterLastRender: 'after the last rendered frame', notPresented: 'delivered but not presented (skipped by decoder/compositor)' };

function frameLatencyPanel(fr, brief) {
  const st = fr.stages || {}, e2e = st.e2e || {};
  const panel = h('div', { class: 'panel' }, h('h3', {}, 'Frame latency (glass-to-glass)'));
  const parts = FR_STAGES.map(([k, name, slot]) => ({ k, name, slot, v: st[k] ? st[k].median : 0 }));
  const total = parts.reduce((a, p) => a + Math.max(0, p.v), 0);
  panel.append(h('div', { class: 'hint' }, `${fmtNum(fr.rendered)} of ${fmtNum(fr.encoded)} encoded frames rendered (${fmtNum(fr.renderedPct, 1)} %) · end-to-end median ${fmtNum(e2e.median)} ms, p95 ${fmtNum(e2e.p95)}, max ${fmtNum(e2e.max)} · stage medians below sum to ${fmtNum(total)} ms`));
  panel.append(h('div', { class: 'stack' }, ...parts.filter((p) => p.v > 0).map((p) => h('i', { style: `--c: var(--s${p.slot}); flex: ${p.v}`, title: `${p.name}: ${fmtNum(p.v)} ms median` }))));
  panel.append(h('div', { class: 'chips' }, ...parts.map((p) => h('span', { class: 'chip cat', style: `--c: var(--s${p.slot})` }, `${p.name} `, h('b', { class: 'num' }, fmtNum(p.v)), h('span', { class: 'n' }, 'ms')))));
  const rows = [...FR_STAGES.map(([k, name]) => [k, name]), ...FR_EXTRA].filter(([k]) => st[k]);
  panel.append(h('table', { class: 'grid static' }, h('thead', {}, h('tr', {}, h('th', {}, 'stage (ms)'), h('th', {}, 'frames'), h('th', {}, 'median'), h('th', {}, 'mean'), h('th', {}, 'p95'), h('th', {}, 'p99'), h('th', {}, 'max'))),
    h('tbody', {}, ...rows.map(([k, name]) => h('tr', { class: k === 'e2e' ? 'total' : '' }, h('td', {}, name), ...['n', 'median', 'mean', 'p95', 'p99', 'max'].map((f) => h('td', { class: 'num' }, fmtNum(st[k][f], f === 'n' ? 0 : undefined))))))));
  const why = Object.entries(fr.notRendered || {});
  if (why.length) panel.append(h('div', { class: 'hint', style: 'margin-top:6px' }, 'not rendered: ' + why.map(([k, n]) => `${n} ${FR_WHY[k] || k}`).join('; ')));
  if (fr.keyframes && fr.keyframes.e2e) panel.append(h('div', { class: 'hint' }, `${fr.keyframes.rendered} keyframes rendered: end-to-end median ${fmtNum(fr.keyframes.e2e.median)} ms, max ${fmtNum(fr.keyframes.e2e.max)} ms`));
  if (!fr.clock.timeOrigin) panel.append(h('div', { class: 'hint' }, 'no timeOrigin in clientInfo: browser times converted with Date.now() − performance.now()'));
  if (brief) panel.append(h('div', { class: 'hint' }, 'Stages: encode → pacer wait (queued → first packet sent) → pacer span (first → last packet) → network (last packet sent → received) → jitter buffer → decode → compositor → displayed. Browser times are on the server clock via the OWD offset. See the Frames tab for every frame.'));
  return panel;
}

async function loadFrames(quiet) {
  const dir = state.selected, f = state.fr;
  if (!dir) return;
  try {
    const data = await api(sessionURL(dir, 'frames'), { from: f.from, to: f.to, limit: f.limit, sort: f.sort });
    if (state.selected !== dir) return;
    f.data = data; f.error = null;
  } catch (e) { f.error = e.message; }
  if (state.tab === 'frames') renderDetail();
  void quiet;
}

function renderFrames(root, S) {
  const f = state.fr, fr = S.frames;
  if (!fr || !fr.available) { root.append(h('div', { class: 'placeholder' }, 'No per-frame data: ' + ((fr && fr.reason) || 'encoder.jsonl has no rtpTs'))); return; }
  const reload = () => { f.data = null; renderDetail(); loadFrames(); };
  const tools = h('div', { class: 'rec-tools' },
    h('label', {}, 't from ', h('input', { type: 'number', step: 'any', value: f.from, placeholder: 's', onchange: (e) => { f.from = e.target.value; reload(); } })),
    h('label', {}, 'to ', h('input', { type: 'number', step: 'any', value: f.to, placeholder: 's', onchange: (e) => { f.to = e.target.value; reload(); } })),
    state.zoom ? h('button', { onclick: () => { f.from = state.zoom[0].toFixed(3); f.to = state.zoom[1].toFixed(3); reload(); } }, `use chart zoom (${fmtT(state.zoom[0])} – ${fmtT(state.zoom[1])})`) : null,
    h('label', {}, 'order ', h('select', { onchange: (e) => { f.sort = e.target.value; reload(); } }, h('option', { value: 't', selected: f.sort === 't' }, 'by time'), h('option', { value: 'e2e', selected: f.sort === 'e2e' }, 'worst end-to-end first'))),
    h('label', {}, 'show ', h('select', { onchange: (e) => { f.limit = +e.target.value; reload(); } }, ...[100, 250, 500, 1000, 2000, 5000].map((n) => h('option', { value: n, selected: n === f.limit }, n)))),
    h('span', { class: 'pager' }, f.data ? `${fmtNum(f.data.rows.length)} of ${fmtNum(f.data.total)} frames in range` : '…'),
  );
  root.append(frameLatencyPanel(fr, false));
  root.append(tools);
  if (f.error) root.append(h('div', { class: 'error' }, f.error));
  if (!f.data) { root.append(h('div', { class: 'placeholder' }, 'Loading frames…')); return; }
  const p95 = (fr.stages.e2e || {}).p95 || Infinity;
  const cols = [['n', 'frame'], ['t', 't'], ['key', 'key'], ['bytes', 'kB'], ['ccKbps', 'CC kbps'], ['pkts', 'pkts'], ['lost', 'lost'], ['rtx', 'rtx'],
    ['encode', 'encode'], ['pacerWait', 'pacer wait'], ['pacerSpan', 'pacer span'], ['pacerQueue', 'pacer q max'], ['network', 'network'], ['twccVar', 'twcc var'], ['jitterBuffer', 'jitter buf'], ['decode', 'decode'], ['compositor', 'compositor'], ['e2e', 'end-to-end'], ['why', 'not rendered']];
  const cell = (r, k) => {
    const v = r[k];
    if (k === 't') return fmtT(v);
    if (k === 'key') return v ? 'K' : '';
    if (k === 'bytes') return (v / 1000).toFixed(1);
    if (k === 'why') return v ? (FR_WHY[v] || v) : '';
    if (v === null || v === undefined) return '';
    return fmtNum(v, k === 'pkts' || k === 'lost' || k === 'rtx' || k === 'n' ? 0 : 1);
  };
  const body = h('tbody', {}, ...f.data.rows.map((r) => h('tr', { class: (!r.rendered ? 'bad ' : '') + (r.e2e !== null && r.e2e > p95 ? 'warn' : ''), title: 'click to zoom the charts ±2 s around this frame',
    onclick: () => { state.zoom = [Math.max(0, r.t - 2), r.t + 2]; state.hover = null; state.tab = 'charts'; renderDetail(); } },
    ...cols.map(([k]) => h('td', { class: k === 'why' ? 'hint' : 'num' }, cell(r, k))))));
  root.append(h('div', { class: 'scroll', style: 'max-height: calc(100vh - 420px)' }, h('table', { class: 'grid frames' }, h('thead', {}, h('tr', {}, ...cols.map(([, label]) => h('th', {}, label)))), body)));
  root.append(h('div', { class: 'hint' }, 'ms unless noted · rows in red were never rendered, orange exceed the p95 end-to-end latency · click a row to zoom the charts around it'));
}

// -- Charts -----------------------------------------------------------------
// One unit per chart (never two y-axes). Series keep their slot colour no
// matter which are hidden. `step` draws a staircase (the value holds until
// the next change). `off` starts hidden.
const CHARTS = [
  { title: 'Bitrate', unit: 'kbps', series: [
    ['ccTargetKbps', 'CC target', { step: true }], ['encCfgKbps', 'encoder configured / simulcast layer'], ['encOutKbps', 'encoder output'], ['sendKbps', 'sent (RTP, incl. headers)'],
    ['simBudgetKbps', 'video budget (simulcast)', { step: true }], ['padKbps', 'padding (simulcast)'], ['probeKbps', 'probe packets on the wire (simulcast)', { off: true }], ['sendTotalKbps', 'total send: video + audio + padding (simulcast)'],
    ['recvKbps', 'received by browser'], ['availInKbps', 'available incoming (browser)', { off: true }], ['lossTargetKbps', 'GCC loss target', { off: true }], ['delayTargetKbps', 'GCC delay target', { off: true }]] },
  { title: 'Network delay', unit: 'ms', series: [
    ['owdDownMs', 'OWD down (beacons, mean)'], ['owdUpMs', 'OWD up (pings)'], ['twccDelayMsMean', 'TWCC queueing delay (mean)'], ['rrRttVideoMs', 'RTT from RR (video)'],
    ['rttClientMs', 'RTT (browser candidate pair)', { off: true }], ['owdDownMaxMs', 'OWD down (max)', { off: true }], ['twccDelayMsMax', 'TWCC queueing delay (max)', { off: true }], ['owdRttMs', 'OWD ping RTT', { off: true }]] },
  { title: 'GCC delay detector', unit: 'ms', series: [['gccDelayEstimateMs', 'delay estimate'], ['gccDelayMeasurementMs', 'delay measurement', { off: true }], ['gccDelayThresholdMs', 'adaptive threshold']] },
  { title: 'Pacer queue', unit: 'ms', series: [['pacerQueueMsMean', 'mean'], ['pacerQueueMsMax', 'max']] },
  { title: 'Playout pipeline (browser)', unit: 'ms per frame', series: [
    ['jbDelayMs', 'jitter buffer delay'], ['jbTargetMs', 'jitter buffer target'], ['processingMs', 'processing (receive→decode done)'], ['decodeMs', 'decode'],
    ['assemblyMs', 'assembly', { off: true }], ['jbMinMs', 'jitter buffer minimum', { off: true }], ['interFrameMs', 'inter-frame delay', { off: true }], ['audioJbMs', 'audio jitter buffer', { off: true }]] },
  { title: 'Glass-to-glass latency (per frame)', unit: 'ms', series: [
    ['frE2eMs', 'end-to-end (bucket median)'], ['frE2eMaxMs', 'end-to-end (bucket max)'], ['frJitterBufferMs', 'jitter buffer (median)'], ['frNetworkMs', 'network (median)'],
    ['frPacerSpanMs', 'pacer span (median)'], ['frJitterBufferMaxMs', 'jitter buffer (max)', { off: true }], ['frNetworkMaxMs', 'network (max)', { off: true }], ['frPacerSpanMaxMs', 'pacer span (max)', { off: true }]] },
  { title: 'Frame latency stages', unit: 'ms, bucket median', series: [
    ['frEncodeMs', 'encode'], ['frPacerWaitMs', 'pacer wait'], ['frPacerSpanMs', 'pacer span'], ['frNetworkMs', 'network'], ['frJitterBufferMs', 'jitter buffer'], ['frDecodeMs', 'decode'], ['frCompositorMs', 'compositor'],
    ['frTwccVarMs', 'network variation (TWCC)', { off: true }], ['frWriteMs', 'write / packetise', { off: true }]] },
  { title: 'Frame delivery', unit: 'per bucket', series: [['frEncoded', 'frames encoded'], ['frRendered', 'frames rendered'], ['frNotRendered', 'frames not rendered'], ['frLostPackets', 'packets lost (TWCC, video)']] },
  { title: 'Frame rate', unit: 'fps', series: [['encFps', 'encoded'], ['fpsDecoded', 'decoded'], ['renderFps', 'rendered (rVFC)'], ['fpsReceived', 'received', { off: true }]] },
  { title: 'Encoded frame size', unit: 'bytes', series: [['encFrameBytesMean', 'mean per bucket'], ['encFrameBytesMax', 'max per bucket']] },
  { title: 'Encode time', unit: 'ms', series: [['encMsMean', 'mean'], ['encMsMax', 'max'], ['writeMsMax', 'WriteSample max', { off: true }]] },
  { title: 'Loss and repair', unit: 'count per bucket', series: [
    ['twccLostPackets', 'lost (TWCC feedback)'], ['nackedSeqs', 'NACKed seqs (RTCP)'], ['retransmits', 'retransmits sent'], ['clientNacks', 'NACKs sent (browser)'], ['clientPlis', 'PLIs sent (browser)'],
    ['clientDropped', 'frames dropped (browser)'], ['clientRtxReceived', 'retransmits received (browser)', { off: true }], ['rtxPackets', 'RTX packets (pacer)', { off: true }]] },
  { title: 'Loss rate', unit: '%', series: [['lossPct', 'video loss (browser)'], ['rrFractionLostVideoPct', 'fraction lost (RR video)'], ['concealedPct', 'audio concealed'], ['gccAverageLossPct', 'GCC average loss', { off: true }]] },
  { title: 'Playback interruptions', unit: 'ms per bucket', series: [['clientFreezeMs', 'freeze duration'], ['renderStallMs', 'render stall']] },
  { title: 'Playback events', unit: 'count per bucket', series: [['clientFreezes', 'freezes'], ['clientPauses', 'pauses'], ['encSkipped', 'encoder skipped frames']] },
  { title: 'Jitter', unit: 'ms', series: [['jitterMs', 'video (browser)'], ['rrJitterVideoMs', 'video (RR)'], ['rrJitterAudioMs', 'audio (RR)', { off: true }]] },
  { title: 'Quantizer', unit: 'QP', series: [['qp', 'average QP']] },
  { title: 'Audio bitrate', unit: 'kbps', series: [['audioKbps', 'received']] },
];
const MARKER_LINE_KINDS = new Set(['freeze', 'pause', 'nack', 'pli', 'fir', 'keyframe', 'loop', 'renderStall', 'packetLoss', 'resolution', 'closed', 'stop']);
const charts = []; // live TimelineChart instances for the current render

function renderCharts(root, S) {
  charts.length = 0;
  const ser = state.series;
  if (!ser) { root.append(h('div', { class: 'placeholder' }, 'Loading series…')); return; }
  const dur = Math.max(ser.durationSec || 0, 1);
  const tools = h('div', { class: 'chart-tools' },
    h('span', {}, `bucket ${ser.bucketSec} s · drag to zoom, double-click to reset`),
    h('button', { onclick: () => { state.zoom = null; state.hover = null; renderDetail(); } }, 'Reset zoom'),
    h('span', {}, state.zoom ? `${fmtT(state.zoom[0])} – ${fmtT(state.zoom[1])}` : `0 – ${fmtT(dur)}`),
    h('label', {}, h('input', { type: 'checkbox', checked: state.markerLines, onchange: (e) => { state.markerLines = e.target.checked; drawAll(); } }), 'event lines'),
    ...['server', 'client', 'rtcp', 'encoder', 'cc'].map((x) => h('label', {}, h('input', { type: 'checkbox', checked: state.markerSrc[x], onchange: (e) => { state.markerSrc[x] = e.target.checked; drawAll(); } }), h('span', { class: 'badge src-' + x }, x))),
  );
  root.append(tools);
  const grid = h('div', { class: 'charts' });
  root.append(grid);
  grid.append(new EventStrip(ser).el);
  for (const def of CHARTS) {
    const present = def.series.filter(([key]) => ser.series[key] && ser.series[key].length);
    if (!present.length) continue;
    grid.append(new TimelineChart(def, present, ser).el);
  }
  requestAnimationFrame(drawAll);
  void S;
}

function xDomain() {
  const ser = state.series;
  return state.zoom || [0, Math.max(ser.durationSec || 0, 1)];
}
function drawAll() { for (const c of charts) c.draw(); }
function css(name) { return getComputedStyle(document.documentElement).getPropertyValue(name).trim(); }
function niceStep(range, ticks) {
  const raw = range / ticks, p = Math.pow(10, Math.floor(Math.log10(raw)));
  for (const m of [1, 2, 2.5, 5, 10]) if (raw <= m * p) return m * p;
  return 10 * p;
}
function bisect(pts, t) { // index of first point with x >= t
  let lo = 0, hi = pts.length;
  while (lo < hi) { const mid = (lo + hi) >> 1; if (pts[mid][0] < t) lo = mid + 1; else hi = mid; }
  return lo;
}
function nearest(pts, t) {
  if (!pts.length) return null;
  const i = bisect(pts, t);
  const a = pts[Math.max(0, i - 1)], b = pts[Math.min(pts.length - 1, i)];
  return Math.abs(a[0] - t) <= Math.abs(b[0] - t) ? a : b;
}
const PAD = { l: 48, r: 10, t: 8, b: 20 };

// Shared pointer handling: hover crosshair on every chart, drag to zoom.
class ChartBase {
  constructor() { this.canvas = h('canvas'); this.tip = h('div', { class: 'tip' }); charts.push(this); this.bindPointer(); }
  bindPointer() {
    const c = this.canvas;
    let drag = null;
    const tAt = (ev) => { const r = c.getBoundingClientRect(); const [x0, x1] = xDomain(); return x0 + (ev.clientX - r.left - PAD.l) / (r.width - PAD.l - PAD.r) * (x1 - x0); };
    c.addEventListener('pointermove', (ev) => {
      state.hover = { t: tAt(ev), owner: this, x: ev.clientX, y: ev.clientY };
      if (drag) drag.to = state.hover.t;
      requestAnimationFrame(drawAll);
    });
    c.addEventListener('pointerleave', () => { state.hover = null; requestAnimationFrame(drawAll); });
    c.addEventListener('pointerdown', (ev) => { if (ev.button !== 0) return; c.setPointerCapture(ev.pointerId); drag = { from: tAt(ev), to: tAt(ev) }; this.drag = drag; });
    c.addEventListener('pointerup', () => {
      if (!drag) return;
      const [a, b] = [Math.min(drag.from, drag.to), Math.max(drag.from, drag.to)];
      drag = null; this.drag = null;
      if (b - a > 0.05) { const [x0, x1] = xDomain(); state.zoom = [Math.max(x0, a), Math.min(x1, b)]; state.hover = null; renderDetail(); } else requestAnimationFrame(drawAll);
    });
    c.addEventListener('dblclick', () => { state.zoom = null; state.hover = null; renderDetail(); });
  }
  setup() {
    const c = this.canvas, dpr = window.devicePixelRatio || 1;
    const w = c.clientWidth, hh = c.clientHeight;
    if (!w || !hh) return null;
    if (c.width !== Math.round(w * dpr) || c.height !== Math.round(hh * dpr)) { c.width = Math.round(w * dpr); c.height = Math.round(hh * dpr); }
    const ctx = c.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, hh);
    ctx.font = '11px system-ui, -apple-system, "Segoe UI", sans-serif';
    const [x0, x1] = xDomain();
    const x = (t) => PAD.l + (t - x0) / (x1 - x0) * (w - PAD.l - PAD.r);
    return { ctx, w, h: hh, x0, x1, x };
  }
  drawXAxis(g, yBase) {
    const { ctx, x0, x1, x } = g;
    const step = niceStep(x1 - x0, Math.max(3, Math.floor(g.w / 90)));
    ctx.fillStyle = css('--muted'); ctx.textAlign = 'center'; ctx.textBaseline = 'top';
    ctx.strokeStyle = css('--axis'); ctx.lineWidth = 1;
    ctx.beginPath(); ctx.moveTo(PAD.l, yBase + 0.5); ctx.lineTo(g.w - PAD.r, yBase + 0.5); ctx.stroke();
    for (let t = Math.ceil(x0 / step) * step; t <= x1 + 1e-9; t += step) {
      const label = fmtT(t), half = ctx.measureText(label).width / 2;
      ctx.fillText(label, Math.min(g.w - PAD.r - half, Math.max(PAD.l + half, x(t))), yBase + 4);
    }
  }
  drawMarkers(g, top, bottom) {
    const { ctx, x0, x1, x } = g;
    if (this.overlay) {
      ctx.lineWidth = 1;
      for (const m of this.overlay()) {
        if (m.t < x0 || m.t > x1) continue;
        ctx.strokeStyle = m.color; ctx.globalAlpha = 0.5;
        ctx.beginPath(); ctx.moveTo(Math.round(x(m.t)) + 0.5, top); ctx.lineTo(Math.round(x(m.t)) + 0.5, bottom); ctx.stroke();
      }
      ctx.globalAlpha = 1;
      return;
    }
    if (!state.markerLines || !state.series) return;
    ctx.lineWidth = 1;
    for (const m of state.series.markers) {
      if (m.t < x0 || m.t > x1 || !state.markerSrc[m.src] || !MARKER_LINE_KINDS.has(m.kind)) continue;
      ctx.strokeStyle = css('--s' + ({ server: 1, client: 2, rtcp: 3, encoder: 4, cc: 7 })[m.src]);
      ctx.globalAlpha = 0.45;
      ctx.beginPath(); ctx.moveTo(Math.round(x(m.t)) + 0.5, top); ctx.lineTo(Math.round(x(m.t)) + 0.5, bottom); ctx.stroke();
      ctx.globalAlpha = 1;
    }
  }
  drawDragAndHover(g, top, bottom) {
    const { ctx, x } = g;
    if (this.drag) {
      ctx.fillStyle = css('--sel');
      const a = x(Math.min(this.drag.from, this.drag.to)), b = x(Math.max(this.drag.from, this.drag.to));
      ctx.fillRect(a, top, b - a, bottom - top);
    }
    if (state.hover && state.hover.t >= g.x0 && state.hover.t <= g.x1) {
      ctx.strokeStyle = css('--fg2'); ctx.lineWidth = 1; ctx.setLineDash([]);
      ctx.globalAlpha = 0.5;
      ctx.beginPath(); ctx.moveTo(Math.round(x(state.hover.t)) + 0.5, top); ctx.lineTo(Math.round(x(state.hover.t)) + 0.5, bottom); ctx.stroke();
      ctx.globalAlpha = 1;
    }
  }
  placeTip(html) {
    const tip = this.tip;
    if (!html || !state.hover || state.hover.owner !== this) { tip.style.display = 'none'; return; }
    tip.innerHTML = html;
    tip.style.display = 'block';
    const r = this.el.getBoundingClientRect();
    let left = state.hover.x - r.left + 14, top = state.hover.y - r.top + 10;
    if (left + tip.offsetWidth > r.width - 4) left = Math.max(0, state.hover.x - r.left - tip.offsetWidth - 14);
    if (top + tip.offsetHeight > r.height - 2) top = Math.max(0, r.height - tip.offsetHeight - 2);
    tip.style.left = left + 'px'; tip.style.top = top + 'px';
  }
}

class TimelineChart extends ChartBase {
  constructor(def, present, ser, opts = {}) {
    super();
    this.def = def;
    this.overlay = opts.overlay; // () => [{t, color}] drawn instead of the generic event lines
    this.tall = !!opts.tall;
    this.series = present.map(([key, name, opts = {}], i) => ({ key, name, slot: def.series.findIndex((s) => s[0] === key) + 1, pts: ser.series[key], step: !!opts.step, off: !!opts.off }));
    const hid = state.hidden[def.title] || (state.hidden[def.title] = Object.fromEntries(this.series.filter((s) => s.off).map((s) => [s.key, true])));
    this.hidden = hid;
    this.legend = h('div', { class: 'legend' });
    this.tableBox = h('div', { class: 'table', style: 'display:none' });
    this.el = h('div', { class: 'chart' + (this.tall ? ' tall wide' : '') },
      h('div', { class: 'head' }, h('span', { class: 'title' }, def.title), h('span', { class: 'unit' }, def.unit),
        h('button', { class: 'tbl', onclick: () => this.toggleTable() }, 'table')),
      this.series.length > 1 ? this.legend : null, this.canvas, this.tip, this.tableBox);
    this.renderLegend();
  }
  renderLegend() {
    this.legend.replaceChildren(...this.series.map((s) => h('span', { class: this.hidden[s.key] ? 'off' : '', style: `--c: var(--s${s.slot})`, onclick: () => { this.hidden[s.key] = !this.hidden[s.key]; this.renderLegend(); this.draw(); } }, s.name)));
  }
  visible() { return this.series.filter((s) => !this.hidden[s.key]); }
  toggleTable() {
    const box = this.tableBox;
    if (box.style.display !== 'none') { box.style.display = 'none'; return; }
    const vis = this.visible(), [x0, x1] = xDomain();
    const ts = new Set();
    for (const s of vis) for (const [t] of s.pts) if (t >= x0 && t <= x1) ts.add(+t.toFixed(3));
    const rows = [...ts].sort((a, b) => a - b).slice(0, 3000);
    const lookup = vis.map((s) => new Map(s.pts.map(([t, v]) => [+t.toFixed(3), v])));
    box.replaceChildren(h('table', { class: 'grid' }, h('thead', {}, h('tr', {}, h('th', {}, 't (s)'), ...vis.map((s) => h('th', {}, s.name)))),
      h('tbody', {}, ...rows.map((t) => h('tr', {}, h('td', { class: 'num' }, t.toFixed(3)), ...lookup.map((m) => h('td', { class: 'num' }, m.has(t) ? fmtNum(m.get(t)) : '')))))));
    box.style.display = 'block';
  }
  draw() {
    const g = this.setup();
    if (!g) return;
    const { ctx, w, x0, x1, x } = g;
    const top = PAD.t, bottom = g.h - PAD.b;
    const vis = this.visible();
    let lo = 0, hi = 0;
    for (const s of vis) {
      const i0 = Math.max(0, bisect(s.pts, x0) - 1), i1 = Math.min(s.pts.length, bisect(s.pts, x1) + 1);
      for (let i = i0; i < i1; i++) { const v = s.pts[i][1]; if (v < lo) lo = v; if (v > hi) hi = v; }
    }
    if (hi === lo) hi = lo + 1;
    const step = niceStep(hi - lo, 4);
    const yMax = Math.ceil(hi / step) * step, yMin = Math.floor(lo / step) * step;
    const y = (v) => bottom - (v - yMin) / (yMax - yMin) * (bottom - top);
    ctx.strokeStyle = css('--grid'); ctx.lineWidth = 1; ctx.fillStyle = css('--muted'); ctx.textAlign = 'right'; ctx.textBaseline = 'middle';
    for (let v = yMin; v <= yMax + 1e-9; v += step) {
      const yy = Math.round(y(v)) + 0.5;
      ctx.beginPath(); ctx.moveTo(PAD.l, yy); ctx.lineTo(w - PAD.r, yy); ctx.stroke();
      ctx.fillText(fmtNum(v), PAD.l - 6, yy);
    }
    this.drawXAxis(g, bottom);
    this.drawMarkers(g, top, bottom);
    ctx.save();
    ctx.beginPath(); ctx.rect(PAD.l, top, w - PAD.l - PAD.r, bottom - top); ctx.clip();
    for (const s of vis) {
      ctx.strokeStyle = css('--s' + s.slot); ctx.lineWidth = 2; ctx.lineJoin = 'round';
      ctx.beginPath();
      const i0 = Math.max(0, bisect(s.pts, x0) - 1), i1 = Math.min(s.pts.length, bisect(s.pts, x1) + 1);
      let prev = null;
      for (let i = i0; i < i1; i++) {
        const [t, v] = s.pts[i];
        const px = x(t), py = y(v);
        if (prev === null) ctx.moveTo(px, py);
        else if (s.step) { ctx.lineTo(px, prev); ctx.lineTo(px, py); } else ctx.lineTo(px, py);
        prev = py;
      }
      if (s.step && prev !== null && i1 === s.pts.length) ctx.lineTo(x(x1), prev);
      ctx.stroke();
      if (i1 - i0 === 1) { ctx.fillStyle = css('--s' + s.slot); ctx.beginPath(); ctx.arc(x(s.pts[i0][0]), y(s.pts[i0][1]), 3, 0, Math.PI * 2); ctx.fill(); }
    }
    ctx.restore();
    this.drawDragAndHover(g, top, bottom);
    // Hover: highlight nearest point per series and show the tooltip.
    if (state.hover && state.hover.t >= x0 && state.hover.t <= x1) {
      const rows = [];
      for (const s of vis) {
        const p = s.step ? s.pts[Math.max(0, bisect(s.pts, state.hover.t + 1e-9) - 1)] : nearest(s.pts, state.hover.t);
        if (!p || Math.abs(p[0] - state.hover.t) > Math.max(2 * (state.series.bucketSec || 0.25), (x1 - x0) / 80) && !s.step) continue;
        ctx.fillStyle = css('--s' + s.slot); ctx.strokeStyle = css('--card'); ctx.lineWidth = 2;
        ctx.beginPath(); ctx.arc(x(s.step ? state.hover.t : p[0]), y(p[1]), 4, 0, Math.PI * 2); ctx.fill(); ctx.stroke();
        rows.push(`<div class="row"><span><i style="--c:var(--s${s.slot})"></i>${s.name}</span><b class="num">${fmtNum(p[1])}</b></div>`);
      }
      this.placeTip(rows.length ? `<div><b>${fmtT(state.hover.t)}</b> <span class="hint">${this.def.unit}</span></div>` + rows.join('') : '');
    } else this.placeTip('');
  }
}

// A strip of event ticks, one lane per source, with hover text.
class EventStrip extends ChartBase {
  constructor(ser) {
    super();
    this.lanes = ['server', 'client', 'rtcp', 'encoder', 'cc'];
    this.markers = ser.markers;
    this.el = h('div', { class: 'chart strip wide' }, h('div', { class: 'head' }, h('span', { class: 'title' }, 'Events'), h('span', { class: 'unit' }, `${ser.markersTotal} total; hover for details, click a row in Overview → Timeline to zoom`)), this.canvas, this.tip);
  }
  draw() {
    const g = this.setup();
    if (!g) return;
    const { ctx, w, x0, x1, x } = g;
    const top = PAD.t, bottom = g.h - PAD.b, laneH = (bottom - top) / this.lanes.length;
    ctx.textAlign = 'right'; ctx.textBaseline = 'middle'; ctx.fillStyle = css('--muted');
    this.lanes.forEach((l, i) => ctx.fillText(l, PAD.l - 6, top + (i + 0.5) * laneH));
    ctx.strokeStyle = css('--grid');
    for (let i = 1; i < this.lanes.length; i++) { const yy = Math.round(top + i * laneH) + 0.5; ctx.beginPath(); ctx.moveTo(PAD.l, yy); ctx.lineTo(w - PAD.r, yy); ctx.stroke(); }
    this.drawXAxis(g, bottom);
    let near = null, nearD = Infinity;
    for (const m of this.markers) {
      if (!state.markerSrc[m.src] || m.t < x0 || m.t > x1) continue;
      const i = this.lanes.indexOf(m.src);
      const px = Math.round(x(m.t)) + 0.5;
      ctx.strokeStyle = css('--s' + ({ server: 1, client: 2, rtcp: 3, encoder: 4, cc: 7 })[m.src]); ctx.lineWidth = MARKER_LINE_KINDS.has(m.kind) ? 2 : 1;
      ctx.beginPath(); ctx.moveTo(px, top + i * laneH + 3); ctx.lineTo(px, top + (i + 1) * laneH - 3); ctx.stroke();
      if (state.hover) { const d = Math.abs(m.t - state.hover.t); if (d < nearD) { nearD = d; near = m; } }
    }
    this.drawDragAndHover(g, top, bottom);
    if (near && state.hover && nearD <= (x1 - x0) / 60) {
      const same = this.markers.filter((m) => state.markerSrc[m.src] && Math.abs(m.t - near.t) <= (x1 - x0) / 200).slice(0, 8);
      this.placeTip(same.map((m) => `<div><b class="num">${fmtT(m.t)}</b> <span class="badge src-${m.src}">${m.src}</span> ${m.kind}: ${escapeHTML(m.text)}</div>`).join(''));
    } else this.placeTip('');
  }
}
function escapeHTML(s) { return String(s).replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c]); }

// -- QoE plot -----------------------------------------------------------------
// Bitrate, server encoded rate and OWD on one time axis, with every discrete
// QoE-relevant event drawn as a coloured line and in a lane strip. Events are
// grouped into fixed categories (fixed slot colours, never re-assigned).
const QOE_CATS = [
  { id: 'freeze', lane: 'freeze', name: 'freeze / pause', slot: 8, kinds: { client: ['freeze', 'pause'] } },
  { id: 'stall', lane: 'stall', name: 'render stall / waiting', slot: 2, kinds: { client: ['renderStall', 'presentationSkipped', 'video.waiting', 'video.stalled'] } },
  { id: 'lossClient', lane: 'loss rx', name: 'packet loss (browser)', slot: 4, kinds: { client: ['packetLoss'] } },
  { id: 'lossTwcc', lane: 'loss fb', name: 'packet loss (TWCC feedback)', slot: 5, bucket: 'twccLostPackets', text: (n) => `${fmtNum(n)} packet${n === 1 ? '' : 's'} reported lost in TWCC feedback` },
  { id: 'rtx', lane: 'rtx', name: 'NACK / retransmission', slot: 3, kinds: { rtcp: ['nack'], client: ['nackSent', 'retransmitsReceived'] }, bucket: 'retransmits', text: (n) => `${fmtNum(n)} retransmission${n === 1 ? '' : 's'} sent` },
  { id: 'key', lane: 'key', name: 'keyframe / PLI / FIR', slot: 7, kinds: { rtcp: ['pli', 'fir'], encoder: ['keyframe'], client: ['pliSent', 'firSent', 'keyframeDecoded'] } },
  { id: 'audio', lane: 'audio', name: 'audio concealment', slot: 6, kinds: { client: ['audioConcealment'] } },
  { id: 'other', lane: 'other', name: 'other (drops, resolution, decoder, loop, visibility, start/stop)', slot: 1,
    kinds: { client: ['framesDropped', 'resolution', 'decoder', 'candidatePairChange', 'visibility', 'firstRenderedFrame', 'start', 'stop', 'uploadError'], server: ['loop', 'media', 'closed'] } },
  { id: 'layer', lane: 'layer', name: 'simulcast layer switch / pending', slot: 9, kinds: { server: ['layer', 'layerPending'], encoder: ['layer'] } },
];
const QOE_BITRATE = { title: 'Bitrate', unit: 'kbps', series: [['ccTargetKbps', 'CC target (GCC)', { step: true }], ['encOutKbps', 'server encoded rate (encoder output)'], ['recvKbps', 'received by browser'], ['sendKbps', 'sent on the wire', { off: true }], ['encCfgKbps', 'encoder configured / simulcast layer', { off: true }], ['padKbps', 'padding (simulcast)', { off: true }], ['sendTotalKbps', 'total send incl. padding (simulcast)', { off: true }]] };
const QOE_OWD = { title: 'One-way delay', unit: 'ms', series: [['owdDownMs', 'server → browser (beacons, bucket mean)'], ['owdUpMs', 'browser → server (pings)'], ['owdDownMaxMs', 'server → browser (bucket max)', { off: true }], ['twccDelayMsMean', 'TWCC queueing delay (relative)', { off: true }]] };
const QOE_LATENCY = { title: 'Glass-to-glass latency', unit: 'ms per frame', optional: true, series: [['frE2eMs', 'encode start → display (bucket median)'], ['frE2eMaxMs', 'encode start → display (bucket max)', { off: true }], ['frJitterBufferMs', 'of which jitter buffer'], ['frNetworkMs', 'of which network'], ['frPacerSpanMs', 'of which pacer span', { off: true }]] };

// Every event of the session mapped to a category: explicit markers by
// (source, kind), plus one marker per non-empty bucket for counted series.
function qoeEvents(ser) {
  const byKind = new Map();
  for (const c of QOE_CATS) for (const [src, kinds] of Object.entries(c.kinds || {})) for (const k of kinds) byKind.set(src + '/' + k, c);
  const out = [];
  for (const m of ser.markers) {
    const c = byKind.get(m.src + '/' + m.kind);
    if (c) out.push({ t: m.t, cat: c, text: m.text, src: m.src, kind: m.kind });
  }
  for (const c of QOE_CATS) {
    if (!c.bucket || !ser.series[c.bucket]) continue;
    for (const [t, n] of ser.series[c.bucket]) if (n > 0) out.push({ t, cat: c, text: c.text(n), src: 'bucket', kind: c.bucket, bucket: true });
  }
  out.sort((a, b) => a.t - b.t);
  return out;
}

function renderQoE(root) {
  charts.length = 0;
  const ser = state.series;
  if (!ser) { root.append(h('div', { class: 'placeholder' }, 'Loading series…')); return; }
  const events = qoeEvents(ser);
  const counts = Object.fromEntries(QOE_CATS.map((c) => [c.id, events.filter((e) => e.cat === c).length]));
  const on = (c) => state.qoeCats[c.id] !== false;
  const visibleEvents = () => events.filter((e) => on(e.cat));
  const overlay = () => (state.qoeLines ? visibleEvents().map((e) => ({ t: e.t, color: css('--s' + e.cat.slot) })) : []);
  const dur = Math.max(ser.durationSec || 0, 1);
  root.append(h('div', { class: 'chart-tools' },
    h('span', {}, `bucket ${ser.bucketSec} s · drag to zoom, double-click to reset`),
    h('button', { onclick: () => { state.zoom = null; state.hover = null; renderDetail(); } }, 'Reset zoom'),
    h('span', {}, state.zoom ? `${fmtT(state.zoom[0])} – ${fmtT(state.zoom[1])}` : `0 – ${fmtT(dur)}`),
    h('label', {}, h('input', { type: 'checkbox', checked: state.qoeLines, onchange: (e) => { state.qoeLines = e.target.checked; drawAll(); } }), 'event lines on charts'),
  ));
  root.append(h('div', { class: 'chips' }, ...QOE_CATS.map((c) => h('span', { class: 'chip cat' + (on(c) ? ' on' : ''), style: `--c: var(--s${c.slot})`, onclick: () => { state.qoeCats[c.id] = !on(c); renderDetail(); } }, c.name, h('span', { class: 'n' }, counts[c.id])))));
  const grid = h('div', { class: 'charts' });
  root.append(grid);
  for (const def of [QOE_BITRATE, QOE_OWD, QOE_LATENCY]) {
    const present = def.series.filter(([key]) => ser.series[key] && ser.series[key].length);
    if (!present.length && def.optional) continue;
    if (!present.length) { grid.append(h('div', { class: 'chart wide' }, h('div', { class: 'head' }, h('span', { class: 'title' }, def.title)), h('div', { class: 'hint' }, 'no data in this session'))); continue; }
    grid.append(new TimelineChart(def, present, ser, { tall: true, overlay }).el);
  }
  grid.append(new QoEStrip(events, on).el);
  // The events as a list, for reading exact times.
  const list = visibleEvents();
  const panel = h('div', { class: 'panel' }, h('h3', {}, `${list.length} events in the selected categories`));
  panel.append(h('div', { class: 'scroll', style: 'max-height: 40vh' }, h('table', { class: 'grid' }, h('thead', {}, h('tr', {}, h('th', {}, 't'), h('th', {}, 'category'), h('th', {}, 'event'))),
    h('tbody', {}, ...list.slice(0, 2000).map((e) => h('tr', { title: 'click to zoom ±5 s', onclick: () => { state.zoom = [Math.max(0, e.t - 5), e.t + 5]; state.hover = null; renderDetail(); } },
      h('td', { class: 't num' }, fmtT(e.t)), h('td', {}, h('span', { class: 'badge', style: `border-color: var(--s${e.cat.slot}); color: var(--s${e.cat.slot})` }, e.cat.name.split(' (')[0])), h('td', {}, e.text)))))));
  root.append(panel);
  requestAnimationFrame(drawAll);
}

// One lane per QoE category; a tick per event, hover lists the events under the cursor.
class QoEStrip extends ChartBase {
  constructor(events, on) {
    super();
    this.events = events; this.on = on;
    this.el = h('div', { class: 'chart strip wide qoe' }, h('div', { class: 'head' }, h('span', { class: 'title' }, 'QoE events'), h('span', { class: 'unit' }, 'one lane per category; hover for details')), this.canvas, this.tip);
  }
  draw() {
    const g = this.setup();
    if (!g) return;
    const { ctx, w, x0, x1, x } = g;
    const lanes = QOE_CATS.filter((c) => this.on(c));
    const top = PAD.t, bottom = g.h - PAD.b, laneH = lanes.length ? (bottom - top) / lanes.length : 1;
    ctx.textAlign = 'right'; ctx.textBaseline = 'middle'; ctx.fillStyle = css('--muted');
    lanes.forEach((c, i) => ctx.fillText(c.lane, PAD.l - 6, top + (i + 0.5) * laneH));
    ctx.strokeStyle = css('--grid');
    for (let i = 1; i < lanes.length; i++) { const yy = Math.round(top + i * laneH) + 0.5; ctx.beginPath(); ctx.moveTo(PAD.l, yy); ctx.lineTo(w - PAD.r, yy); ctx.stroke(); }
    this.drawXAxis(g, bottom);
    for (const e of this.events) {
      const i = lanes.indexOf(e.cat);
      if (i < 0 || e.t < x0 || e.t > x1) continue;
      const px = Math.round(x(e.t)) + 0.5;
      ctx.strokeStyle = css('--s' + e.cat.slot); ctx.lineWidth = e.bucket ? 1 : 2;
      ctx.beginPath(); ctx.moveTo(px, top + i * laneH + 3); ctx.lineTo(px, top + (i + 1) * laneH - 3); ctx.stroke();
    }
    this.drawDragAndHover(g, top, bottom);
    if (state.hover && state.hover.owner === this) {
      const win = (x1 - x0) / 120;
      const near = this.events.filter((e) => lanes.includes(e.cat) && Math.abs(e.t - state.hover.t) <= win).slice(0, 10);
      this.placeTip(near.map((e) => `<div><b class="num">${fmtT(e.t)}</b> <span class="badge" style="border-color:var(--s${e.cat.slot});color:var(--s${e.cat.slot})">${e.cat.id}</span> ${escapeHTML(e.text)}</div>`).join(''));
    } else this.placeTip('');
  }
}

// -- Records ----------------------------------------------------------------
async function loadRecords(quiet) {
  const dir = state.selected, r = state.rec;
  if (!dir) return;
  try {
    const data = await api(sessionURL(dir, 'records'), { file: r.file, kind: r.kind, q: r.q, from: r.from, to: r.to, offset: r.offset, limit: r.limit });
    if (state.selected !== dir) return;
    r.data = data; r.error = null;
  } catch (e) { r.error = e.message; }
  if (state.tab === 'records' && !quiet) renderDetail();
  else if (state.tab === 'records') renderDetail();
}

function renderRecords(root, S) {
  const r = state.rec, files = S.files.map((f) => f.name.replace('.jsonl', ''));
  if (!files.includes(r.file)) r.file = files[0] || 'events';
  const reload = (reset) => { if (reset) { r.offset = 0; r.open = new Set(); } r.data = null; renderDetail(); loadRecords(); };
  const kinds = (r.data && r.data.file === r.file && r.data.kinds) || ((S.files.find((f) => f.name === r.file + '.jsonl') || {}).kinds) || {};
  const total = r.data ? r.data.total : 0;
  const tools = h('div', { class: 'rec-tools' },
    h('label', {}, 'file ', h('select', { onchange: (e) => { r.file = e.target.value; r.kind = ''; reload(true); } }, ...files.map((f) => h('option', { value: f, selected: f === r.file }, `${f}.jsonl (${fmtNum((S.files.find((x) => x.name === f + '.jsonl') || {}).lines)})`)))),
    h('label', {}, 'kind ', h('select', { onchange: (e) => { r.kind = e.target.value; reload(true); } }, h('option', { value: '' }, 'all'), ...Object.entries(kinds).sort((a, b) => b[1] - a[1]).map(([k, n]) => h('option', { value: k, selected: k === r.kind }, `${k} (${n})`)))),
    h('label', {}, 'contains ', h('input', { type: 'search', value: r.q, placeholder: 'substring of the JSON line', onchange: (e) => { r.q = e.target.value; reload(true); } })),
    h('label', {}, 't from ', h('input', { type: 'number', step: 'any', value: r.from, placeholder: 's', onchange: (e) => { r.from = e.target.value; reload(true); } })),
    h('label', {}, 'to ', h('input', { type: 'number', step: 'any', value: r.to, placeholder: 's', onchange: (e) => { r.to = e.target.value; reload(true); } })),
    h('label', {}, 'per page ', h('select', { onchange: (e) => { r.limit = +e.target.value; reload(true); } }, ...[50, 100, 250, 500, 1000].map((n) => h('option', { value: n, selected: n === r.limit }, n)))),
    h('span', { class: 'pager' },
      h('button', { disabled: r.offset === 0, onclick: () => { r.offset = Math.max(0, r.offset - r.limit); reload(); } }, '‹ prev'),
      h('span', { class: 'num' }, r.data ? `${total ? r.offset + 1 : 0}–${Math.min(total, r.offset + r.limit)} of ${fmtNum(total)}` : '…'),
      h('button', { disabled: !r.data || r.offset + r.limit >= total, onclick: () => { r.offset += r.limit; reload(); } }, 'next ›'),
      h('a', { href: fileURL(S.session.dir, r.file + '.jsonl'), class: 'hint' }, 'download')),
  );
  root.append(tools);
  if (r.error) root.append(h('div', { class: 'error' }, r.error));
  if (!r.data) { root.append(h('div', { class: 'placeholder' }, 'Loading…')); return; }
  const t0 = r.data.createdMs;
  const body = h('tbody');
  r.data.records.forEach((rec, i) => {
    const idx = r.offset + i;
    const open = r.open.has(idx);
    const t = (rec.t - t0) / 1000;
    const tr = h('tr', { class: open ? 'open' : '' },
      h('td', { class: 'num hint' }, idx + 1),
      h('td', { class: 't num', title: new Date(rec.t).toISOString() + (rec.tClient ? '\nbrowser: ' + new Date(rec.tClient).toISOString() : '') }, fmtT(t)),
      h('td', { class: 'kind' }, rec.kind, rec.data && rec.data.kind && rec.kind === 'event' ? h('span', { class: 'hint' }, ' · ' + rec.data.kind) : null),
      h('td', { class: 'sum', onclick: () => { if (r.open.has(idx)) r.open.delete(idx); else r.open.add(idx); renderDetail(); } }, open ? JSON.stringify(rec, null, 1) : flat(rec.data)));
    body.append(tr);
  });
  root.append(h('div', { class: 'scroll', style: 'max-height: calc(100vh - 260px)' }, h('table', { class: 'grid records' }, h('thead', {}, h('tr', {}, h('th', {}, '#'), h('th', {}, 't'), h('th', {}, 'kind'), h('th', {}, 'data (click to expand)'))), body)));
  if (!r.data.records.length) root.append(h('div', { class: 'hint' }, 'No records match.'));
}

// -- Meta -------------------------------------------------------------------
async function loadMeta() {
  const dir = state.selected;
  try { const m = await api(sessionURL(dir, 'meta')); if (state.selected === dir) { state.meta = m; renderDetail(); } } catch (e) { state.meta = { error: e.message }; renderDetail(); }
}
function renderMeta(root) {
  const m = state.meta;
  if (!m) { root.append(h('div', { class: 'placeholder' }, 'Loading meta.json…')); return; }
  const { offer, answer, ...rest } = m;
  root.append(h('div', { class: 'panel' }, h('h3', {}, 'meta.json'), h('pre', {}, JSON.stringify(rest, null, 2))));
  for (const [name, sdp] of [['offer (browser)', offer], ['answer (server)', answer]]) if (sdp) root.append(h('div', { class: 'panel' }, h('details', {}, h('summary', {}, 'SDP ' + name), h('pre', {}, sdp))));
}

// ---------------------------------------------------------------------------
// Boot
// ---------------------------------------------------------------------------
$('search').addEventListener('input', renderList);
$('sort').addEventListener('change', renderList);
$('refresh').addEventListener('click', () => { loadSessions(); refreshSelected(); });
window.addEventListener('resize', () => requestAnimationFrame(drawAll));
window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => requestAnimationFrame(drawAll));
window.addEventListener('hashchange', () => { const [dir, tab] = location.hash.slice(1).split('/'); if (dir && decodeURIComponent(dir) !== state.selected) select(decodeURIComponent(dir), tab); });
(async () => {
  await loadSessions();
  const [dir, tab] = location.hash.slice(1).split('/');
  if (dir) select(decodeURIComponent(dir), ['overview', 'qoe', 'charts', 'frames', 'records', 'meta'].includes(tab) ? tab : 'overview');
  scheduleAuto();
})();
