// agent-harness web chat. Plain JS, no build step. Talks to:
//   GET  /api/meta     -> states, transitions, hook points, hooks, patterns
//   POST /api/chat     -> text/event-stream of harness events for one turn
//   POST /api/approve  -> answer a pending approval (the "SPA" hook)
//   POST /api/reset    -> forget this browser's session
'use strict';

const $ = (s, el = document) => el.querySelector(s);
const el = (tag, attrs = {}, ...kids) => {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') n.className = v;
    else if (k === 'text') n.textContent = v;
    else if (k.startsWith('on')) n.addEventListener(k.slice(2), v);
    else n.setAttribute(k, v);
  }
  for (const c of kids) if (c != null) n.append(c);
  return n;
};
const svgEl = (tag, attrs = {}) => {
  const n = document.createElementNS('http://www.w3.org/2000/svg', tag);
  for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, v);
  return n;
};
const fmtMs = ns => ns == null ? '' : (ns / 1e6 < 1 ? (ns / 1e3).toFixed(0) + 'µs' : (ns / 1e6).toFixed(ns / 1e6 < 10 ? 1 : 0) + 'ms');
const clip = (s, n = 160) => { s = String(s ?? '').replace(/\s+/g, ' ').trim(); return s.length > n ? s.slice(0, n) + '…' : s; };

const sessionId = localStorage.getItem('harness.session') || (crypto.randomUUID ? crypto.randomUUID() : String(Math.random()).slice(2));
localStorage.setItem('harness.session', sessionId);

const state = {
  meta: null,
  pattern: localStorage.getItem('harness.pattern') || 'single',
  busy: false,
  turnStart: 0,
  runs: new Map(),
  focus: null,      // run id shown in the diagram
  visited: new Set(),
  counts: new Map(),
  hookTotal: 0,
  spans: [],
  bubble: null,
};

const SUGGEST = [
  'What time is it?', 'Compute (17+4)*3', 'Search the docs for budget', 'Send an email to Jan',
  'Delete all records', 'My password is hunter2', 'Show my AHV number', 'flaky: what time is it?', 'loop', 'Tell me something long',
];
const SHORT = { BeforeRun: 'BeforeRun', BeforeModel: 'BeforeModel', WrapModel: 'WrapModel', OnDelta: 'OnDelta', AfterModel: 'AfterModel', BeforeTool: 'BeforeTool', WrapTool: 'WrapTool', AfterTool: 'AfterTool', AfterTurn: 'AfterTurn', CheckOutput: 'CheckOutput', AfterRun: 'AfterRun', OnTransition: 'OnTransition' };

// ---------- boot ----------
async function boot() {
  const meta = await (await fetch('api/meta')).json();
  state.meta = meta;
  if (!meta.patterns.some(p => p.id === state.pattern)) state.pattern = 'single';
  $('#provider').textContent = meta.provider;
  renderPatterns();
  renderMachine();
  renderMatrix();
  renderBudget({ usage: {}, cost_usd: 0, model_calls: 0, tool_calls: 0 }, meta.limits);
  $('#auto-approve').checked = localStorage.getItem('harness.auto') === '1';
  $('#auto-approve').addEventListener('change', e => localStorage.setItem('harness.auto', e.target.checked ? '1' : '0'));
  $('#composer').addEventListener('submit', e => { e.preventDefault(); send(); });
  $('#composer-input').addEventListener('keydown', e => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); send(); } });
  $('#reset').addEventListener('click', reset);
  $('#filters').addEventListener('change', applyFilters);
  for (const a of document.querySelectorAll('.lang a')) a.addEventListener('click', e => { e.preventDefault(); setView(a.dataset.view); });
  setView('chat');
  greet();
}

function setView(v) {
  document.body.dataset.view = v;
  for (const a of document.querySelectorAll('.lang a')) a.toggleAttribute('aria-current', a.dataset.view === v);
}

function greet() {
  addBot('Hi — this chat runs on agent-harness. Every message is one outer-loop episode; the panels on the right show each state transition and every hook invocation as it happens. Pick a pattern above or try a suggestion below.', null, 'harness');
}

// ---------- patterns ----------
function renderPatterns() {
  const box = $('#patterns');
  box.replaceChildren();
  for (const p of state.meta.patterns) {
    box.append(el('button', { type: 'button', role: 'radio', 'aria-checked': String(p.id === state.pattern), 'data-id': p.id, title: p.summary, text: p.label,
      onclick: () => { state.pattern = p.id; localStorage.setItem('harness.pattern', p.id); renderPatterns(); } }));
  }
  const p = state.meta.patterns.find(x => x.id === state.pattern);
  $('#pattern-info').replaceChildren(document.createTextNode(p.summary), el('span', { class: 'src', text: 'source: ' + p.source }));
  const chips = $('#chips');
  chips.replaceChildren();
  const list = state.pattern === 'single' ? SUGGEST : [p.try, ...SUGGEST.slice(0, 3)];
  for (const s of list) chips.append(el('button', { type: 'button', text: s, onclick: () => { $('#composer-input').value = s; send(); } }));
}

// ---------- state machine diagram ----------
const POS = {
  start: [44, 62], prepare: [146, 62], compose: [262, 62], model: [378, 62], route: [494, 62], verify: [622, 62], done: [756, 62],
  observe: [262, 160], tools: [494, 160], stopped: [622, 222], failed: [756, 222],
};
const NW = 86, NH = 30;
function renderMachine() {
  const svg = $('#machine');
  svg.replaceChildren();
  const defs = svgEl('defs');
  for (const [id, color] of [['arr', 'rgba(13,42,34,.4)'], ['arr-hot', '#009368'], ['arr-inner', 'rgba(14,124,134,.7)']]) {
    const m = svgEl('marker', { id, viewBox: '0 0 10 10', refX: 9, refY: 5, markerWidth: 8, markerHeight: 8, markerUnits: 'userSpaceOnUse', orient: 'auto-start-reverse' });
    m.append(svgEl('path', { d: 'M0,0 L10,5 L0,10 z', fill: color }));
    defs.append(m);
  }
  svg.append(defs);
  const inner = new Set(['compose>model', 'model>route', 'route>tools', 'tools>observe', 'observe>compose']);
  for (const { from, to } of state.meta.transitions) {
    if (to === 'stopped' || to === 'failed') continue;
    const d = edgePath(from, to);
    const isInner = inner.has(from + '>' + to);
    svg.append(svgEl('path', { d, class: 'edge' + (isInner ? ' inner' : ''), id: `e-${from}-${to}`, 'marker-end': `url(#${isInner ? 'arr-inner' : 'arr'})` }));
  }
  const lbl = (x, y, t, cls = '') => { const n = svgEl('text', { x, y, class: 'lbl ' + cls }); n.textContent = t; svg.append(n); };
  lbl(318, 116, 'INNER LOOP · one model/tool turn', 'loop');
  lbl(330, 14, 'verify → compose: repair (revise verdict)');
  lbl(232, 226, 'any state → stopped (on purpose) | failed (error) →');
  lbl(8, 18, 'OUTER LOOP · one task / episode', 'outer');
  for (const s of state.meta.states) {
    const [x, y] = POS[s] || [0, 0];
    const g = svgEl('g', { class: 'node' + (['done', 'stopped', 'failed'].includes(s) ? ' terminal' : '') + (s === 'failed' || s === 'stopped' ? ' bad' : ''), id: 'n-' + s });
    g.append(svgEl('rect', { x: x - NW / 2, y: y - NH / 2, width: NW, height: NH, rx: 15 }));
    const t = svgEl('text', { x, y }); t.textContent = s; g.append(t);
    const title = svgEl('title'); title.textContent = s; g.append(title);
    svg.append(g);
  }
}
function edgePath(a, b) {
  const [x1, y1] = POS[a], [x2, y2] = POS[b];
  if (a === 'verify' && b === 'compose') return `M${x1},${y1 - NH / 2} C${x1 - 40},${y1 - 58} ${x2 + 40},${y2 - 58} ${x2},${y2 - NH / 2}`;
  if (y1 === y2) { const s = Math.sign(x2 - x1); return `M${x1 + s * NW / 2},${y1} L${x2 - s * (NW / 2 + 2)},${y2}`; }
  if (x1 === x2) { const s = Math.sign(y2 - y1); return `M${x1},${y1 + s * NH / 2} L${x2},${y2 - s * (NH / 2 + 2)}`; }
  return `M${x1},${y1} L${x2},${y2}`;
}
function showTransition(ev) {
  if (state.focus !== ev.run_id) { state.focus = ev.run_id; state.visited.clear(); }
  state.visited.add(ev.from); state.visited.add(ev.to);
  for (const s of state.meta.states) {
    const n = $('#n-' + s);
    n.classList.toggle('visited', state.visited.has(s));
    n.classList.toggle('current', s === ev.to);
  }
  for (const p of document.querySelectorAll('#machine .edge.hot')) { p.classList.remove('hot'); p.setAttribute('marker-end', p.classList.contains('inner') ? 'url(#arr-inner)' : 'url(#arr)'); }
  const e = $(`#e-${ev.from}-${ev.to}`);
  if (e) { e.classList.add('hot'); e.setAttribute('marker-end', 'url(#arr-hot)'); }
  $('#current-state').textContent = `${ev.agent} · d${ev.depth} · ${ev.to}`;
}

// ---------- runs table ----------
function trackRun(ev) {
  if (!ev.run_id) return;
  let r = state.runs.get(ev.run_id);
  if (!r) { r = { id: ev.run_id, agent: ev.agent, depth: ev.depth, turn: 0, state: ev.kind === 'pattern' ? 'pattern:' + ev.hook : 'start', stop: '' }; state.runs.set(ev.run_id, r); }
  if (ev.turn) r.turn = ev.turn;
  if (ev.kind === 'transition') r.state = ev.to;
  if (ev.kind === 'run.end') r.stop = ev.outcome;
  if (ev.kind === 'pattern' && (ev.outcome === 'done' || ev.outcome === 'end')) r.stop = ev.text ? clip(ev.text, 24) : 'done';
  renderRuns();
}
function renderRuns() {
  const tb = $('#runs tbody');
  tb.replaceChildren();
  for (const r of [...state.runs.values()].slice(-12)) {
    tb.append(el('tr', {}, el('td', { text: '  '.repeat(r.depth) + r.id }), el('td', { text: r.depth }), el('td', { text: r.turn || '' }),
      el('td', { class: 'st', text: r.state }), el('td', { text: r.stop })));
  }
}

// ---------- hook matrix ----------
function renderMatrix() {
  const t = $('#matrix');
  t.replaceChildren();
  const head = el('tr', {}, el('th', { text: 'hook \\ point' }));
  for (const p of state.meta.points) head.append(el('th', { text: SHORT[p] || p, title: p }));
  t.append(el('thead', {}, head));
  const body = el('tbody');
  for (const h of state.meta.hooks) {
    const tr = el('tr', {}, el('th', { title: h.role }, h.name, el('small', { text: h.role })));
    for (const p of state.meta.points) {
      const has = (h.points || []).includes(p);
      tr.append(el('td', { id: `c-${h.name}-${p}`, class: has ? '' : 'na', text: has ? '0' : '·', title: has ? `${h.name} @ ${p}` : `${h.name} does not implement ${p}` }));
    }
    body.append(tr);
  }
  t.append(body);
}
function resetMatrix() {
  state.counts.clear(); state.hookTotal = 0;
  for (const td of document.querySelectorAll('#matrix td:not(.na)')) { td.textContent = '0'; td.className = ''; td.title = td.title.split('\n')[0]; }
  $('#hook-total').textContent = '0 calls';
}
const BAD = new Set(['deny', 'halt', 'block', 'revise', 'stop', 'error']);
function countHook(ev) {
  const td = document.getElementById(`c-${ev.hook}-${ev.point}`);
  state.hookTotal++;
  $('#hook-total').textContent = state.hookTotal + ' calls';
  if (!td) return;
  const k = ev.hook + ev.point;
  const n = (state.counts.get(k) || 0) + 1;
  state.counts.set(k, n);
  td.textContent = String(n);
  const trivial = ev.point === 'OnTransition' || (ev.point === 'WrapTool' && !String(ev.text || '').includes(': '));
  if (BAD.has(ev.outcome)) td.className = 'deny';
  else if (ev.text && !trivial && td.className !== 'deny') td.className = 'act';
  else if (!td.className) td.className = 'on';
  if (ev.text && !trivial) td.title = td.title.split('\n')[0] + '\nlast: ' + ev.outcome + ' — ' + ev.text;
  td.classList.add('flash');
  setTimeout(() => td.classList.remove('flash'), 180);
}

// ---------- budget & metrics ----------
function renderBudget(spent, limits) {
  const u = spent.usage || {};
  const used = (u.input || 0) + (u.output || 0);
  const pct = limits.Tokens ? Math.min(100, 100 * used / limits.Tokens) : 0;
  const fill = $('#budget-fill');
  fill.style.width = pct.toFixed(1) + '%';
  fill.classList.toggle('hot', pct > 80);
  const rows = [
    ['tokens', `${used.toLocaleString()} / ${(limits.Tokens || 0).toLocaleString()}`],
    ['input · output', `${(u.input || 0).toLocaleString()} · ${(u.output || 0).toLocaleString()}`],
    ['cached · reasoning', `${u.cached || 0} · ${u.reasoning || 0}`],
    ['cost', `$${(spent.cost_usd || 0).toFixed(5)} / $${(limits.CostUSD || 0).toFixed(2)}`],
    ['model calls', `${spent.model_calls || 0} / ${limits.ModelCalls || '∞'}`],
    ['tool calls', `${spent.tool_calls || 0} / ${limits.ToolCalls || '∞'}`],
    ['loop detection', limits.RepeatCalls ? `${limits.RepeatCalls}× identical call` : 'off'],
  ];
  $('#budget').replaceChildren(...rows.flatMap(([k, v]) => [el('dt', { text: k }), el('dd', { text: v })]));
}
// The budget hook notes "+N tokens → used/limit" at AfterModel; use it to
// move the meter live, before the turn's final numbers arrive.
function liveBudget(ev) {
  if (ev.hook !== 'budget' || ev.point !== 'AfterModel' || !ev.text) return;
  const m = /→ (\d+)\/(\d+)/.exec(ev.text);
  if (!m) return;
  const used = +m[1], lim = +m[2];
  const pct = Math.min(100, 100 * used / lim);
  $('#budget-fill').style.width = pct.toFixed(1) + '%';
  $('#budget-fill').classList.toggle('hot', pct > 80);
  const dd = $('#budget dd');
  if (dd) dd.textContent = `${used.toLocaleString()} / ${lim.toLocaleString()}`;
}
function renderMetrics(m) {
  if (!m) return;
  const rows = [];
  const c = m.counters || {}, h = m.histograms || {};
  for (const k of Object.keys(c).sort()) rows.push([k, String(Math.round(c[k] * 1000) / 1000)]);
  for (const k of Object.keys(h).sort()) { const s = h[k]; rows.push([k, `n=${s.count} p50=${fmtNum(s.p50)} p95=${fmtNum(s.p95)}`]); }
  $('#metrics').replaceChildren(...rows.flatMap(([k, v]) => [el('dt', { text: k, title: k }), el('dd', { text: v })]));
}
const fmtNum = v => v == null ? '–' : (Math.abs(v) >= 100 ? v.toFixed(0) : v.toFixed(3));

// ---------- spans ----------
function addSpan(sd) { state.spans.push(sd); }
function renderSpans() {
  const list = $('#spans');
  list.replaceChildren();
  const byId = new Map(state.spans.map(s => [s.span_id, s]));
  const depth = s => { let d = 0, p = s; while (p && p.parent_id && byId.has(p.parent_id) && d < 12) { p = byId.get(p.parent_id); d++; } return d; };
  const t0 = Math.min(...state.spans.map(s => Date.parse(s.start)));
  const t1 = Math.max(...state.spans.map(s => Date.parse(s.end || s.start)));
  const span = Math.max(1, t1 - t0);
  const sorted = [...state.spans].sort((a, b) => (Date.parse(a.start) - Date.parse(b.start)) || (depth(a) - depth(b)));
  for (const s of sorted) {
    const st = Date.parse(s.start), en = Date.parse(s.end || s.start);
    const attrs = s.attrs || {};
    const extra = attrs['gen_ai.usage.input_tokens'] != null ? ` · ${attrs['gen_ai.usage.input_tokens']}→${attrs['gen_ai.usage.output_tokens']} tok` : '';
    const bar = el('span', { class: 'bar' }, el('i', { style: `left:${(100 * (st - t0) / span).toFixed(2)}%;width:${Math.max(1, 100 * (en - st) / span).toFixed(2)}%` }));
    list.append(el('li', { title: JSON.stringify(attrs, null, 1) },
      el('span', { class: 'nm' + (s.status === 'error' ? ' err' : ''), text: '\u00a0\u00a0'.repeat(depth(s)) + s.name + extra }),
      el('span', { class: 'd', text: (en - st) + 'ms' }), bar));
  }
  $('#span-total').textContent = state.spans.length + ' spans';
}

// ---------- event stream ----------
function streamLine(ev) {
  const list = $('#stream');
  const kind = ev.kind.startsWith('run.') ? 'run' : ev.kind;
  const t = '+' + Math.max(0, Date.parse(ev.time) - state.turnStart) + 'ms';
  const ind = '  '.repeat(ev.depth || 0);
  const body = el('span', {}, ind, el('span', { class: 'agent', text: ev.agent || '' }), ' ');
  const add = (txt, cls) => body.append(cls ? el('span', { class: cls, text: txt }) : txt);
  switch (ev.kind) {
    case 'transition': add(`${ev.from} → ${ev.to}`); if (ev.turn) add(`  (turn ${ev.turn})`, 'note'); break;
    case 'hook': add(`${ev.point} · ${ev.hook} → `); add(ev.outcome, BAD.has(ev.outcome) ? 'bad' : ''); if (ev.text) add(`  ${clip(ev.text, 200)}`, 'note'); add(`  ${fmtMs(ev.duration_ns)}`, 'note'); break;
    case 'delta': add(JSON.stringify(ev.text), 'note'); break;
    case 'model': { const d = ev.data || {}; const u = d.usage || {}; add(`reply ${d.tool_calls ? d.tool_calls + ' tool call(s)' : clip(ev.text, 90)}  ${u.input || 0}→${u.output || 0} tok  ${fmtMs(ev.duration_ns)}`); break; }
    case 'tool': { const d = ev.data || {}; add(`${ev.hook}(${clip(d.args, 80)}) → `); add(ev.outcome, ev.outcome === 'ok' ? '' : 'bad'); add(`  ${clip(ev.text || d.result, 140)}`, 'note'); break; }
    case 'pattern': add(`${ev.hook} · ${ev.outcome}`); if (ev.text) add(`  ${clip(ev.text, 200)}`, 'note'); break;
    case 'run.start': add('run.start  '); add(clip(ev.text, 120), 'note'); break;
    case 'run.end': add('run.end → '); add(ev.outcome, ev.outcome === 'completed' ? '' : 'bad'); { const d = ev.data || {}; if (d.detail) add('  ' + d.detail, 'note'); } break;
    case 'approval': add(`approval needed for ${ev.text}`, 'bad'); break;
    default: add(clip(ev.text, 160));
  }
  const li = el('li', { 'data-kind': kind }, el('span', { class: 't', text: t }), el('span', { class: 'k ' + kind, text: ev.kind }), body);
  if (!filterSet().has(kind)) li.classList.add('hidden');
  list.append(li);
  while (list.children.length > 2500) list.firstChild.remove();
  if (list.scrollHeight - list.scrollTop - list.clientHeight < 120) list.scrollTop = list.scrollHeight;
}
function filterSet() { return new Set([...document.querySelectorAll('#filters input:checked')].map(i => i.value)); }
function applyFilters() { const f = filterSet(); for (const li of $('#stream').children) li.classList.toggle('hidden', !f.has(li.dataset.kind)); }

// ---------- chat ----------
function addUser(text) { const m = el('li', { class: 'msg user', text }); $('#messages').append(m); scrollChat(); }
function addBot(text, metaNode, who) {
  const m = el('li', { class: 'msg bot' }, el('span', { class: 'who', text: who || 'agent' }), el('span', { class: 'body', text }));
  if (metaNode) m.append(metaNode);
  $('#messages').append(m); scrollChat();
  return m;
}
function scrollChat() { const b = $('#messages'); b.scrollTop = b.scrollHeight; }

async function send() {
  const input = $('#composer-input');
  const text = input.value.trim();
  if (!text || state.busy) return;
  input.value = '';
  state.busy = true; $('#send').disabled = true;
  addUser(text);
  const bubble = el('li', { class: 'msg bot' }, el('span', { class: 'who', text: state.pattern === 'single' ? 'assistant' : state.pattern }), el('span', { class: 'body typing', text: '' }), el('span', { class: 'live' }));
  $('#messages').append(bubble); scrollChat();
  state.bubble = bubble;
  state.turnStart = Date.now(); state.runs.clear(); state.spans = []; state.focus = null; state.visited.clear();
  resetMatrix(); renderRuns(); $('#stream').replaceChildren(); $('#spans').replaceChildren();
  try {
    const res = await fetch('api/chat', { method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ session: sessionId, pattern: state.pattern, message: text, auto_approve: $('#auto-approve').checked }) });
    if (!res.ok) throw new Error(await res.text());
    await readSSE(res.body, onSSE);
  } catch (err) {
    $('.body', bubble).classList.remove('typing');
    $('.body', bubble).textContent = 'Request failed: ' + err.message;
    bubble.classList.add('failed');
  } finally {
    state.busy = false; $('#send').disabled = false; input.focus();
  }
}

async function readSSE(stream, handle) {
  const reader = stream.getReader();
  const dec = new TextDecoder();
  let buf = '';
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += dec.decode(value, { stream: true });
    let i;
    while ((i = buf.indexOf('\n\n')) >= 0) {
      const chunk = buf.slice(0, i); buf = buf.slice(i + 2);
      let event = 'message', data = '';
      for (const line of chunk.split('\n')) {
        if (line.startsWith('event: ')) event = line.slice(7);
        else if (line.startsWith('data: ')) data += line.slice(6);
      }
      if (data) handle(event, JSON.parse(data));
    }
  }
}

function onSSE(event, data) {
  if (event === 'span') { addSpan(data); return; }
  if (event === 'done') { finish(data); return; }
  const ev = data;
  streamLine(ev);
  trackRun(ev);
  switch (ev.kind) {
    case 'transition': showTransition(ev); break;
    case 'hook': countHook(ev); liveBudget(ev); break;
    case 'delta': { const live = $('.live', state.bubble); if (live.dataset.run !== ev.run_id) { live.dataset.run = ev.run_id; live.textContent = (ev.agent ? ev.agent + ': ' : ''); } live.textContent += ev.text; scrollChat(); break; }
    case 'approval': askApproval(ev); break;
  }
}

function askApproval(ev) {
  const d = ev.data || {};
  const card = el('li', { class: 'approval' },
    el('p', { class: 'ix', text: `SPA · APPROVAL REQUIRED · ${d.risk || ''} tool` }),
    el('strong', { text: `${ev.agent} wants to call ${d.tool}` }),
    el('pre', { text: JSON.stringify(d.arguments, null, 2) }));
  const decide = async ok => {
    card.classList.add('decided');
    for (const b of card.querySelectorAll('button')) b.disabled = true;
    await fetch('api/approve', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id: d.id, approve: ok }) });
    card.append(el('p', { class: 'ix', text: ok ? 'approved' : 'denied' }));
  };
  card.append(el('div', { class: 'row' },
    el('button', { class: 'btn pri sm', type: 'button', text: 'Approve', onclick: () => decide(true) }),
    el('button', { class: 'btn ghost sm', type: 'button', text: 'Deny', onclick: () => decide(false) })));
  const live = $('.live', state.bubble);
  if (live && !live.textContent) live.textContent = 'waiting for your approval…';
  $('#messages').insertBefore(card, state.bubble);
  scrollChat();
}

function finish(data) {
  const b = state.bubble;
  const body = $('.body', b);
  body.classList.remove('typing');
  $('.live', b).remove();
  const r = data.result;
  if (data.error && !r) { body.textContent = 'Error: ' + data.error; b.classList.add('failed'); }
  else if (r) {
    body.textContent = r.output || `(no answer — ${r.stop}: ${r.detail || ''})`;
    if (r.stop !== 'completed') b.classList.add(r.stop === 'failed' ? 'failed' : 'stopped');
    $('.who', b).textContent = r.agent;
    const u = r.usage || {};
    const meta = el('span', { class: 'meta' },
      el('span', { class: r.stop === 'completed' ? 'stop-completed' : 'stop-other', text: r.stop }),
      r.detail ? ` (${r.detail})` : '',
      ` · ${r.turns} turn${r.turns === 1 ? '' : 's'} · ${(u.input || 0) + (u.output || 0)} tok · ${r.model_calls} model / ${r.tool_calls} tool calls · ${r.duration_ms} ms`,
      el('br'), (r.path || []).join(' → '));
    b.append(meta);
  }
  renderBudget(data.spent || {}, data.limits || state.meta.limits);
  renderMetrics(data.metrics);
  renderSpans();
  scrollChat();
}

async function reset() {
  await fetch('api/reset', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ session: sessionId }) });
  $('#messages').replaceChildren(); $('#stream').replaceChildren(); $('#spans').replaceChildren();
  state.runs.clear(); renderRuns(); resetMatrix(); renderMachine();
  renderBudget({ usage: {} }, state.meta.limits); $('#metrics').replaceChildren();
  $('#current-state').textContent = 'idle';
  greet();
}

boot().catch(err => { document.body.prepend(el('p', { class: 'wrap', text: 'Failed to load: ' + err.message })); });
