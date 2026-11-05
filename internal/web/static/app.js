'use strict';

/* ------------------------------------------------------------------ *
 * CodeDeep web client
 * ------------------------------------------------------------------ */

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

const state = {
  boot: null,
  sessionId: null,
  running: false,
  abort: null,
  turn: null,
  assistant: null,
  assistantFresh: false,
  subagents: [],
  toolCards: new Map(),
  approvals: new Map(),
  callSeq: 0,
  keyPresent: true,
  lastSessions: [],
  usage: null,
};

const chat = $('#chat');
const input = $('#input');
const messagesEl = $('#messages');
const emptyState = $('#emptyState');

/* ------------------------------------------------------------------ *
 * boot
 * ------------------------------------------------------------------ */

init();

async function init() {
  applyTheme(localStorage.getItem('codedeep-theme') || 'dark');
  if (localStorage.getItem('codedeep-collapsed') === '1' && window.innerWidth > 900) {
    $('#app').classList.add('collapsed');
  }
  bindUI();
  try {
    const boot = await api('/api/bootstrap');
    state.boot = boot;
    state.keyPresent = !!boot.api_key_present;
    $('#modelChip').textContent = boot.model;
    $('#approvalChip').textContent = boot.approval + ' mode';
    $('#workspaceLabel').textContent = compactPath(boot.workspace);
    $('#workspaceLabel').title = boot.workspace;
    $('#versionLabel').textContent = 'v' + boot.version;
    if (!boot.api_key_present) {
      $('#keyBanner').hidden = false;
      $('#sendBtn').disabled = true;
    }
    renderSessions(boot.sessions || []);
  } catch (err) {
    toast('Could not reach the local server: ' + err.message, 6000);
    setStatus('error', 'offline');
  }
  resizeInput();
  input.focus();
}

function bindUI() {
  $('#sendBtn').addEventListener('click', send);
  $('#stopBtn').addEventListener('click', stopRun);
  $('#newChatBtn').addEventListener('click', newChat);
  $('#refreshBtn').addEventListener('click', refreshSessions);
  $('#themeBtn').addEventListener('click', toggleTheme);
  $('#helpBtn').addEventListener('click', () =>
    toast('Enter to send · Shift+Enter for a newline · Ctrl/⌘+N for a new chat · Ctrl/⌘+K to focus · Esc to stop', 6000)
  );
  $('#collapseBtn').addEventListener('click', () => {
    $('#app').classList.toggle('collapsed');
    localStorage.setItem('codedeep-collapsed', $('#app').classList.contains('collapsed') ? '1' : '0');
  });
  $('#menuBtn').addEventListener('click', () => $('#app').classList.add('sidebar-open'));
  $('#sidebarBackdrop').addEventListener('click', closeSidebar);

  input.addEventListener('input', resizeInput);
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      send();
    }
  });

  document.addEventListener('keydown', (e) => {
    const mod = e.ctrlKey || e.metaKey;
    if (mod && e.key.toLowerCase() === 'n') {
      e.preventDefault();
      newChat();
    } else if (mod && e.key.toLowerCase() === 'k') {
      e.preventDefault();
      input.focus();
    } else if (e.key === 'Escape') {
      if (state.running) {
        e.preventDefault();
        stopRun();
      } else if (document.activeElement === input) {
        input.blur();
      }
    }
  });

  for (const btn of $$('.suggestion')) {
    btn.addEventListener('click', () => {
      input.value = btn.dataset.prompt || btn.textContent.trim();
      resizeInput();
      input.focus();
    });
  }
}

/* ------------------------------------------------------------------ *
 * sessions
 * ------------------------------------------------------------------ */

function renderSessions(metas) {
  state.lastSessions = metas || [];
  const list = $('#sessionList');
  list.innerHTML = '';
  if (!state.lastSessions.length) {
    list.append(el('div', 'session-empty', 'No sessions yet. Your conversations are saved here.'));
    return;
  }
  for (const meta of state.lastSessions) {
    const item = el('div', 'session-item' + (meta.id === state.sessionId ? ' active' : ''));
    const open = el('button', 'session-open');
    open.type = 'button';
    const title = el('div', 'session-title', meta.title || 'Untitled session');
    const line = el('div', 'session-meta');
    line.append(
      el('span', '', `${meta.message_count || 0} msgs`),
      el('span', '', '·'),
      el('span', '', fmtTokens(meta.total_tokens || 0)),
      el('span', 'session-time', relTime(meta.updated))
    );
    open.append(title, line);
    open.addEventListener('click', () => loadSession(meta.id));

    const del = el('button', 'session-del', '×');
    del.type = 'button';
    del.title = 'Delete session';
    del.setAttribute('aria-label', 'Delete session');
    del.addEventListener('click', (e) => {
      e.stopPropagation();
      deleteSession(meta.id);
    });

    item.append(open, del);
    list.append(item);
  }
}

async function refreshSessions() {
  try {
    const metas = await api('/api/sessions');
    renderSessions(metas);
    const mine = metas.find((m) => m.id === state.sessionId);
    if (mine && mine.title) setTitle(mine.title);
  } catch {
    /* non-fatal */
  }
}

async function loadSession(id) {
  if (state.running) stopRun();
  try {
    const sess = await api('/api/sessions/' + encodeURIComponent(id));
    state.sessionId = sess.id;
    clearChat();
    renderHistory(sess);
    setTitle(sess.title || 'Session');
    setUsage(sess.usage || null);
    if (sess.model) $('#modelChip').textContent = sess.model;
    await refreshSessions();
    closeSidebar();
    input.focus();
  } catch (err) {
    toast('Could not load session: ' + err.message);
  }
}

async function deleteSession(id) {
  if (!window.confirm('Delete this session? This cannot be undone.')) return;
  try {
    await api('/api/sessions/' + encodeURIComponent(id), { method: 'DELETE' });
    if (state.sessionId === id) {
      state.sessionId = null;
      clearChat();
      showEmpty();
      setTitle('New session');
      setUsage(null);
    }
    await refreshSessions();
  } catch (err) {
    toast('Could not delete session: ' + err.message);
  }
}

function newChat() {
  if (state.running) stopRun();
  state.sessionId = null;
  clearChat();
  showEmpty();
  setTitle('New session');
  setUsage(null);
  renderSessions(state.lastSessions);
  closeSidebar();
  input.focus();
}

/* ------------------------------------------------------------------ *
 * sending and streaming
 * ------------------------------------------------------------------ */

async function send() {
  if (!state.keyPresent) {
    toast('Set DEEPSEEK_API_KEY and restart the server to start chatting.', 5000);
    return;
  }
  const text = input.value.trim();
  if (!text || state.running) return;

  hideEmpty();
  const turn = newTurn();
  state.turn = turn;
  addUserMessage(text, turn);
  state.assistant = createAssistant();
  state.assistantFresh = true;
  state.toolCards.clear();
  state.approvals.clear();
  state.subagents = [];

  input.value = '';
  resizeInput();
  state.abort = new AbortController();
  setRunning(true, 'thinking');

  try {
    const res = await fetch('/api/chat', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ session_id: state.sessionId || '', prompt: text }),
      signal: state.abort.signal,
    });
    if (!res.ok) {
      let message = 'HTTP ' + res.status;
      try {
        const body = await res.json();
        if (body.error) message = body.error;
      } catch {
        /* ignore */
      }
      throw new Error(message);
    }
    if (!res.body) throw new Error('This browser does not support streaming responses.');
    await readStream(res.body);
  } catch (err) {
    if (err.name === 'AbortError') {
      addNote('Stopped by user', 'warn', turn);
    } else {
      addErrorCard(err.message, turn);
      setStatus('error', 'error');
    }
  } finally {
    setRunning(false);
    finalizeAssistant();
    state.abort = null;
    await refreshSessions();
    input.focus();
  }
}

function stopRun() {
  if (state.abort) state.abort.abort();
}

async function readStream(body) {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    let index;
    while ((index = buffer.indexOf('\n')) !== -1) {
      const line = buffer.slice(0, index).trim();
      buffer = buffer.slice(index + 1);
      if (line) handleLine(line);
    }
  }
  const tail = buffer.trim();
  if (tail) handleLine(tail);
}

function handleLine(line) {
  let ev;
  try {
    ev = JSON.parse(line);
  } catch {
    return;
  }
  handleEvent(ev);
}

function handleEvent(ev) {
  switch (ev.type) {
    case 'session':
      if (ev.session_id) {
        state.sessionId = ev.session_id;
        if (ev.message) setTitle(ev.message);
      }
      break;
    case 'user':
      break;
    case 'assistant_start':
      if (state.assistantFresh) {
        state.assistantFresh = false;
      } else {
        state.assistant = createAssistant();
      }
      setStatus('busy', 'thinking');
      break;
    case 'reasoning':
      appendReasoning(ev.text || '', ev.depth || 0);
      break;
    case 'text':
      appendAssistantText(ev.text || '', ev.depth || 0);
      break;
    case 'tool_start':
      onToolStart(ev);
      break;
    case 'tool_end':
      onToolEnd(ev);
      break;
    case 'approval_request':
      onApprovalRequest(ev);
      break;
    case 'approval_result':
      onApprovalResult(ev);
      break;
    case 'subagent_start':
      onSubagentStart(ev);
      break;
    case 'subagent_end':
      onSubagentEnd(ev);
      break;
    case 'usage':
      setUsage(ev.total_usage || ev.usage || null);
      break;
    case 'compact':
      addNote(ev.message || 'Context compacted');
      break;
    case 'error':
      if (ev.message && ev.message !== 'context canceled') addErrorCard(ev.message);
      setStatus('error', 'error');
      break;
    case 'done':
      if (ev.message === 'stopped') addNote('Stopped by user', 'warn');
      break;
    default:
      break;
  }
}

/* ------------------------------------------------------------------ *
 * assistant rendering
 * ------------------------------------------------------------------ */

function appendAssistantText(delta, depth) {
  if (depth > 0) {
    appendSubagentText(delta);
    return;
  }
  if (!state.assistant) state.assistant = createAssistant();
  const a = state.assistant;
  a.text += delta;
  if (a.typing) {
    a.typing.remove();
    a.typing = null;
  }
  if (a.thinking && a.thinking.open) a.thinking.open = false;
  scheduleRender(a);
}

function appendReasoning(delta, depth) {
  if (depth > 0) {
    appendSubagentText(delta);
    return;
  }
  if (!state.assistant) state.assistant = createAssistant();
  const a = state.assistant;
  a.reasoning += delta;
  a.thinking.hidden = false;
  if (!a.thinking.dataset.opened) {
    a.thinking.open = true;
    a.thinking.dataset.opened = '1';
  }
  a.tpre.textContent = a.reasoning;
  a.tpre.scrollTop = a.tpre.scrollHeight;
  scrollSoon();
}

function scheduleRender(a) {
  if (a.frame) return;
  // Timers still fire (throttled) in background tabs; requestAnimationFrame
  // does not, which would leave streamed text invisible until the tab returns.
  a.frame = setTimeout(() => {
    a.frame = null;
    a.md.innerHTML = renderMarkdown(a.text);
    scrollSoon();
  }, 80);
}

function buildAssistant(turn) {
  const msg = el('div', 'msg assistant');
  const avatar = el('div', 'avatar', '◈');
  const body = el('div', 'assistant-body');

  const thinking = document.createElement('details');
  thinking.className = 'thinking';
  thinking.hidden = true;
  const tsummary = document.createElement('summary');
  tsummary.textContent = 'Thinking';
  const tpre = document.createElement('pre');
  thinking.append(tsummary, tpre);

  const md = el('div', 'md');
  const typing = el('div', 'typing');
  typing.append(el('span'), el('span'), el('span'));

  body.append(thinking, md, typing);
  msg.append(avatar, body);
  turn.append(msg);

  return { el: msg, body, thinking, tpre, md, typing, text: '', reasoning: '', frame: null };
}

function createAssistant() {
  const a = buildAssistant(state.turn || newTurn());
  state.assistant = a;
  scrollSoon();
  return a;
}

function finalizeAssistant() {
  if (!state.assistant) return;
  const a = state.assistant;
  if (a.frame) {
    clearTimeout(a.frame);
    a.frame = null;
  }
  if (a.text) a.md.innerHTML = renderMarkdown(a.text);
  if (a.typing) {
    a.typing.remove();
    a.typing = null;
  }
  if (!a.text && !a.reasoning) {
    a.el.remove();
    state.assistant = null;
  }
}

/* ------------------------------------------------------------------ *
 * tools
 * ------------------------------------------------------------------ */

function onToolStart(ev) {
  if (state.assistant && state.assistant.typing) {
    state.assistant.typing.remove();
    state.assistant.typing = null;
  }
  const depth = ev.depth || 0;
  const nested = depth > 0;
  const parent = nested ? subagentHost() : (state.turn || newTurn());
  const card = buildToolCard(ev, nested);
  parent.append(card);
  const key = ev.call_id || 'call-' + ++state.callSeq;
  state.toolCards.set(key, card);
  setStatus('busy', 'running ' + (ev.name || 'tool'));
  scrollSoon();
}

function buildToolCard(ev, nested) {
  const card = el('div', 'tool-card' + (nested ? ' nested' : ''));
  const head = el('div', 'tool-head');
  const icon = el('span', 'tool-icon', toolGlyph(ev.name));
  const name = el('span', 'tool-name', ev.name || 'tool');
  const args = el('span', 'tool-args', summarizeArgs(ev.name, ev.args));
  args.title = prettyArgs(ev.args).slice(0, 2000);
  const status = el('span', 'tool-status');
  status.append(el('span', 'spinner'), el('span', '', 'running'));
  head.append(icon, name, args, status);

  const body = document.createElement('details');
  body.className = 'tool-body';
  body.hidden = true;
  const summary = document.createElement('summary');
  summary.textContent = 'Output';
  const pre = el('pre');
  body.append(summary, pre);

  card.append(head, body);
  card._status = status;
  card._body = body;
  card._pre = pre;
  card._name = ev.name;
  card._done = false;
  return card;
}

function onToolEnd(ev) {
  const card = findToolCard(ev);
  if (!card) {
    const parent = (ev.depth || 0) > 0 ? subagentHost() : (state.turn || newTurn());
    const created = buildToolCard(ev, (ev.depth || 0) > 0);
    parent.append(created);
    setToolResultOn(created, ev.output || '', !!ev.is_error);
    scrollSoon();
    return;
  }
  setToolResultOn(card, ev.output || '', !!ev.is_error);
  setStatus('busy', 'working');
  scrollSoon();
}

function findToolCard(ev) {
  if (ev.call_id && state.toolCards.has(ev.call_id)) return state.toolCards.get(ev.call_id);
  let found = null;
  for (const card of state.toolCards.values()) {
    if (!card._done && card._name === ev.name) found = card;
  }
  return found;
}

function setToolResultOn(card, output, isError) {
  card._done = true;
  card.classList.toggle('error', !!isError);
  card._status.innerHTML = '';
  card._status.append(el('span', isError ? 'err' : 'ok', isError ? 'failed' : 'done'));
  card._pre.textContent = output || '(no output)';
  card._body.hidden = false;
  if (isError) card._body.open = true;
  card._pre.scrollTop = card._pre.scrollHeight;
}

/* ------------------------------------------------------------------ *
 * approvals
 * ------------------------------------------------------------------ */

function onApprovalRequest(ev) {
  if (state.assistant && state.assistant.typing) {
    state.assistant.typing.remove();
    state.assistant.typing = null;
  }
  const card = el('div', 'approval-card');
  card.dataset.approval = ev.approval_id || '';

  const head = el('div', 'approval-head');
  head.append(
    el('span', 'badge warn', 'Approval required'),
    el('span', 'approval-tool', ev.name || 'tool'),
    el('span', 'approval-note', 'wants to run in your workspace')
  );

  const pre = el('pre', 'approval-args', prettyArgs(ev.args) || '(no arguments)');
  const actions = el('div', 'approval-actions');
  const result = el('div', 'approval-result');

  const allow = el('button', 'btn primary', 'Allow once');
  allow.type = 'button';
  allow.addEventListener('click', () => respondApproval(ev.approval_id, 'allow'));

  const always = el('button', 'btn', 'Always allow ' + (ev.name || 'tool'));
  always.type = 'button';
  always.addEventListener('click', () => respondApproval(ev.approval_id, 'always'));

  const deny = el('button', 'btn danger', 'Deny');
  deny.type = 'button';
  deny.addEventListener('click', () => respondApproval(ev.approval_id, 'deny'));

  actions.append(allow, always, deny);
  card.append(head, pre, actions, result);

  const parent = (ev.depth || 0) > 0 && state.subagents.length ? subagentHost() : (state.turn || newTurn());
  parent.append(card);
  state.approvals.set(ev.approval_id || '', { card, actions, result, name: ev.name, answered: false });
  setStatus('busy', 'waiting for approval');
  scrollSoon();
}

async function respondApproval(id, decision) {
  const entry = state.approvals.get(id || '');
  if (!entry || entry.answered) return;
  entry.answered = true;
  $$('button', entry.actions).forEach((b) => (b.disabled = true));
  entry.result.textContent = decision === 'deny' ? 'Denying…' : 'Allowing…';
  try {
    const res = await fetch('/api/approve', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id, decision }),
    });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    entry.result.textContent =
      decision === 'deny' ? 'Denied — the agent will change approach' : 'Allowed — continuing';
    entry.result.className = 'approval-result ' + (decision === 'deny' ? 'denied' : 'ok');
  } catch (err) {
    entry.answered = false;
    $$('button', entry.actions).forEach((b) => (b.disabled = false));
    entry.result.textContent = 'Could not send decision: ' + err.message;
    entry.result.className = 'approval-result denied';
  }
}

function onApprovalResult(ev) {
  const entry = state.approvals.get(ev.approval_id || '');
  if (!entry) return;
  entry.actions.remove();
  const message = ev.message || '';
  const denied = message === 'deny' || ev.is_error;
  entry.result.className = 'approval-result ' + (denied ? 'denied' : 'ok');
  if (message === 'allow') entry.result.textContent = 'Allowed';
  else if (message === 'always') entry.result.textContent = `Allowed — always for ${entry.name || 'this tool'}`;
  else if (message === 'deny') entry.result.textContent = 'Denied';
  else entry.result.textContent = message || (denied ? 'Denied' : 'Allowed');
  setStatus('busy', 'working');
}

/* ------------------------------------------------------------------ *
 * subagents
 * ------------------------------------------------------------------ */

function onSubagentStart(ev) {
  const depth = ev.depth || 1;
  const nested = state.subagents.length > 0;
  const parent = nested ? state.subagents[state.subagents.length - 1].log : (state.turn || newTurn());
  const card = el('div', 'subagent-card' + (nested ? ' nested' : ''));

  const head = el('div', 'subagent-head');
  const spark = el('span', 'spark', '✦');
  const name = el('span', 'subagent-name', ev.name || 'Subagent');
  const status = el('span', 'subagent-status');
  status.append(el('span', 'spinner'), el('span', '', 'working'));
  head.append(spark, name, status);

  const prompt = el('div', 'subagent-prompt', ev.message || '');
  prompt.title = ev.message || '';
  const log = el('div', 'subagent-log');
  const report = el('div', 'subagent-report');
  report.hidden = true;

  card.append(head, prompt, log, report);
  parent.append(card);

  state.subagents.push({ card, log, status, report, name: ev.name, depth, textEl: null, text: '' });
  setStatus('busy', 'subagent working');
  scrollSoon();
}

function appendSubagentText(delta) {
  const current = state.subagents[state.subagents.length - 1];
  if (!current) {
    appendAssistantText(delta, 0);
    return;
  }
  current.text += delta;
  if (!current.textEl) {
    current.textEl = el('div', 'subagent-text');
    current.log.append(current.textEl);
  }
  current.textEl.textContent = current.text;
  current.textEl.scrollTop = current.textEl.scrollHeight;
  scrollSoon();
}

function onSubagentEnd(ev) {
  let index = -1;
  for (let i = state.subagents.length - 1; i >= 0; i--) {
    if (state.subagents[i].depth === ev.depth) {
      index = i;
      break;
    }
  }
  const entry = index >= 0 ? state.subagents.splice(index, 1)[0] : state.subagents.pop();
  if (!entry) return;
  entry.status.innerHTML = '';
  entry.status.append(el('span', ev.is_error ? 'err' : 'ok', ev.is_error ? 'failed' : 'done'));
  if (ev.message) {
    entry.report.hidden = false;
    entry.report.textContent = ev.message;
  }
  scrollSoon();
}

function subagentHost() {
  if (state.subagents.length) return state.subagents[state.subagents.length - 1].log;
  return state.turn || newTurn();
}

/* ------------------------------------------------------------------ *
 * history
 * ------------------------------------------------------------------ */

function renderHistory(sess) {
  clearChat();
  const msgs = sess.messages || [];
  if (!msgs.length) {
    showEmpty();
    return;
  }
  hideEmpty();

  let turn = null;
  const toolCards = new Map();

  for (const m of msgs) {
    if (!m) continue;
    if (m.role === 'user') {
      turn = newTurn();
      if ((m.content || '').startsWith('[Conversation summary')) {
        addSummaryCard(m.content || '', turn);
      } else {
        addUserMessage(m.content || '', turn);
      }
      continue;
    }
    if (!turn) turn = newTurn();

    if (m.role === 'assistant') {
      if (m.content || m.reasoning_content) {
        const a = buildAssistant(turn);
        a.text = m.content || '';
        a.reasoning = m.reasoning_content || '';
        if (a.reasoning) {
          a.thinking.hidden = false;
          a.tpre.textContent = a.reasoning;
        }
        a.md.innerHTML = renderMarkdown(a.text);
        if (a.typing) {
          a.typing.remove();
          a.typing = null;
        }
      }
      for (const call of m.tool_calls || []) {
        const name = call.function ? call.function.name : 'tool';
        const args = parseMaybe(call.function ? call.function.arguments : '');
        const card = buildToolCard({ name, args, call_id: call.id }, false);
        turn.append(card);
        const key = call.id || 'hist-' + ++state.callSeq;
        state.toolCards.set(key, card);
        toolCards.set(call.id, card);
      }
      continue;
    }

    if (m.role === 'tool') {
      const card = toolCards.get(m.tool_call_id);
      if (card) {
        const output = m.content || '';
        setToolResultOn(card, output, /^(Error:|error:)/.test(output));
      }
    }
  }
  scrollToBottom(true);
}

function addUserMessage(text, turn) {
  const msg = el('div', 'msg user');
  msg.append(el('div', 'bubble', text));
  (turn || state.turn || newTurn()).append(msg);
  scrollSoon();
}

function addSummaryCard(text, turn) {
  const details = document.createElement('details');
  details.className = 'summary-card';
  const summary = document.createElement('summary');
  summary.textContent = 'Earlier conversation summary (context compacted)';
  const body = el('pre', '', text);
  details.append(summary, body);
  (turn || state.turn || newTurn()).append(details);
}

function addNote(text, kind, turn) {
  const node = el('div', 'note-card' + (kind ? ' ' + kind : ''), text);
  (turn || state.turn || newTurn()).append(node);
  scrollSoon();
}

function addErrorCard(text, turn) {
  const node = el('div', 'error-card', text);
  (turn || state.turn || newTurn()).append(node);
  scrollSoon();
}

/* ------------------------------------------------------------------ *
 * markdown (safe, no raw HTML)
 * ------------------------------------------------------------------ */

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

function inlineMarkdown(text) {
  const parts = String(text).split('`');
  let out = '';
  for (let i = 0; i < parts.length; i++) {
    if (i % 2 === 1) {
      out += '<code>' + escapeHtml(parts[i]) + '</code>';
      continue;
    }
    let t = escapeHtml(parts[i]);
    t = t.replace(/\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)/g,
      '<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>');
    t = t.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
    t = t.replace(/(^|[\s(])\*([^*\n]+)\*/g, '$1<em>$2</em>');
    out += t;
  }
  return out;
}

function renderBlocks(text) {
  const lines = String(text).split('\n');
  let html = '';
  let list = null;
  let para = [];

  const flushPara = () => {
    if (!para.length) return;
    html += '<p>' + inlineMarkdown(para.join('\n')).replace(/\n/g, '<br>') + '</p>';
    para = [];
  };
  const closeList = () => {
    if (list) {
      html += `</${list}>`;
      list = null;
    }
  };

  for (const line of lines) {
    if (/^\s*$/.test(line)) {
      flushPara();
      closeList();
      continue;
    }
    let m = line.match(/^(#{1,4})\s+(.*)$/);
    if (m) {
      flushPara();
      closeList();
      const level = Math.min(5, m[1].length + 1);
      html += `<h${level}>${inlineMarkdown(m[2])}</h${level}>`;
      continue;
    }
    m = line.match(/^\s*[-*]\s+(.*)$/);
    if (m) {
      flushPara();
      if (list !== 'ul') {
        closeList();
        list = 'ul';
        html += '<ul>';
      }
      html += '<li>' + inlineMarkdown(m[1]) + '</li>';
      continue;
    }
    m = line.match(/^\s*\d+\.\s+(.*)$/);
    if (m) {
      flushPara();
      if (list !== 'ol') {
        closeList();
        list = 'ol';
        html += '<ol>';
      }
      html += '<li>' + inlineMarkdown(m[1]) + '</li>';
      continue;
    }
    m = line.match(/^>\s?(.*)$/);
    if (m) {
      flushPara();
      closeList();
      html += '<blockquote>' + inlineMarkdown(m[1]) + '</blockquote>';
      continue;
    }
    closeList();
    para.push(line);
  }
  flushPara();
  closeList();
  return html;
}

function renderMarkdown(text) {
  const chunks = String(text || '').split('```');
  let html = '';
  for (let i = 0; i < chunks.length; i++) {
    if (i % 2 === 1) {
      let raw = chunks[i];
      if (raw.startsWith('\n')) raw = raw.slice(1);
      const nl = raw.indexOf('\n');
      if (nl >= 0) {
        const first = raw.slice(0, nl).trim();
        if (first && /^[A-Za-z0-9_+.#-]{1,20}$/.test(first)) raw = raw.slice(nl + 1);
      }
      html += '<pre class="code"><code>' + escapeHtml(raw.replace(/\n$/, '')) + '</code></pre>';
    } else {
      html += renderBlocks(chunks[i]);
    }
  }
  return html;
}

/* ------------------------------------------------------------------ *
 * UI helpers
 * ------------------------------------------------------------------ */

function newTurn() {
  const turn = el('div', 'turn');
  messagesEl.append(turn);
  state.turn = turn;
  hideEmpty();
  return turn;
}

function clearChat() {
  messagesEl.innerHTML = '';
  state.turn = null;
  state.assistant = null;
  state.assistantFresh = false;
  state.toolCards.clear();
  state.approvals.clear();
  state.subagents = [];
}

function showEmpty() {
  emptyState.hidden = false;
}
function hideEmpty() {
  emptyState.hidden = true;
}

function setTitle(text) {
  const title = (text || 'New session').trim();
  $('#chatTitle').textContent = title;
  document.title = title === 'New session' ? 'CodeDeep' : title + ' — CodeDeep';
}

function setUsage(usage) {
  state.usage = usage;
  const chip = $('#usageChip');
  if (!usage || !usage.total_tokens) {
    chip.hidden = true;
    return;
  }
  chip.hidden = false;
  chip.textContent = `↑ ${fmtShort(usage.prompt_tokens)} · ↓ ${fmtShort(usage.completion_tokens)} · ${fmtShort(usage.total_tokens)} tok`;
  chip.title = `${usage.prompt_tokens || 0} prompt tokens, ${usage.completion_tokens || 0} completion tokens`;
}

function setRunning(running, text) {
  state.running = running;
  $('#sendBtn').disabled = running || !state.keyPresent;
  $('#stopBtn').hidden = !running;
  setStatus(running ? 'busy' : 'ready', running ? text || 'working' : 'ready');
}

function setStatus(kind, text) {
  const dot = $('#statusDot');
  dot.className = 'status-dot' + (kind === 'busy' ? ' busy' : kind === 'error' ? ' error' : '');
  $('#statusText').textContent = text;
}

function toggleTheme() {
  const next = document.documentElement.getAttribute('data-theme') === 'light' ? 'dark' : 'light';
  applyTheme(next);
  localStorage.setItem('codedeep-theme', next);
}

function applyTheme(theme) {
  document.documentElement.setAttribute('data-theme', theme === 'light' ? 'light' : 'dark');
}

function closeSidebar() {
  $('#app').classList.remove('sidebar-open');
}

function resizeInput() {
  input.style.height = 'auto';
  input.style.height = Math.min(input.scrollHeight, 200) + 'px';
}

let toastTimer = null;
function toast(message, ms = 3400) {
  const node = $('#toast');
  node.textContent = message;
  node.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => {
    node.hidden = true;
  }, ms);
}

function scrollSoon() {
  const distance = chat.scrollHeight - chat.scrollTop - chat.clientHeight;
  if (distance < 280) chat.scrollTop = chat.scrollHeight;
}

function scrollToBottom(force) {
  if (force || chat.scrollHeight - chat.scrollTop - chat.clientHeight < 280) {
    chat.scrollTop = chat.scrollHeight;
  }
}

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined && text !== null) node.textContent = text;
  return node;
}

function parseMaybe(value) {
  if (value === null || value === undefined || value === '') return {};
  if (typeof value === 'object') return value;
  try {
    return JSON.parse(value);
  } catch {
    return String(value);
  }
}

function prettyArgs(args) {
  if (typeof args === 'string') return args;
  try {
    return JSON.stringify(args, null, 2);
  } catch {
    return String(args);
  }
}

function summarizeArgs(name, args) {
  if (args === null || args === undefined) return '';
  if (typeof args === 'string') return truncate(args, 140);
  switch (name) {
    case 'bash':
      return truncate(args.command || '', 150);
    case 'read_file':
      return args.path || '';
    case 'write_file':
      return `${args.path || ''} · ${(args.content || '').length} chars`;
    case 'edit_file':
      return args.path || '';
    case 'glob':
    case 'grep': {
      const where = args.path ? ' in ' + args.path : '';
      return truncate((args.pattern || '') + where, 140);
    }
    case 'agent':
      return truncate(args.description || '', 120);
    default: {
      const keys = Object.keys(args);
      const bits = keys.slice(0, 3).map((k) => `${k}=${shortValue(args[k])}`);
      return truncate(bits.join(' · '), 150);
    }
  }
}

function shortValue(value) {
  let s;
  try {
    s = typeof value === 'string' ? value : JSON.stringify(value);
  } catch {
    s = String(value);
  }
  s = String(s === undefined ? '' : s).replace(/\s+/g, ' ');
  return truncate(s, 34);
}

function toolGlyph(name) {
  if (!name) return '⚙';
  if (name === 'bash') return '$';
  if (name === 'read_file') return '▤';
  if (name === 'write_file' || name === 'edit_file') return '✎';
  if (name === 'glob') return '◇';
  if (name === 'grep') return '⌕';
  if (name === 'agent') return '✦';
  if (name.startsWith('mcp__')) return '⚙';
  return '▸';
}

function compactPath(path) {
  if (!path) return '';
  const clean = path.replace(/\\/g, '/');
  const parts = clean.split('/').filter(Boolean);
  if (parts.length <= 3) return clean;
  return '…/' + parts.slice(-2).join('/');
}

function truncate(text, max) {
  const s = String(text || '');
  return s.length > max ? s.slice(0, max - 1) + '…' : s;
}

function fmtShort(n) {
  n = Number(n) || 0;
  if (n < 1000) return String(n);
  if (n < 1000000) return (n / 1000).toFixed(n < 10000 ? 1 : 0) + 'k';
  return (n / 1000000).toFixed(1) + 'M';
}

function fmtTokens(n) {
  n = Number(n) || 0;
  return fmtShort(n) + ' tok';
}

function relTime(iso) {
  const t = new Date(iso).getTime();
  if (!isFinite(t)) return '';
  const seconds = Math.max(0, Math.floor((Date.now() - t) / 1000));
  if (seconds < 60) return 'just now';
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return minutes + 'm ago';
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return hours + 'h ago';
  const days = Math.floor(hours / 24);
  if (days < 7) return days + 'd ago';
  return new Date(iso).toLocaleDateString();
}

async function api(path, opts = {}) {
  const options = { ...opts, headers: { 'Content-Type': 'application/json', ...(opts.headers || {}) } };
  const res = await fetch(path, options);
  if (!res.ok) {
    let message = res.statusText || 'HTTP ' + res.status;
    try {
      const body = await res.json();
      if (body.error) message = body.error;
    } catch {
      /* ignore */
    }
    throw new Error(message);
  }
  if (res.status === 204) return null;
  return res.json();
}
