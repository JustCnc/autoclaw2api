// Load / limit probe against a local CPA instance exposing the autoclaw2api plugin.
const BASE = process.env.BASE || 'http://127.0.0.1:8317';
const KEY = process.env.KEY || '123456';
const MODEL = process.env.MODEL || 'autoclaw/zai_glm-5.3-flash';

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function chat({ stream = false, maxTokens = 16, prompt = 'Reply with exactly: OK', signal } = {}) {
  const t0 = performance.now();
  const res = await fetch(`${BASE}/v1/chat/completions`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${KEY}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({
      model: MODEL,
      messages: [{ role: 'user', content: prompt }],
      max_tokens: maxTokens,
      stream,
    }),
    signal,
  });
  const text = await res.text();
  const ms = performance.now() - t0;
  const out = { status: res.status, ms, text };
  if (res.ok) {
    try {
      const j = JSON.parse(text);
      out.usage = j.usage;
      out.content = j.choices?.[0]?.message?.content;
      out.finish = j.choices?.[0]?.finish_reason;
    } catch {}
  } else {
    out.error = text.slice(0, 300);
  }
  return out;
}

function pct(arr, p) {
  if (!arr.length) return 0;
  const s = [...arr].sort((a, b) => a - b);
  return s[Math.min(s.length - 1, Math.floor((p / 100) * s.length))];
}

async function burst(concurrency, perWorker, opts = {}) {
  const jobs = [];
  for (let w = 0; w < concurrency; w++) jobs.push(w);
  const results = [];
  const t0 = performance.now();
  await Promise.all(
    jobs.map(async () => {
      for (let i = 0; i < perWorker; i++) {
        try {
          results.push(await chat(opts));
        } catch (e) {
          results.push({ status: 0, ms: 0, error: String(e) });
        }
      }
    }),
  );
  const wall = (performance.now() - t0) / 1000;
  const ok = results.filter((r) => r.status === 200);
  const lat = ok.map((r) => r.ms);
  const statuses = {};
  for (const r of results) statuses[r.status] = (statuses[r.status] || 0) + 1;
  const errs = {};
  for (const r of results) {
    if (r.status !== 200) {
      const k = (r.error || '').replace(/\s+/g, ' ').slice(0, 120);
      errs[k] = (errs[k] || 0) + 1;
    }
  }
  return {
    concurrency,
    total: results.length,
    ok: ok.length,
    fail: results.length - ok.length,
    wall: +wall.toFixed(2),
    rps: +(results.length / wall).toFixed(2),
    statuses,
    p50: Math.round(pct(lat, 50)),
    p95: Math.round(pct(lat, 95)),
    max: Math.round(Math.max(0, ...lat)),
    errs,
  };
}

async function streamProbe(maxTokens) {
  const t0 = performance.now();
  const res = await fetch(`${BASE}/v1/chat/completions`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${KEY}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({
      model: MODEL,
      messages: [{ role: 'user', content: 'Write a 300-word plain text description of the ocean.' }],
      max_tokens: maxTokens,
      stream: true,
    }),
  });
  if (!res.ok) {
    const t = await res.text();
    return { status: res.status, error: t.slice(0, 300) };
  }
  const reader = res.body.getReader();
  const dec = new TextDecoder();
  let buf = '';
  let chunks = 0;
  let firstChunkMs = 0;
  let usage = null;
  let contentChars = 0;
  let done = false;
  for (;;) {
    const { value, done: d } = await reader.read();
    if (d) break;
    if (!firstChunkMs) firstChunkMs = performance.now() - t0;
    buf += dec.decode(value, { stream: true });
    let idx;
    while ((idx = buf.indexOf('\n')) >= 0) {
      const line = buf.slice(0, idx).trim();
      buf = buf.slice(idx + 1);
      if (!line.startsWith('data:')) continue;
      const data = line.slice(5).trim();
      if (data === '[DONE]') { done = true; continue; }
      chunks++;
      try {
        const j = JSON.parse(data);
        if (j.usage) usage = j.usage;
        const delta = j.choices?.[0]?.delta?.content;
        if (delta) contentChars += delta.length;
      } catch {}
    }
  }
  const totalMs = performance.now() - t0;
  const completion = usage?.completion_tokens || 0;
  return {
    status: 200,
    ttftMs: Math.round(firstChunkMs),
    totalMs: Math.round(totalMs),
    chunks,
    done,
    usage,
    contentChars,
    tokPerSec: completion ? +(completion / (totalMs / 1000)).toFixed(1) : null,
    decodeTokPerSec: completion && totalMs > firstChunkMs ? +(completion / ((totalMs - firstChunkMs) / 1000)).toFixed(1) : null,
  };
}

const mode = process.argv[2] || 'all';

if (mode === 'all' || mode === 'single') {
  console.log('--- warmup ---');
  console.log(JSON.stringify(await chat({ maxTokens: 16 })).slice(0, 400));
  await sleep(1000);
  console.log('--- sequential x3 ---');
  for (let i = 0; i < 3; i++) {
    const r = await chat({ maxTokens: 16 });
    console.log(JSON.stringify({ status: r.status, ms: Math.round(r.ms), usage: r.usage, err: r.error }));
    await sleep(500);
  }
}

if (mode === 'all' || mode === 'burst') {
  for (const c of [1, 2, 4, 8, 16, 32]) {
    const r = await burst(c, 1, { maxTokens: 16 });
    console.log('BURST', JSON.stringify(r));
    await sleep(4000);
  }
}

if (mode === 'all' || mode === 'sustain') {
  for (const c of [8, 16]) {
    const r = await burst(c, 3, { maxTokens: 16 });
    console.log('SUSTAIN', JSON.stringify(r));
    await sleep(6000);
  }
}

if (mode === 'all' || mode === 'stream') {
  for (const mt of [128, 512]) {
    const r = await streamProbe(mt);
    console.log('STREAM', mt, JSON.stringify(r));
    await sleep(3000);
  }
}
