'use strict';

// ---------- assistant: control room ----------
// Everything that shapes JARVIS, behind the composer's plug: how it runs (model, persona, channels), what it
// remembers (the standing rules you've given it), its skills, tools, MCP servers, scheduled jobs and logs.
// It reads and writes Hermes' own dashboard API through the panel's /hermes/ proxy (assistant.go).

const AIC_TABS = [['overview', 'Overview', 'gauge'], ['memory', 'Memory', 'layers'], ['skills', 'Skills', 'wand-sparkle'],
  ['tools', 'Tools', 'terminal-square'], ['mcp', 'MCP', 'puzzle'], ['jobs', 'Jobs', 'clock-circle'], ['logs', 'Logs', 'document-code']];
const AIC = {
  tab: AIC_TABS.some(([k]) => k === store.get('aicTab')) ? store.get('aicTab') : 'overview',
  d: {}, err: {}, v: 0, mcpTools: {}, runs: {}, memEdit: null, arm: null, armTimer: 0,
  skillQ: '', logFile: 'agent', logLevel: 'ALL', logNoise: false, logTimer: 0,
};

async function hapi(path, method = 'GET', body) {
  const r = await fetch('/hermes/api/' + path, {
    method, credentials: 'same-origin',
    headers: body === undefined ? {} : { 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body),
  });
  const j = await r.json().catch(() => null);
  if (!r.ok) throw new Error((typeof j?.detail === 'string' && j.detail) || j?.error || `Hermes answered ${r.status}`);
  return j;
}

const aicBump = () => { AIC.v++; if (AI.conn) aiRender(); };

async function aicLoad(tab = AIC.tab) {
  try {
    if (tab === 'overview') {
      const [st, model, soul] = await Promise.all([hapi('status'), hapi('model/info'), hapi('profiles/default/soul').catch(() => null)]);
      AIC.d.overview = { st, model, soul: soul?.content || '' };
      api('/api/ai/model').then((r) => { if (r.status === 200) { AI.models = r.models || []; aicBump(); } }).catch(() => {});
      aiGoogle();
      aiTelegramLoad();
    } else if (tab === 'memory') {
      const g = await hapi('learning/graph');
      const nodes = (g.nodes || []).filter((n) => n.kind === 'memory');
      const full = await Promise.all(nodes.map((n) => hapi('learning/node?id=' + encodeURIComponent(n.id)).catch(() => n)));
      AIC.d.memory = nodes.map((n, i) => ({ ...n, content: full[i].content || n.label }));
    } else if (tab === 'skills') AIC.d.skills = await hapi('skills');
    else if (tab === 'tools') AIC.d.tools = await hapi('tools/toolsets');
    else if (tab === 'mcp') AIC.d.mcp = (await hapi('mcp/servers')).servers || [];
    else if (tab === 'jobs') AIC.d.jobs = await hapi('cron/jobs');
    else if (tab === 'logs') AIC.d.logs = await hapi(`logs?file=${AIC.logFile}&lines=400&level=${AIC.logLevel}`);
    delete AIC.err[tab];
  } catch (e) { AIC.err[tab] = e.message; }
  aicBump();
}

// the logs follow along while they're on screen
function aicFollow() {
  clearInterval(AIC.logTimer);
  if (AI.conn && AIC.tab === 'logs') AIC.logTimer = setInterval(() => { if (AI.conn && AIC.tab === 'logs' && S.view === 'ai' && !document.hidden) aicLoad('logs'); else clearInterval(AIC.logTimer); }, 4000);
}

// a second tap within 4 s confirms (delete a memory, restart the gateway)
function aicArmed(key) {
  if (AIC.arm === key) { AIC.arm = null; return true; }
  AIC.arm = key;
  clearTimeout(AIC.armTimer);
  AIC.armTimer = setTimeout(() => { AIC.arm = null; aicBump(); }, 4000);
  aicBump();
  return false;
}

const aicAgo = (iso) => (iso ? ago(Date.parse(iso) / 1000) : '–');
const aicIn = (iso) => { if (!iso) return '–'; const s = Date.parse(iso) / 1000 - now(); return s <= 0 ? 'now' : 'in ' + fmtDur(s); };
const aicSw = (on, attrs, label) => `<button type="button" class="sw" role="switch" aria-checked="${!!on}" aria-label="${esc(label)}" ${attrs}></button>`;
const aicDash = (path, label) => `<a class="btn sm" href="/hermes${path}" target="_blank" rel="noopener">${label}${ic('arrow-up-right')}</a>`;
const aicHead = (icn, title, sub = '', extra = '') => `<div class="sec-h"><h2>${ic(icn)}${title}</h2>${sub ? `<span class="muted">${sub}</span>` : ''}${extra ? `<span class="spacer"></span>${extra}` : ''}</div>`;

function aicHTML() {
  const tabs = `<div class="aic-tabs" role="tablist">${AIC_TABS.map(([k, label, icn]) =>
    `<button type="button" role="tab" class="aic-t" data-aictab="${k}" aria-selected="${k === AIC.tab}">${ic(icn)}<span>${label}</span></button>`).join('')}</div>`;
  const err = AIC.err[AIC.tab];
  const ready = AIC.d[AIC.tab] !== undefined;
  const body = err ? `<div class="card aic-err">${ic('alert-triangle')}<span>Couldn't reach Hermes: ${esc(err)}</span><button type="button" class="btn sm" data-aicreload>Retry</button></div>`
    : ready ? AIC_VIEW[AIC.tab]() : '<div class="skel"></div><div class="skel"></div>';
  return tabs + `<div class="aic-body">${body}</div>`;
}

const AIC_VIEW = {
  overview() {
    const { st, model, soul } = AIC.d.overview;
    const m = st.gateway_platforms?.api_server?.metrics?.metrics_today || {};
    const stat = (label, v) => `<div class="aic-stat"><span>${label}</span><b>${v}</b></div>`;
    const caps = model.capabilities || {};
    return aicHead('gauge', 'JARVIS', esc(`Hermes ${st.version}`), `<button type="button" class="btn sm${AIC.arm === 'restart' ? ' danger' : ''}" data-aicrestart>${ic('arrow-rotate')}${AIC.arm === 'restart' ? 'Tap again to restart' : 'Restart'}</button>`) +
      `<div class="card aic-stats">${stat('Gateway', esc(st.gateway_state || '–'))}${stat('Sessions', st.active_sessions ?? '–')}${stat('Requests today', m.requests ?? 0)}` +
      `${stat('Tokens today', (m.tokens ?? 0).toLocaleString())}${stat('p95 reply', m.latency_p95_ms ? aiSecs(m.latency_p95_ms / 1000) : '–')}</div>` +
      aicHead('sparkle', 'Model', 'Hermes’ default, for every channel', aicDash('/models', 'Models')) +
      `<div class="card aic-model"><b>${esc(model.model)}</b><span class="tag">${esc(model.provider)}</span>` +
      `<span class="muted">${(model.effective_context_length || 0).toLocaleString()} tokens context · ${(caps.max_output_tokens || 0).toLocaleString()} out` +
      `${caps.supports_vision ? ' · vision' : ''}${caps.supports_reasoning ? ' · reasoning' : ''}</span></div>` +
      // the panel chat's own model, and starting it over (aisession.go)
      `<div class="card aic-pick"><label><span>Panel chat model</span><select data-aicmodel aria-label="Panel chat model">` +
      `<option value="">Default (${esc(model.model)})</option>${[...new Set([...(AI.models || []), AI.state?.pick].filter(Boolean))]
        .map((m) => `<option value="${esc(m)}"${m === AI.state?.pick ? ' selected' : ''}>${esc(m)}</option>`).join('')}</select></label>` +
      `<button type="button" class="btn sm" data-ainew>${ic('arrow-rotate')}New conversation</button>` +
      '<p class="faint">A new conversation starts JARVIS fresh in this chat, without what was said before (the chat above stays, and so do its memory and your task board). Use it when every reply fails. Typing /new or /model also works.</p></div>' +
      aicHead('people', 'Persona', 'Who it is and how it works (SOUL.md)') +
      `<details class="card aic-soul"><summary>${esc(soul.split('\n')[0] || 'No persona set')}<span class="faint"> · show all</span></summary><div class="md">${md(soul)}</div>` +
      '<p class="faint">Lives in projects/vm-agent/hermes/SOUL.md. Day-to-day rules belong in Memory: tell JARVIS “remember …”.</p></details>' +
      aiConnHTML();
  },

  memory() {
    const list = AIC.d.memory;
    const src = { profile: 'About you', memory: 'Its notes' };
    return aicHead('layers', 'Memory', 'What JARVIS keeps across every chat and channel, and reads before answering') +
      `<form class="card aic-rule" data-aicruleform><input id="aic-rule" placeholder="Teach it a rule, e.g. “Always reply in English”" autocomplete="off" aria-label="New rule">` +
      `<button type="submit" class="btn sm primary">${ic('plus')}Teach</button></form>` +
      (list.length ? list.map((n) => {
        const editing = AIC.memEdit === n.id;
        return `<div class="card aic-mem" data-k="${esc(n.id)}"><div class="aic-mem-h"><span class="tag">${esc(src[n.memorySource] || n.memorySource || 'memory')}</span>` +
          `<span class="faint">${n.timestamp ? ago(n.timestamp) : ''}</span><span class="spacer"></span>` +
          (editing ? '' : `<button type="button" class="abtn" data-aicmemedit="${esc(n.id)}" title="Edit" aria-label="Edit">${ic('pen')}</button>` +
            `<button type="button" class="abtn${AIC.arm === 'del:' + n.id ? ' armed' : ''}" data-aicmemdel="${esc(n.id)}" title="${AIC.arm === 'del:' + n.id ? 'Tap again to forget it' : 'Forget'}" aria-label="Forget">${ic('trash2')}</button>`) + '</div>' +
          (editing ? `<textarea class="aic-mem-in" rows="4" aria-label="Memory">${esc(n.content)}</textarea><div class="aic-mem-a"><button type="button" class="btn sm" data-aicmemcancel>Cancel</button><button type="button" class="btn sm primary" data-aicmemsave="${esc(n.id)}">Save</button></div>`
            : `<p>${esc(n.content)}</p>`) + '</div>';
      }).join('') : '<div class="empty">Nothing remembered yet. Tell JARVIS “remember …” in any chat, or teach it a rule above.</div>');
  },

  skills() {
    const all = AIC.d.skills || [];
    const on = all.filter((s) => String(s.enabled) !== 'false').length;
    const byCat = new Map();
    for (const s of all) { const c = s.category || 'other'; if (!byCat.has(c)) byCat.set(c, []); byCat.get(c).push(s); }
    const prov = { bundled: '', hub: 'hub', agent: 'learned', 'agent-created': 'learned', external: 'yours', local: 'yours' };
    return aicHead('wand-sparkle', 'Skills', `${on} of ${all.length} on · procedures it loads when a task needs them`, aicDash('/skills', 'Manage')) +
      `<label class="aic-q">${ic('search')}<input id="aic-skillq" type="search" placeholder="Search skills" value="${esc(AIC.skillQ)}" autocomplete="off" aria-label="Search skills"></label>` +
      [...byCat].sort(([a], [b]) => a.localeCompare(b)).map(([cat, list]) => `<div class="aic-grp" data-skillgrp><div class="aic-cat">${esc(cat.replace(/-/g, ' '))}</div><div class="card flush">` +
        list.map((s) => `<div class="aic-row" data-skillrow="${esc((s.name + ' ' + s.description + ' ' + cat).toLowerCase())}"><div class="aic-row-b"><b>${esc(s.name)}</b>` +
          `${prov[s.provenance] ? `<span class="tag">${prov[s.provenance]}</span>` : ''}${+s.usage ? `<span class="faint">used ${s.usage}×</span>` : ''}<span class="muted">${esc(s.description || '')}</span></div>` +
          aicSw(String(s.enabled) !== 'false', `data-aicskill="${esc(s.name)}"`, s.name) + '</div>').join('') + '</div></div>').join('');
  },

  tools() {
    const sets = AIC.d.tools || [];
    const servers = AIC.d.mcp || [];
    return aicHead('terminal-square', 'Tools', 'What JARVIS can do: built-in toolsets, plus every MCP server’s tools', aicDash('/config', 'Per channel')) +
      `<div class="card flush">${sets.map((t) => `<div class="aic-row${t.enabled ? '' : ' off'}"><div class="aic-row-b"><b>${esc(t.label || t.name)}</b>` +
        `${t.enabled ? '' : '<span class="tag">off</span>'}${t.available === false ? '<span class="tag warn">unavailable</span>' : ''}` +
        `<span class="aic-chips">${(t.tools || []).map((x) => `<code>${esc(x)}</code>`).join('')}</span></div></div>`).join('')}</div>` +
      (servers.length ? aicHead('puzzle', 'From MCP servers', 'Their tools appear as mcp__server__tool') + `<div class="card flush">${servers.map((s) => {
        const t = AIC.mcpTools[s.name];
        return `<div class="aic-row"><div class="aic-row-b"><b>${esc(s.name)}</b><span class="aic-chips">${t?.tools ? t.tools.map((x) => `<code title="${esc(x.description || '')}">${esc(x.name)}</code>`).join('') : t?.error ? `<span class="err">${esc(t.error)}</span>` : '<span class="faint">open MCP and tap Test to list them</span>'}</span></div></div>`;
      }).join('')}</div>` : '');
  },

  mcp() {
    const servers = AIC.d.mcp || [];
    return aicHead('puzzle', 'MCP servers', 'Services JARVIS can use: Notion, GitHub, databases…', aicDash('/mcp', 'Add from catalog')) +
      (servers.length ? servers.map((s) => {
        const t = AIC.mcpTools[s.name];
        const target = s.url || [s.command, ...(s.args || [])].filter(Boolean).join(' ');
        return `<div class="card aic-mcp"><div class="aic-mem-h"><b>${esc(s.name)}</b><span class="tag">${esc(s.transport || '')}</span>${s.auth ? `<span class="tag">${esc(s.auth)}</span>` : ''}<span class="spacer"></span>` +
          `<button type="button" class="btn sm" data-aicmcptest="${esc(s.name)}"${t?.testing ? ' disabled' : ''}>${t?.testing ? 'Testing…' : 'Test'}</button>${aicSw(String(s.enabled) !== 'false', `data-aicmcp="${esc(s.name)}"`, s.name)}</div>` +
          `<code class="aic-cmd">${esc(target)}</code>` +
          (t?.tools ? `<div class="aic-ok">${ic('check-circle-f')}Connected · ${plural(t.tools.length, 'tool')}</div><div class="aic-chips">${t.tools.map((x) => `<code title="${esc(x.description || '')}">${esc(x.name)}</code>`).join('')}</div>`
            : t?.error ? `<div class="err">${esc(t.error)}</div>` : '') + '</div>';
      }).join('') : '<div class="empty">No MCP servers yet.</div>');
  },

  jobs() {
    const jobs = AIC.d.jobs || [];
    return aicHead('clock-circle', 'Scheduled jobs', 'What JARVIS does on its own. Ask it: “every weekday at 8, brief me”') +
      (jobs.length ? jobs.map((j) => {
        const paused = j.state === 'paused' || j.enabled === false;
        const bad = /fail|error/.test(j.last_status || '');
        const runs = AIC.runs[j.id];
        return `<div class="card aic-job"><div class="aic-mem-h"><b>${esc(j.name || j.id)}</b><span class="tag">${esc(j.schedule_display || j.schedule?.display || '')}</span>` +
          `<span class="tag${paused ? ' warn' : ''}">${paused ? 'paused' : esc(j.state || 'scheduled')}</span><span class="spacer"></span>` +
          `<button type="button" class="btn sm" data-aicjob="trigger" data-id="${esc(j.id)}">${ic('play3')}Run</button>` +
          `<button type="button" class="btn sm" data-aicjob="${paused ? 'resume' : 'pause'}" data-id="${esc(j.id)}">${ic(paused ? 'play-circle3' : 'pause3')}${paused ? 'Resume' : 'Pause'}</button></div>` +
          `<p class="muted">${esc(j.prompt ? trunc(j.prompt, 220) : j.script ? 'Script: ' + j.script + (j.no_agent ? ' (no AI)' : '') : '')}</p>` +
          `<div class="aic-job-m"><span>Last: <b class="${bad ? 'err' : ''}">${esc(j.last_status || '–')}</b> ${aicAgo(j.last_run_at)}</span><span>Next: ${paused ? '–' : aicIn(j.next_run_at)}</span><span>Runs: ${j.repeat?.completed ?? 0}</span>` +
          `${j.model ? `<span>Model: ${esc(j.model)}</span>` : ''}</div>` +
          `<details class="aic-runs" data-aicruns="${esc(j.id)}"${runs ? ' open' : ''}><summary>Recent runs</summary>${runs ? (runs.length ? runs.slice(0, 8).map((r) => `<div class="aic-run">${esc(r.title || r.preview || r.id)}</div>`).join('') : '<div class="faint">None yet</div>') : shim('Loading…')}</details></div>`;
      }).join('') : '<div class="empty">No scheduled jobs.</div>');
  },

  logs() {
    const noise = /aiohttp\.access|tui_gateway\.ws|memory trim/;
    const lines = (AIC.d.logs?.lines || []).filter((l) => AIC.logNoise || !noise.test(l)).slice(-250);
    const seg = (attr, cur, opts) => `<div class="seg">${opts.map(([v, l]) => `<button type="button" ${attr}="${v}" aria-pressed="${v === cur}">${l}</button>`).join('')}</div>`;
    return aicHead('document-code', 'Logs', 'Live, newest at the bottom', aicDash('/logs', 'Full')) +
      `<div class="aic-logbar">${seg('data-aiclogfile', AIC.logFile, [['agent', 'Agent'], ['errors', 'Errors'], ['gateway', 'Gateway']])}` +
      `${seg('data-aicloglevel', AIC.logLevel, [['ALL', 'All'], ['WARNING', 'Warnings'], ['ERROR', 'Errors']])}` +
      `<button type="button" class="chip" data-aiclognoise aria-pressed="${AIC.logNoise}">Show noise</button></div>` +
      `<pre class="card aic-log" data-keep-scroll="aiclog">${lines.length ? lines.map((l) => `<span class="${/ ERROR | CRITICAL /.test(l) ? 'err' : / WARNING /.test(l) ? 'warn' : ''}">${esc(l.replace(/\n$/, ''))}</span>`).join('\n') : 'Nothing here.'}</pre>`;
  },
};

// after a redraw: put back what a redraw resets (the skill filter, the log's scroll at the bottom)
function aicAfter(root) {
  aicFilterSkills(root);
  const log = $('.aic-log', root);
  if (log && !log.dataset.seen) { log.scrollTop = log.scrollHeight; log.dataset.seen = '1'; }
}

function aicFilterSkills(root = document) {
  const q = AIC.skillQ.trim().toLowerCase();
  for (const r of $$('[data-skillrow]', root)) r.hidden = !!q && !r.dataset.skillrow.includes(q);
  for (const g of $$('[data-skillgrp]', root)) g.hidden = !$$('[data-skillrow]', g).some((r) => !r.hidden);
}

function aicTab(tab) {
  AIC.tab = tab;
  store.set('aicTab', tab);
  AIC.memEdit = null;
  if (tab === 'tools' && !AIC.d.mcp) aicLoad('mcp');
  aicLoad(tab);
  aicFollow();
  aicBump();
}

async function aicDo(fn, reload) {
  try { await fn(); } catch (e) { toast(e.message, true); }
  if (reload) aicLoad(reload);
}

// clicks in the control room; true when handled
function aicClick(e) {
  const t = e.target.closest('[data-aictab],[data-aicreload],[data-aicrestart],[data-aicskill],[data-aicmcp],[data-aicmcptest],[data-aicjob],[data-aicmemedit],[data-aicmemdel],[data-aicmemsave],[data-aicmemcancel],[data-aiclogfile],[data-aicloglevel],[data-aiclognoise]');
  if (!t) return false;
  const d = t.dataset;
  if (d.aictab) aicTab(d.aictab);
  else if ('aicreload' in d) aicLoad();
  else if ('aicrestart' in d) { // the panel stops the gateway; run-agent.sh's loop starts it again with the new settings
    if (aicArmed('restart')) aicDo(async () => { const r = await api('/api/ai/restart', {}); if (r.status !== 200) throw new Error(r.error || 'restart failed'); toast('Restarting JARVIS… back in a few seconds'); }, 'overview');
  }
  else if (d.aicskill) {
    const s = AIC.d.skills.find((x) => x.name === d.aicskill);
    const enabled = String(s.enabled) === 'false';
    s.enabled = enabled;
    aicBump();
    aicDo(() => hapi('skills/toggle', 'PUT', { name: s.name, enabled }), 'skills');
  } else if (d.aicmcp) {
    const s = AIC.d.mcp.find((x) => x.name === d.aicmcp);
    const enabled = String(s.enabled) === 'false';
    s.enabled = enabled;
    aicBump();
    aicDo(() => hapi(`mcp/servers/${encodeURIComponent(s.name)}/enabled`, 'PUT', { enabled }), 'mcp');
  } else if (d.aicmcptest) {
    const name = d.aicmcptest;
    AIC.mcpTools[name] = { testing: true };
    aicBump();
    hapi(`mcp/servers/${encodeURIComponent(name)}/test`, 'POST', {})
      .then((r) => { AIC.mcpTools[name] = r.ok === false ? { error: r.error || 'failed' } : { tools: r.tools || [] }; })
      .catch((err) => { AIC.mcpTools[name] = { error: err.message }; })
      .finally(aicBump);
  } else if (d.aicjob) aicDo(() => hapi(`cron/jobs/${encodeURIComponent(d.id)}/${d.aicjob}`, 'POST', {}).then(() => toast(d.aicjob === 'trigger' ? 'Running it now' : d.aicjob === 'pause' ? 'Paused' : 'Resumed')), 'jobs');
  else if (d.aicmemedit) { AIC.memEdit = d.aicmemedit; aicBump(); $('.aic-mem-in')?.focus(); }
  else if ('aicmemcancel' in d) { AIC.memEdit = null; aicBump(); }
  else if (d.aicmemsave) {
    const content = $('.aic-mem-in').value.trim();
    AIC.memEdit = null;
    aicDo(() => hapi('learning/node', 'PUT', { id: d.aicmemsave, content }), 'memory');
  } else if (d.aicmemdel) { if (aicArmed('del:' + d.aicmemdel)) aicDo(() => hapi('learning/node', 'DELETE', { id: d.aicmemdel }).then(() => toast('Forgotten')), 'memory'); }
  else if (d.aiclogfile) { AIC.logFile = d.aiclogfile; aicLoad('logs'); }
  else if (d.aicloglevel) { AIC.logLevel = d.aicloglevel; aicLoad('logs'); }
  else if ('aiclognoise' in d) { AIC.logNoise = !AIC.logNoise; aicBump(); }
  return true;
}

document.addEventListener('input', (e) => { if (e.target.id === 'aic-skillq') { AIC.skillQ = e.target.value; aicFilterSkills(); } });
document.addEventListener('submit', (e) => { // Teach: a standing rule goes to JARVIS in the chat, which saves it to memory
  if (!e.target.matches('[data-aicruleform]')) return;
  e.preventDefault();
  const rule = $('#aic-rule').value.trim();
  if (!rule) return;
  aiSend(`Remember this as a standing rule (save it to memory): ${rule}`);
});
document.addEventListener('toggle', (e) => { // a job's recent runs load when opened
  const id = e.target.dataset?.aicruns;
  if (id && e.target.open && !AIC.runs[id]) hapi(`cron/jobs/${encodeURIComponent(id)}/runs`).then((r) => { AIC.runs[id] = r.runs || []; aicBump(); }).catch(() => {});
}, true);
