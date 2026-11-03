'use strict';

// Small dependency-free canvas time-series chart (from the v1 page).
const $ = (id) => document.getElementById(id);
const PALETTE = ['#2f6fdb', '#e8703a', '#1f9e74', '#b04fc9', '#d4a21a', '#6b6b70', '#d6336c'];

class Chart {
  constructor(canvas, series, { windowSec = 90 } = {}) {
    this.canvas = canvas;
    this.series = series.map((s, i) => ({ color: PALETTE[i % PALETTE.length], ...s, pts: [] }));
    this.windowSec = windowSec;
  }
  reset() { for (const s of this.series) s.pts = []; }
  push(t, values) {
    for (const s of this.series) {
      const v = values[s.key];
      if (v !== undefined && v !== null && Number.isFinite(v)) s.pts.push([t, v]);
      const cutoff = t - this.windowSec * 1000;
      while (s.pts.length && s.pts[0][0] < cutoff) s.pts.shift();
    }
  }
  draw(now) {
    const c = this.canvas;
    const dpr = window.devicePixelRatio || 1;
    const w = c.clientWidth;
    const L = 44, R = 8, B = 18;
    const ctx = c.getContext('2d');
    const font = '11px system-ui, sans-serif', rowH = 14;
    ctx.font = font;
    let lx = L, row = 0;
    const legend = this.series.map((s) => {
      const lw = 14 + ctx.measureText(s.name).width;
      if (lx > L && lx + lw > w - R) { lx = L; row++; }
      const pos = { x: lx, y: 9 + row * rowH };
      lx += lw + 14;
      return pos;
    });
    const T = 22 + row * rowH;
    const height = `${180 + row * rowH}px`;
    if (c.style.height !== height) c.style.height = height;
    const h = c.clientHeight;
    if (c.width !== w * dpr || c.height !== h * dpr) { c.width = w * dpr; c.height = h * dpr; }
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, h);
    const css = getComputedStyle(document.documentElement);
    const grid = css.getPropertyValue('--grid').trim();
    const muted = css.getPropertyValue('--muted').trim();
    const t1 = now, t0 = now - this.windowSec * 1000;
    let max = 0;
    for (const s of this.series) for (const [, v] of s.pts) max = Math.max(max, v);
    max = niceMax(max || 1);
    const x = (t) => L + (t - t0) / (t1 - t0) * (w - L - R);
    const y = (v) => T + (1 - v / max) * (h - T - B);
    ctx.font = font;
    ctx.lineWidth = 1;
    for (let i = 0; i <= 4; i++) {
      const v = max * i / 4, yy = y(v);
      ctx.strokeStyle = grid; ctx.beginPath(); ctx.moveTo(L, yy); ctx.lineTo(w - R, yy); ctx.stroke();
      ctx.fillStyle = muted; ctx.textAlign = 'right'; ctx.textBaseline = 'middle';
      ctx.fillText(fmtNum(v), L - 6, yy);
    }
    ctx.textAlign = 'left'; ctx.textBaseline = 'alphabetic';
    ctx.fillText(`last ${this.windowSec}s`, L, h - 4);
    this.series.forEach((s, i) => {
      const { x: lx, y: ly } = legend[i];
      ctx.fillStyle = s.color; ctx.fillRect(lx, ly - 2, 10, 3);
      ctx.fillStyle = muted; ctx.textBaseline = 'middle';
      ctx.fillText(s.name, lx + 14, ly);
      if (s.pts.length < 2) return;
      ctx.strokeStyle = s.color; ctx.lineWidth = 1.75; ctx.beginPath();
      s.pts.forEach(([t, v], i) => (i ? ctx.lineTo(x(t), y(v)) : ctx.moveTo(x(t), y(v))));
      ctx.stroke();
    });
  }
}

function niceMax(v) {
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 2.5, 5, 10]) if (v <= m * p) return m * p;
  return 10 * p;
}
function fmtNum(v) {
  if (v === null || v === undefined || Number.isNaN(v)) return '–';
  if (typeof v !== 'number') return String(v);
  if (Math.abs(v) >= 1e6) return (v / 1e6).toFixed(1) + 'M';
  if (Math.abs(v) >= 1e4) return (v / 1e3).toFixed(0) + 'k';
  if (Math.abs(v) >= 100 || Number.isInteger(v)) return v.toFixed(0);
  if (Math.abs(v) >= 1) return v.toFixed(1);
  return v.toFixed(3);
}
function tilesHTML(tiles) {
  return tiles.map(([label, val, unit]) =>
    `<div class="tile"><div class="label">${label}</div><div class="value">${typeof val === 'string' ? val : fmtNum(val)} <small>${unit || ''}</small></div></div>`).join('');
}
