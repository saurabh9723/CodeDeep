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
  seq: 0,
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
  cost: 0,
  plan: false,
  checkpoints: 0,
  todos: [],
  files: { open: false, mention: false, path: '', seq: 0 },
  palette: { open: false, items: [], index: 0 },
  images: [],
  stats: { open: false, loading: false, pending: false, timer: null },
  traces: { open: false, loading: false, items: [], selected: null, trace: null, timer: null, compareBase: null },
};

const chat = $('#chat');
const input = $('#input');
const messagesEl = $('#messages');
const emptyState = $('#emptyState');

const SESSION_KEY = 'codedeep-session';

function rememberSession(id) {
  if (id) localStorage.setItem(SESSION_KEY, id);
}

function forgetSession() {
  localStorage.removeItem(SESSION_KEY);
}

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
    state.plan = !!boot.plan;
    $('#modelChip').textContent = boot.model;
    $('#modelChip').title = `${boot.provider || 'openai'} · ${boot.model} · ${boot.base_url || ''}`;
    $('#approvalChip').textContent = boot.approval + ' mode';
    updatePlanChip(boot.approval);
    setCheckpoints(0);
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
  const saved = localStorage.getItem(SESSION_KEY);
  if (saved) await loadSession(saved, { auto: true });
  resizeInput();
  input.focus();
}

function bindUI() {
  $('#sendBtn').addEventListener('click', send);
  $('#stopBtn').addEventListener('click', stopRun);
  $('#newChatBtn').addEventListener('click', newChat);
  $('#refreshBtn').addEventListener('click', refreshSessions);
  $('#themeBtn').addEventListener('click', toggleTheme);
  $('#statsBtn').addEventListener('click', openStats);
  $('#tracesBtn').addEventListener('click', openTraces);
  $('#tracesCloseBtn').addEventListener('click', closeTraces);
  $('#tracesRefreshBtn').addEventListener('click', loadTraces);
  $('#tracesOverlay').addEventListener('click', (e) => {
    if (e.target === $('#tracesOverlay')) closeTraces();
  });
  $('#tracesKind').addEventListener('change', () => {
    localStorage.setItem('codedeep-traces-kind', $('#tracesKind').value);
    loadTraces();
  });
  $('#tracesSearch').addEventListener('input', renderTraceList);
  $('#statsCloseBtn').addEventListener('click', closeStats);
  $('#statsRefreshBtn').addEventListener('click', loadStats);
  $('#statsOverlay').addEventListener('click', (e) => {
    if (e.target === $('#statsOverlay')) closeStats();
  });
  $('#statsDays').addEventListener('change', () => {
    localStorage.setItem('codedeep-stats-days', $('#statsDays').value);
    loadStats();
  });
  $('#statsScope').addEventListener('change', () => {
    localStorage.setItem('codedeep-stats-scope', $('#statsScope').value);
    loadStats();
  });
  $('#planChip').addEventListener('click', togglePlan);
  $('#undoBtn').addEventListener('click', openCheckpoints);
  $('#checkpointsCloseBtn').addEventListener('click', closeCheckpoints);
  $('#todoHead').addEventListener('click', toggleTodos);
  $('#exportBtn').addEventListener('click', (e) => {
    e.stopPropagation();
    toggleExportMenu();
  });
  $('#exportMenu').addEventListener('click', (e) => {
    const btn = e.target.closest('button[data-format]');
    if (btn) exportSession(btn.dataset.format);
  });
  $('#filesBtn').addEventListener('click', () => {
    if (state.files.open) closeFilePalette();
    else openFilePalette(false);
  });
  $('#filesCloseBtn').addEventListener('click', closeFilePalette);
  $('#filesUpBtn').addEventListener('click', filePaletteUp);
  $('#filesFilter').addEventListener('input', onFileFilter);
  $('#filesFilter').addEventListener('keydown', (e) => {
    if (e.key === 'Escape') {
      e.preventDefault();
      closeFilePalette();
      input.focus();
    }
  });
  document.addEventListener('click', () => {
    const menu = $('#exportMenu');
    if (menu) menu.hidden = true;
  });
  let searchTimer = null;
  $('#sessionSearch').addEventListener('input', (e) => {
    clearTimeout(searchTimer);
    const q = e.target.value;
    searchTimer = setTimeout(() => refreshSessions(q), 220);
  });
  $('#sessionSearch').addEventListener('keydown', (e) => {
    if (e.key === 'Escape') {
      e.target.value = '';
      refreshSessions('');
    }
  });
  $('#helpBtn').addEventListener('click', () =>
    toast('Enter to send · Shift+Enter for a newline · Ctrl/⌘+N for a new chat · Ctrl/⌘+K to focus · Esc to stop', 6000)
  );
  $('#collapseBtn').addEventListener('click', () => {
    $('#app').classList.toggle('collapsed');
    localStorage.setItem('codedeep-collapsed', $('#app').classList.contains('collapsed') ? '1' : '0');
  });
  $('#menuBtn').addEventListener('click', () => $('#app').classList.add('sidebar-open'));
  $('#sidebarBackdrop').addEventListener('click', closeSidebar);

  input.addEventListener('input', onInputChanged);
  const composer = $('#composer');
  composer.addEventListener('paste', (e) => {
    const files = Array.from((e.clipboardData && e.clipboardData.items) || [])
      .filter((it) => it.kind === 'file' && (it.type || '').startsWith('image/'))
      .map((it) => it.getAsFile())
      .filter(Boolean);
    if (!files.length) return;
    e.preventDefault();
    files.forEach(addImageFile);
  });
  composer.addEventListener('dragover', (e) => {
    if (!e.dataTransfer || !Array.from(e.dataTransfer.types || []).includes('Files')) return;
    e.preventDefault();
    composer.classList.add('dragging');
  });
  composer.addEventListener('dragleave', () => composer.classList.remove('dragging'));
  composer.addEventListener('drop', (e) => {
    composer.classList.remove('dragging');
    if (!e.dataTransfer) return;
    const files = Array.from(e.dataTransfer.files || []).filter((f) => (f.type || '').startsWith('image/'));
    if (!files.length) return;
    e.preventDefault();
    files.forEach(addImageFile);
  });
  input.addEventListener('keydown', (e) => {
    if (state.palette.open) {
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        movePalette(1);
        return;
      }
      if (e.key === 'ArrowUp') {
        e.preventDefault();
        movePalette(-1);
        return;
      }
      if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
        e.preventDefault();
        runPaletteSelection();
        return;
      }
      if (e.key === 'Escape') {
        e.preventDefault();
        closeCommandPalette();
        return;
      }
    }
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
      if (state.palette.open) {
        e.preventDefault();
        closeCommandPalette();
      } else if (state.files.open) {
        e.preventDefault();
        closeFilePalette();
      } else if (!$('#checkpointPalette').hidden) {
        e.preventDefault();
        closeCheckpoints();
      } else if (state.stats.open) {
        e.preventDefault();
        closeStats();
      } else if (state.traces.open) {
        e.preventDefault();
        closeTraces();
      } else if (state.running) {
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

function renderSessions(metas, query) {
  state.lastSessions = metas || [];
  const list = $('#sessionList');
  list.innerHTML = '';
  if (!state.lastSessions.length) {
    list.append(
      el(
        'div',
        'session-empty',
        query
          ? `No sessions match “${truncate(query, 40)}”.`
          : 'No sessions yet. Your conversations are saved here.'
      )
    );
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
    if (meta.snippet) open.append(el('div', 'session-snippet', meta.snippet));
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

async function refreshSessions(query) {
  try {
    const q = query !== undefined ? String(query || '').trim() : ($('#sessionSearch').value || '').trim();
    const metas = await api('/api/sessions' + (q ? '?q=' + encodeURIComponent(q) : ''));
    renderSessions(metas, q);
    const mine = metas.find((m) => m.id === state.sessionId);
    if (mine && mine.title) setTitle(mine.title);
  } catch {
    /* non-fatal */
  }
}

async function loadSession(id, opts = {}) {
  if (state.running) stopRun();
  try {
    const sess = await api('/api/sessions/' + encodeURIComponent(id));
    state.sessionId = sess.id;
    rememberSession(sess.id);
    state.seq = 0;
    clearChat();
    renderHistory(sess);
    clearImages();
    setTitle(sess.title || 'Session');
    updateExportButton();
    if (sess.model) $('#modelChip').textContent = sess.model;
    setUsage(sess.usage || null, 0, sess.tool_usage || null);
    setCheckpoints((sess.checkpoints || []).length);
    setTodos(sess.todos || []);
    await refreshSessions();
    closeSidebar();
    if (sess.running) reattachRun(sess.id);
    input.focus();
  } catch (err) {
    if (opts.auto) {
      forgetSession();
      return;
    }
    toast('Could not load session: ' + err.message);
  }
}

async function deleteSession(id) {
  if (!window.confirm('Delete this session? This cannot be undone.')) return;
  try {
    await api('/api/sessions/' + encodeURIComponent(id), { method: 'DELETE' });
    if (state.sessionId === id) {
      state.sessionId = null;
      forgetSession();
      clearChat();
      showEmpty();
      setTitle('New session');
      setUsage(null);
      updateExportButton();
    }
    await refreshSessions();
  } catch (err) {
    toast('Could not delete session: ' + err.message);
  }
}

function newChat() {
  if (state.running) stopRun();
  state.sessionId = null;
  state.seq = 0;
  forgetSession();
  clearChat();
  clearImages();
  showEmpty();
  setTitle('New session');
  setUsage(null);
  setCheckpoints(0);
  setTodos([]);
  updateExportButton();
  renderSessions(state.lastSessions);
  closeSidebar();
  input.focus();
}

function updateExportButton() {
  const btn = $('#exportBtn');
  if (btn) btn.hidden = !state.sessionId;
}

function toggleExportMenu() {
  const menu = $('#exportMenu');
  if (!menu) return;
  if (!state.sessionId) {
    toast('Open or start a session to export it.');
    return;
  }
  menu.hidden = !menu.hidden;
}

function exportSession(format) {
  const menu = $('#exportMenu');
  if (menu) menu.hidden = true;
  if (!state.sessionId) {
    toast('Open or start a session to export it.');
    return;
  }
  const url =
    '/api/sessions/' +
    encodeURIComponent(state.sessionId) +
    '/export?format=' +
    (format === 'json' ? 'json' : 'md');
  const a = document.createElement('a');
  a.href = url;
  a.download = '';
  document.body.appendChild(a);
  a.click();
  a.remove();
  toast('Exporting as ' + (format === 'json' ? 'JSON' : 'Markdown') + '…');
}

/* ------------------------------------------------------------------ *
 * image attachments
 * ------------------------------------------------------------------ */

const MAX_IMAGES = 4;
const MAX_IMAGE_BYTES = 5 * 1024 * 1024;
const IMAGE_TYPES = ['image/png', 'image/jpeg', 'image/webp', 'image/gif'];

function addImageFile(file) {
  if (!file || !file.type) return;
  if (!IMAGE_TYPES.includes(file.type)) {
    toast('Use PNG, JPEG, WebP, or GIF images.');
    return;
  }
  if (state.images.length >= MAX_IMAGES) {
    toast(`Up to ${MAX_IMAGES} images per message.`);
    return;
  }
  if (file.size > MAX_IMAGE_BYTES) {
    toast('That image is larger than 5 MB.');
    return;
  }
  const reader = new FileReader();
  reader.onload = () => {
    const result = String(reader.result || '');
    const comma = result.indexOf(',');
    if (comma < 0) return;
    state.images.push({ mime: file.type, data: result.slice(comma + 1), name: file.name || 'pasted image' });
    renderAttachments();
  };
  reader.onerror = () => toast('Could not read that image.');
  reader.readAsDataURL(file);
}

function renderAttachments() {
  const tray = $('#attachments');
  if (!tray) return;
  tray.innerHTML = '';
  tray.hidden = state.images.length === 0;
  state.images.forEach((img, i) => {
    const item = el('div', 'attachment');
    item.title = img.name || 'attached image';
    const thumb = document.createElement('img');
    thumb.src = `data:${img.mime};base64,${img.data}`;
    thumb.alt = img.name || 'attached image';
    const remove = el('button', 'attachment-remove', '×');
    remove.type = 'button';
    remove.title = 'Remove image';
    remove.setAttribute('aria-label', 'Remove image');
    remove.addEventListener('click', (e) => {
      e.stopPropagation();
      state.images.splice(i, 1);
      renderAttachments();
    });
    item.append(thumb, remove);
    tray.append(item);
  });
}

function clearImages() {
  state.images = [];
  renderAttachments();
}

function imagePayload() {
  return state.images.map(({ mime, data }) => ({ mime, data }));
}

/* ------------------------------------------------------------------ *
 * sending and streaming
 * ------------------------------------------------------------------ */

async function send() {
  const text = input.value.trim();
  const images = imagePayload();
  if ((!text && !images.length) || state.running) return;
  if (text.startsWith('/')) {
    input.value = '';
    resizeInput();
    closeCommandPalette();
    await runSlashCommand(text);
    return;
  }
  if (!state.keyPresent) {
    toast('Set DEEPSEEK_API_KEY and restart the server to start chatting.', 5000);
    return;
  }

  input.value = '';
  resizeInput();
  await startChat(text, images, text);
}

async function startChat(text, images, displayText) {
  if (state.running) return;
  if (!state.keyPresent) {
    toast('Set DEEPSEEK_API_KEY and restart the server to start chatting.', 5000);
    return;
  }
  hideEmpty();
  const turn = newTurn();
  state.turn = turn;
  addUserMessage(displayText === undefined ? text : displayText, turn, images);
  state.assistant = createAssistant();
  state.assistantFresh = true;
  state.toolCards.clear();
  state.approvals.clear();
  state.subagents = [];
  state.seq = 0;

  state.abort = new AbortController();
  setRunning(true, 'thinking');

  try {
    const res = await fetch('/api/chat', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ session_id: state.sessionId || '', prompt: text, images }),
      signal: state.abort.signal,
    });
    if (res.status === 401) {
      location.assign('/login');
      return;
    }
    // The server names the session before the first byte, so a dropped
    // connection can still reattach.
    const sid = res.headers.get('X-Session-Id');
    if (sid) {
      state.sessionId = sid;
      rememberSession(sid);
      updateExportButton();
    }
    if (!res.ok) {
      let message = 'HTTP ' + res.status;
      try {
        const body = await res.json();
        if (body.error) message = body.error;
      } catch {
        /* ignore */
      }
      const err = new Error(message);
      err.noRetry = res.status < 500;
      throw err;
    }
    if (!res.body) throw new Error('This browser does not support streaming responses.');
    clearImages();
    await readStream(res.body);
  } catch (err) {
    if (err.name === 'AbortError') {
      addNote('Stopped by user', 'warn', turn);
    } else if (err.noRetry || !(await reconnectRun(turn))) {
      addErrorCard(err.message, turn);
      setStatus('error', 'error');
    }
  } finally {
    finishRunUI();
  }
}

function stopRun() {
  const id = state.sessionId;
  if (id) {
    setStatus('busy', 'stopping…');
    fetch('/api/sessions/' + encodeURIComponent(id) + '/stop', { method: 'POST' }).catch(() => {});
    return;
  }
  if (state.abort) state.abort.abort();
}

// reconnectRun reattaches after a dropped stream. The run keeps going on the
// server, so this is a replay from the last event the page saw.
async function reconnectRun(turn, opts = {}) {
  if (!state.sessionId) return false;
  let notice = false;
  for (let attempt = 0; attempt < 4; attempt++) {
    if (attempt > 0) {
      if (turn && state.turn !== turn) return false;
      await sleep(400 * attempt);
    }
    setStatus('busy', 'reconnecting…');
    const query = opts.resume && attempt === 0 ? '' : '?after=' + Math.max(0, state.seq);
    try {
      const res = await fetch(
        '/api/sessions/' + encodeURIComponent(state.sessionId) + '/stream' + query
      );
      if (res.status === 401) {
        location.assign('/login');
        return true;
      }
      if (res.status === 404 || !res.body) return false;
      if (!res.ok) continue;
      if (!notice) {
        notice = true;
        addNote('Reconnected to the running turn', 'ok', turn);
      }
      await readStream(res.body);
      return true;
    } catch (err) {
      if (err.name === 'AbortError') return true;
    }
  }
  return false;
}

// reattachRun resumes a turn that was still running when the page loaded.
async function reattachRun(id) {
  if (state.running || state.sessionId !== id) return;
  hideEmpty();
  state.seq = 0;
  state.assistant = null;
  state.assistantFresh = true;
  state.toolCards.clear();
  state.approvals.clear();
  state.subagents = [];
  if (!state.turn) newTurn();
  const turn = state.turn;
  setRunning(true, 'reconnecting…');
  const ok = await reconnectRun(turn, { resume: true });
  finishRunUI();
  if (!ok) {
    setStatus('ready', 'ready');
    await refreshSessionState();
    await refreshSessions();
  }
}

function finishRunUI() {
  setRunning(false);
  finalizeAssistant();
  state.abort = null;
  refreshSessionState();
  refreshSessions();
  input.focus();
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/* ------------------------------------------------------------------ *
 * file picker, @-mentions, and slash commands
 * ------------------------------------------------------------------ */

function onInputChanged() {
  resizeInput();
  const value = input.value;
  const caret = input.selectionStart || 0;
  const before = value.slice(0, caret);
  if (/(^|\s)@$/.test(before)) {
    openFilePalette(true);
    return;
  }
  if (/^\/[^\n]*$/.test(value)) {
    updateCommandPalette(value);
  } else {
    closeCommandPalette();
  }
}

function openFilePalette(mention) {
  state.files.open = true;
  state.files.mention = !!mention;
  state.files.path = '';
  $('#filePalette').hidden = false;
  $('#filesHint').textContent = mention
    ? 'Click a file or symbol to insert @path[:line]'
    : 'Click a file or symbol to insert its path';
  $('#filesFilter').value = '';
  closeCommandPalette();
  loadFiles('');
  $('#filesFilter').focus();
}

function closeFilePalette() {
  state.files.open = false;
  $('#filePalette').hidden = true;
}

function filePaletteUp() {
  if (!state.files.path) return;
  const parent = state.files.path.split('/').slice(0, -1).join('/');
  loadFiles(parent);
}

async function loadFiles(path) {
  const seq = ++state.files.seq;
  state.files.path = path || '';
  $('#filesPath').textContent = '/' + state.files.path;
  $('#filesUpBtn').disabled = !state.files.path;
  try {
    const entries = await api('/api/files?path=' + encodeURIComponent(state.files.path));
    if (seq !== state.files.seq) return;
    renderFileEntries(entries || [], false);
  } catch (err) {
    if (seq === state.files.seq) renderFileMessage('Could not list files: ' + err.message);
  }
}

let fileSearchTimer = null;
function onFileFilter() {
  clearTimeout(fileSearchTimer);
  const q = $('#filesFilter').value.trim();
  fileSearchTimer = setTimeout(() => {
    if (!state.files.open) return;
    if (q) searchFiles(q);
    else loadFiles('');
  }, 180);
}

async function searchFiles(query) {
  const seq = ++state.files.seq;
  const symbolsPromise = api(
    '/api/index?q=' + encodeURIComponent(query) + '&limit=12'
  ).catch(() => null);
  try {
    const entries = await api('/api/files/search?q=' + encodeURIComponent(query));
    if (seq !== state.files.seq) return;
    const indexed = await symbolsPromise;
    if (seq !== state.files.seq) return;
    $('#filesPath').textContent = '“' + truncate(query, 40) + '”';
    $('#filesUpBtn').disabled = true;
    renderSearchResults(entries || [], (indexed && indexed.results) || []);
  } catch (err) {
    if (seq === state.files.seq) renderFileMessage('Search failed: ' + err.message);
  }
}

function renderSearchResults(files, symbols) {
  const list = $('#filesList');
  list.innerHTML = '';
  if (!files.length && !symbols.length) {
    list.append(el('div', 'palette-empty', 'No matching files or symbols.'));
    return;
  }
  if (symbols.length) {
    list.append(el('div', 'palette-group', 'Symbols · ' + symbols.length));
    for (const sym of symbols) list.append(renderSymbolRow(sym));
  }
  if (files.length) {
    if (symbols.length) list.append(el('div', 'palette-group', 'Files · ' + files.length));
    for (const entry of files) list.append(renderFileRow(entry, true));
  }
}

function symbolGlyph(kind) {
  switch (kind) {
    case 'struct':
    case 'class':
    case 'interface':
    case 'type':
    case 'enum':
    case 'trait':
      return '◇';
    case 'const':
    case 'var':
      return '=';
    case 'heading':
      return '#';
    case 'module':
      return '▣';
    default:
      return 'ƒ';
  }
}

function renderSymbolRow(sym) {
  const ref = sym.file + ':' + sym.line;
  const row = el('button', 'palette-row palette-symbol');
  row.type = 'button';
  row.title = (sym.signature || sym.name) + ' — ' + ref;
  const label = sym.parent ? sym.name + ' · ' + sym.parent : sym.name;
  row.append(
    el('span', 'palette-icon', symbolGlyph(sym.kind)),
    el('span', 'palette-name', label),
    el('span', 'palette-desc', ref)
  );
  const open = el('span', 'palette-open', '↗');
  open.title = 'Open in editor';
  open.setAttribute('role', 'button');
  open.setAttribute('tabindex', '0');
  const launch = (e) => {
    e.preventDefault();
    e.stopPropagation();
    openInEditor(sym.file, sym.line);
  };
  open.addEventListener('click', launch);
  open.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ' ') launch(e);
  });
  row.append(open);
  row.addEventListener('click', () => insertFileMention(ref));
  return row;
}

function renderFileMessage(text) {
  const list = $('#filesList');
  list.innerHTML = '';
  list.append(el('div', 'palette-empty', text));
}

function renderFileEntries(entries, searching) {
  const list = $('#filesList');
  list.innerHTML = '';
  if (!entries.length) {
    list.append(el('div', 'palette-empty', searching ? 'No matching files.' : 'Empty directory.'));
    return;
  }
  for (const entry of entries) list.append(renderFileRow(entry, searching));
}

function renderFileRow(entry, searching) {
  const row = el('button', 'palette-row');
  row.type = 'button';
  row.title = entry.path;
  row.append(
    el('span', 'palette-icon', entry.dir && !searching ? '▸' : '·'),
    el('span', 'palette-name', entry.name)
  );
  if (entry.dir && !searching) row.append(el('span', 'palette-desc', 'dir'));
  else {
    const open = el('span', 'palette-open', '↗');
    open.title = 'Open in editor';
    open.setAttribute('role', 'button');
    open.setAttribute('tabindex', '0');
    const launch = (e) => {
      e.preventDefault();
      e.stopPropagation();
      openInEditor(entry.path);
    };
    open.addEventListener('click', launch);
    open.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' || e.key === ' ') launch(e);
    });
    row.append(open);
  }
  row.addEventListener('click', () => {
    if (entry.dir && !searching) loadFiles(entry.path);
    else insertFileMention(entry.path);
  });
  return row;
}

function insertFileMention(path) {
  const mention = state.files.mention;
  closeFilePalette();
  const value = input.value;
  const caret = input.selectionStart || value.length;
  const before = value.slice(0, caret);
  const trailing = value.slice(caret);
  if (mention) {
    const m = /(^|\s)@([^\s@]*)$/.exec(before);
    if (m) {
      const start = before.length - m[2].length - 1;
      input.value = value.slice(0, start) + '@' + path + ' ' + trailing;
    } else {
      input.value = before + '@' + path + ' ' + trailing;
    }
  } else {
    const sep = before && !/\s$/.test(before) ? ' ' : '';
    input.value = before + sep + path + ' ' + trailing;
  }
  input.focus();
  input.selectionStart = input.selectionEnd = input.value.length;
  resizeInput();
}

const slashCommands = [
  { name: '/compact', desc: 'Summarize older messages', run: () => compactNow() },
  { name: '/new', desc: 'Start a new session', run: () => newChat() },
  { name: '/plan', desc: 'Toggle plan mode', run: () => togglePlan() },
  { name: '/checkpoints', desc: 'Review per-turn diffs', run: () => openCheckpoints() },
  { name: '/undo', desc: 'Revert the last turn', run: () => undoLastTurn() },
  { name: '/skills', desc: 'List workspace skills', run: () => showSkills() },
  { name: '/skill', desc: 'Run a skill: /skill name task', run: () => toast('Usage: /skill <name> [task]') },
  { name: '/open', desc: 'Open a file in your editor', run: () => toast('Usage: /open <path>[:line]') },
  { name: '/export md', desc: 'Export as Markdown', run: () => exportSession('md') },
  { name: '/export json', desc: 'Export as JSON', run: () => exportSession('json') },
  { name: '/sessions', desc: 'Search sessions', run: () => $('#sessionSearch').focus() },
  {
    name: '/help',
    desc: 'Keyboard shortcuts',
    run: () => toast('Enter to send · Shift+Enter newline · @ files · / commands · Ctrl/⌘+K focus', 6000),
  },
];

function updateCommandPalette(value) {
  const q = value.trim().toLowerCase();
  const items = slashCommands.filter(
    (cmd) => cmd.name.startsWith(q) || (q.length > 1 && cmd.desc.toLowerCase().includes(q.slice(1)))
  );
  if (!items.length) {
    closeCommandPalette();
    return;
  }
  state.palette.open = true;
  state.palette.items = items;
  if (state.palette.index >= items.length) state.palette.index = 0;
  const list = $('#commandList');
  list.innerHTML = '';
  items.forEach((cmd, i) => {
    const row = el('button', 'palette-row' + (i === state.palette.index ? ' selected' : ''));
    row.type = 'button';
    row.append(el('span', 'palette-name', cmd.name), el('span', 'palette-desc', cmd.desc));
    row.addEventListener('click', () => {
      closeCommandPalette();
      input.value = '';
      resizeInput();
      cmd.run();
    });
    list.append(row);
  });
  $('#commandPalette').hidden = false;
}

function closeCommandPalette() {
  state.palette.open = false;
  state.palette.items = [];
  state.palette.index = 0;
  $('#commandPalette').hidden = true;
}

function movePalette(delta) {
  if (!state.palette.items.length) return;
  const n = state.palette.items.length;
  state.palette.index = (state.palette.index + delta + n) % n;
  updateCommandPalette(input.value);
}

function runPaletteSelection() {
  const cmd = state.palette.items[state.palette.index];
  if (!cmd) return;
  closeCommandPalette();
  input.value = '';
  resizeInput();
  cmd.run();
}

async function runSlashCommand(text) {
  const parts = text.trim().split(/\s+/);
  switch (parts[0].toLowerCase()) {
    case '/compact':
      return compactNow();
    case '/new':
      newChat();
      return;
    case '/plan':
      return togglePlan();
    case '/checkpoints':
      return openCheckpoints();
    case '/undo':
      return undoLastTurn();
    case '/export':
      exportSession(parts[1] === 'json' ? 'json' : 'md');
      return;
    case '/sessions':
      $('#sessionSearch').focus();
      return;
    case '/skills':
      return showSkills();
    case '/skill': {
      const name = parts[1];
      if (!name) {
        toast('Usage: /skill <name> [task]');
        return;
      }
      const task = text.trim().replace(/^\/skill\s+\S+\s*/i, '').trim();
      return runSkill(name, task);
    }
    case '/open': {
      const ref = text.trim().replace(/^\/open\s*/i, '').trim();
      if (!ref) {
        toast('Usage: /open <path>[:line]');
        return;
      }
      return openInEditor(ref);
    }
    case '/help':
      toast(
        'Slash commands: /compact, /new, /plan, /checkpoints, /undo, /skills, /skill, /open, /export md|json, /sessions, /help',
        6000
      );
      return;
    default:
      toast('Unknown command ' + parts[0] + '. Type / to browse commands.');
  }
}

async function showSkills() {
  try {
    const list = await api('/api/skills');
    if (!list || !list.length) {
      addNote('No skills found. Add markdown files to .codedeep/skills or ~/.codedeep/skills.');
      return;
    }
    const card = el('div', 'note-card');
    card.append(el('div', '', `Workspace skills (${list.length}):`));
    for (const skill of list) {
      const line = el('div', 'skill-line');
      line.append(
        el('span', 'skill-name', '/' + skill.name + (skill.description ? ' — ' + skill.description : '')),
        el('span', 'skill-path', compactPath(skill.path || ''))
      );
      card.append(line);
    }
    (state.turn || newTurn()).append(card);
    scrollSoon();
  } catch (err) {
    toast('Could not list skills: ' + err.message);
  }
}

async function runSkill(name, task) {
  try {
    const skill = await api('/api/skills/' + encodeURIComponent(name));
    if (!task) {
      const details = document.createElement('details');
      details.className = 'summary-card';
      const summary = document.createElement('summary');
      summary.textContent = `Skill: ${skill.name}`;
      const body = el('pre', '', skill.body || '(empty skill)');
      details.append(summary, body);
      details.open = true;
      (state.turn || newTurn()).append(details);
      scrollSoon();
      return;
    }
    let prompt = `Use the skill "${skill.name}".\n\n${skill.body || ''}`;
    if (task) prompt += `\n\n# Task\n${task}`;
    await startChat(prompt, [], `/skill ${skill.name} ${task}`);
  } catch (err) {
    toast('Could not load skill: ' + err.message, 5000);
  }
}

async function openInEditor(ref, line) {
  try {
    const res = await api('/api/open', {
      method: 'POST',
      body: JSON.stringify({ path: ref, line: line || 0 }),
    });
    toast('Opened ' + compactPath(res.opened || ref));
  } catch (err) {
    toast('Could not open editor: ' + err.message, 5000);
  }
}

async function compactNow() {
  if (state.running) {
    toast('Wait for the current run to finish.');
    return;
  }
  if (!state.sessionId) {
    toast('Open or start a session first.');
    return;
  }
  if (!state.keyPresent) {
    toast('Set DEEPSEEK_API_KEY to compact a session.');
    return;
  }
  setStatus('busy', 'compacting');
  try {
    const res = await api('/api/compact', {
      method: 'POST',
      body: JSON.stringify({ session_id: state.sessionId }),
    });
    if (res.usage) setUsage(res.usage);
    toast('Compacted ' + res.removed + ' messages.');
    await refreshSessionState();
    await refreshSessions();
  } catch (err) {
    toast('Compact failed: ' + err.message, 5000);
  } finally {
    setStatus('ready', 'ready');
    input.focus();
  }
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
      if (line) await handleLine(line);
    }
  }
  const tail = buffer.trim();
  if (tail) await handleLine(tail);
}

async function handleLine(line) {
  let ev;
  try {
    ev = JSON.parse(line);
  } catch {
    return;
  }
  await handleEvent(ev);
}

async function handleEvent(ev) {
  if (typeof ev.seq === 'number') state.seq = ev.seq;
  switch (ev.type) {
    case 'reset':
      await reloadForReset(ev.session_id);
      break;
    case 'session':
      if (ev.session_id) {
        state.sessionId = ev.session_id;
        rememberSession(ev.session_id);
        updateExportButton();
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
    case 'diff':
      onDiff(ev);
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
      setUsage(ev.total_usage || ev.usage || null, ev.total_cost_usd || 0);
      break;
    case 'compact':
      addNote(ev.message || 'Context compacted');
      break;
    case 'hook':
      if (ev.is_error) {
        const where = ev.name ? ev.name + '-tool hook' : 'hook';
        addNote(`${where} failed for ${ev.message || 'a tool'}: ${truncate(ev.output || '', 600)}`, 'warn');
      }
      break;
    case 'retry': {
      const r = ev.retry || {};
      const reason = r.message || 'busy';
      const delay = formatDelay(r.delay_ms || 0);
      addNote(`Provider ${reason} — retrying in ${delay} (attempt ${r.attempt || 1}/${r.max || 1})`, 'warn');
      setStatus('busy', r.status === 429 ? 'rate limited' : 'retrying');
      break;
    }
    case 'diagnostics':
      addDiagnostics(ev);
      break;
    case 'todos':
      setTodos(ev.todos || []);
      break;
    case 'verify_start':
      setStatus('busy', 'verifying');
      break;
    case 'verify':
      if (ev.is_error) {
        addNote(`Verification failed: ${ev.message || 'verify'}\n${truncate(ev.output || '', 1200)}`, 'warn');
      } else {
        addNote(`Verification passed: ${ev.message || 'verify'}`);
      }
      break;
    case 'error':
      if (ev.message && ev.message !== 'context canceled') addErrorCard(ev.message);
      setStatus('error', 'error');
      break;
    case 'done':
      if (ev.message === 'stopped') addNote('Stopped by user', 'warn');
      if (typeof ev.checkpoints === 'number') setCheckpoints(ev.checkpoints);
      addToolUsage(ev.tool_usage);
      scheduleStatsRefresh();
      scheduleTracesRefresh();
      break;
    default:
      break;
  }
}

function formatDelay(ms) {
  const n = Number(ms) || 0;
  return n < 1000 ? n + 'ms' : (n / 1000).toFixed(1) + 's';
}

// reloadForReset rebuilds the transcript from disk after the server dropped
// events the page never saw. Remaining events in the stream keep appending.
async function reloadForReset(id) {
  if (!id) return;
  try {
    const sess = await api('/api/sessions/' + encodeURIComponent(id));
    clearChat();
    renderHistory(sess);
    setTitle(sess.title || 'Session');
    if (sess.model) $('#modelChip').textContent = sess.model;
    setUsage(sess.usage || null, 0, sess.tool_usage || null);
    setCheckpoints((sess.checkpoints || []).length);
    setTodos(sess.todos || []);
  } catch {
    /* keep the current transcript if the reload fails */
  }
  state.assistant = null;
  state.assistantFresh = true;
  if (!state.turn) newTurn();
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
  const cost = el('span', 'tool-cost');
  if (ev.usage && ev.usage.total_tokens) {
    const usd = Number(ev.cost_usd) || costFor(ev.usage, state.model);
    cost.textContent = '↑' + fmtShort(ev.usage.prompt_tokens) + ' ↓' + fmtShort(ev.usage.completion_tokens) +
      (usd ? ' · ' + fmtCost(usd) : '');
    cost.title = `${ev.usage.prompt_tokens || 0} prompt tokens, ${ev.usage.completion_tokens || 0} completion tokens` +
      (usd ? `, estimated ${fmtCost(usd)} for this tool call` : '');
    head.append(icon, name, args, cost, status);
  } else {
    head.append(icon, name, args, status);
  }

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

function onDiff(ev) {
  const depth = ev.depth || 0;
  const parent = depth > 0 && state.subagents.length ? subagentHost() : (state.turn || newTurn());
  const card = el('div', 'diff-card' + (depth > 0 ? ' nested' : ''));
  const head = el('div', 'diff-head');
  head.append(
    el('span', 'diff-icon', '±'),
    el('span', 'diff-path', ev.path || ev.name || 'changes'),
    el('span', 'diff-badge', ev.name || 'proposed changes')
  );
  const pre = el('pre', 'diff-pre');
  appendDiff(pre, ev.diff || '');
  card.append(head, pre);
  parent.append(card);
  scrollSoon();
}

function appendDiff(pre, text) {
  const lines = String(text || '').split('\n');
  const max = 400;
  for (let i = 0; i < lines.length; i++) {
    if (i === lines.length - 1 && lines[i] === '') continue;
    if (i >= max) {
      pre.append(el('span', 'diff-line meta', `… ${lines.length - max} more lines`));
      break;
    }
    const line = lines[i];
    let cls = 'diff-line';
    if (line.startsWith('+++') || line.startsWith('---')) cls += ' meta';
    else if (line.startsWith('@@')) cls += ' hunk';
    else if (line.startsWith('+')) cls += ' add';
    else if (line.startsWith('-')) cls += ' del';
    else if (line.startsWith('[')) cls += ' meta';
    pre.append(el('span', cls, line || ' '));
  }
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
    el('span', 'approval-note', ev.diff ? 'review the proposed diff above' : 'wants to run in your workspace')
  );

  const argsText = ev.diff
    ? truncate(prettyArgs(ev.args), 1600)
    : prettyArgs(ev.args) || '(no arguments)';
  const pre = el('pre', 'approval-args', argsText);
  const actions = el('div', 'approval-actions');
  const result = el('div', 'approval-result');

  const allow = el('button', 'btn primary', 'Allow once');
  allow.type = 'button';
  allow.addEventListener('click', () => respondApproval(ev.approval_id, 'allow'));

  const always = el('button', 'btn', approvalAlwaysLabel(ev));
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

function approvalAlwaysLabel(ev) {
  const args = parseMaybe(ev.args) || {};
  if (ev.name === 'bash' && args.command) return 'Always allow this command';
  if ((ev.name === 'write_file' || ev.name === 'edit_file') && args.path) return 'Always allow this file';
  if (ev.name === 'apply_patch' && Array.isArray(args.edits) && args.edits.length) return 'Always allow these files';
  return 'Always allow ' + (ev.name || 'tool');
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
      if ((m.content || '').startsWith('[automatic verification failed]')) {
        if (!turn) turn = newTurn();
        addNote('Automatic verification failed — the failure was sent back to the model', 'warn', turn);
        continue;
      }
      turn = newTurn();
      if ((m.content || '').startsWith('[Conversation summary')) {
        addSummaryCard(m.content || '', turn);
      } else {
        addUserMessage(m.content || '', turn, m.images || []);
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

function addUserMessage(text, turn, images) {
  const msg = el('div', 'msg user');
  const bubble = el('div', 'bubble');
  if (text) bubble.append(el('div', 'bubble-text', text));
  const list = images || [];
  if (list.length) {
    const grid = el('div', 'bubble-images');
    for (const img of list) {
      const thumb = document.createElement('img');
      thumb.src = `data:${img.mime || 'image/png'};base64,${img.data}`;
      thumb.alt = 'attached image';
      thumb.loading = 'lazy';
      grid.append(thumb);
    }
    bubble.append(grid);
  }
  msg.append(bubble);
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

function addDiagnostics(ev) {
  const list = Array.isArray(ev.annotations) ? ev.annotations : [];
  if (!list.length) {
    if (ev.is_error) {
      const target = ev.path || ev.name || 'changed file';
      addNote(`Diagnostics failed for ${target}: ${truncate(ev.output || '', 600)}`, 'warn');
    }
    return;
  }
  const card = el('div', 'diag-card' + (ev.is_error ? ' failed' : ''));
  const head = el('div', 'diag-head');
  head.append(
    el('span', 'diag-icon', '▸'),
    el('span', 'diag-title', ev.message || ev.name || 'diagnostics')
  );
  if (ev.path) head.append(el('span', 'diag-file', ev.path));
  card.append(head);
  for (const d of list) {
    const row = el('div', 'diag-row' + (d.severity ? ' ' + d.severity : ''));
    const loc = el('button', 'diag-loc', diagLocation(d));
    loc.type = 'button';
    loc.title = 'Open in editor';
    loc.addEventListener('click', () => openInEditor(d.path, d.line || 0));
    row.append(
      loc,
      el('span', 'diag-sev', d.severity || 'info'),
      el('span', 'diag-msg', d.message || '')
    );
    card.append(row);
  }
  (state.turn || newTurn()).append(card);
  scrollSoon();
}

function diagLocation(d) {
  let loc = d.path || '';
  if (d.line) loc += ':' + d.line;
  if (d.column) loc += ':' + d.column;
  return loc;
}

function addToolUsage(usage) {
  const names = Object.keys(usage || {}).filter((name) => {
    const u = usage[name];
    return u && u.usage && u.usage.total_tokens;
  });
  if (!names.length) return;
  names.sort();
  const card = el('div', 'tool-usage-card');
  card.append(el('div', 'tool-usage-head', 'Attributed to tool calls'));
  for (const name of names) {
    const u = usage[name];
    const usd = Number(u.cost_usd) || costFor(u.usage, state.model);
    const row = el('div', 'tool-usage-row');
    row.append(
      el('span', 'tool-usage-name', `${name} ×${u.calls || 1}`),
      el('span', 'tool-usage-tokens', `↑${fmtShort(u.usage.prompt_tokens)} ↓${fmtShort(u.usage.completion_tokens)} · ${fmtShort(u.usage.total_tokens)} tok`),
      el('span', 'tool-usage-cost', fmtCost(usd) || '—')
    );
    card.append(row);
  }
  (state.turn || newTurn()).append(card);
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

function pricingFor(model) {
  const table = (state.boot && state.boot.pricing) || {};
  const p = table[model || $('#modelChip').textContent];
  if (!p || (!p.input_per_mtok && !p.output_per_mtok)) return null;
  return p;
}

function costFor(usage, model) {
  const p = pricingFor(model);
  if (!p || !usage) return 0;
  const prompt = Number(usage.prompt_tokens) || 0;
  let cache = Number(usage.prompt_cache_hit_tokens) || 0;
  cache = Math.max(0, Math.min(cache, prompt));
  const miss = prompt - cache;
  const cacheRate = p.cache_hit_per_mtok > 0 ? p.cache_hit_per_mtok : p.input_per_mtok || 0;
  return (
    (miss * (p.input_per_mtok || 0) +
      (Number(usage.completion_tokens) || 0) * (p.output_per_mtok || 0) +
      cache * cacheRate) /
    1e6
  );
}

function fmtCost(value) {
  const v = Number(value) || 0;
  if (v <= 0) return '';
  if (v < 0.01) return '~$' + v.toFixed(4);
  if (v < 1) return '~$' + v.toFixed(3);
  return '~$' + v.toFixed(2);
}

function setUsage(usage, totalCost, toolUsage) {
  state.usage = usage || null;
  state.cost = typeof totalCost === 'number' && totalCost > 0 ? totalCost : costFor(state.usage);
  const chip = $('#usageChip');
  if (!usage || !usage.total_tokens) {
    chip.hidden = true;
    return;
  }
  chip.hidden = false;
  const cost = fmtCost(state.cost);
  chip.textContent =
    `↑ ${fmtShort(usage.prompt_tokens)} · ↓ ${fmtShort(usage.completion_tokens)} · ${fmtShort(usage.total_tokens)} tok` +
    (cost ? ` · ${cost}` : '');
  let title =
    `${usage.prompt_tokens || 0} prompt tokens, ${usage.completion_tokens || 0} completion tokens` +
    (cost ? `, estimated ${cost}` : '');
  const names = Object.keys(toolUsage || {});
  if (names.length) {
    names.sort();
    const parts = names.map((name) => {
      const u = toolUsage[name] || {};
      const tokens = (u.usage && u.usage.total_tokens) || 0;
      const usd = Number(u.cost_usd) || 0;
      return `${name} ×${u.calls || 1} ${fmtShort(tokens)} tok${usd ? ' ' + fmtCost(usd) : ''}`;
    });
    title += `; by tool: ${parts.join(' · ')}`;
  }
  chip.title = title;
}

function setCheckpoints(count) {
  state.checkpoints = Number(count) || 0;
  const btn = $('#undoBtn');
  if (!btn) return;
  btn.hidden = state.checkpoints === 0;
  btn.title =
    state.checkpoints === 0
      ? 'No file changes to review'
      : 'Review per-turn diffs and roll back' +
        (state.checkpoints > 1 ? ` (${state.checkpoints} checkpoints)` : '');
}

function setTodos(todos) {
  state.todos = Array.isArray(todos) ? todos : [];
  renderTodos();
}

function renderTodos() {
  const panel = $('#todoPanel');
  const list = $('#todoList');
  const progress = $('#todoProgress');
  if (!panel || !list || !progress) return;
  const todos = state.todos;
  if (!todos.length) {
    panel.hidden = true;
    list.textContent = '';
    progress.textContent = '';
    return;
  }
  panel.hidden = false;

  let done = 0;
  let active = false;
  list.textContent = '';
  for (const todo of todos) {
    const status = todo.status || 'pending';
    if (status === 'completed') done++;
    const row = el('li', 'todo-row ' + status);
    const text = todo.text || '';
    if (status === 'completed') {
      row.append(el('span', 'todo-icon', '✓'), el('span', 'todo-text', text));
    } else if (status === 'in_progress') {
      row.append(el('span', 'todo-icon', '◐'), el('span', 'todo-text', text));
      active = true;
    } else {
      row.append(el('span', 'todo-icon', '○'), el('span', 'todo-text', text));
    }
    list.append(row);
  }
  progress.textContent = `${done}/${todos.length}`;
  panel.classList.toggle('active', active);
}

function toggleTodos() {
  const panel = $('#todoPanel');
  const head = $('#todoHead');
  if (!panel || !head) return;
  const collapsed = panel.classList.toggle('collapsed');
  head.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
  const caret = $('#todoCaret');
  if (caret) caret.textContent = collapsed ? '▸' : '▾';
}

function updatePlanChip(approval) {
  if (approval) state.plan = approval === 'plan';
  const chip = $('#planChip');
  if (!chip) return;
  chip.textContent = state.plan ? 'plan on' : 'plan off';
  chip.classList.toggle('on', state.plan);
  chip.setAttribute('aria-pressed', state.plan ? 'true' : 'false');
  const mode = $('#approvalChip');
  if (mode && approval) mode.textContent = approval + ' mode';
}

async function refreshSessionState() {
  if (!state.sessionId) return;
  try {
    const sess = await api('/api/sessions/' + encodeURIComponent(state.sessionId));
    setCheckpoints((sess.checkpoints || []).length);
    setTodos(sess.todos || []);
    setUsage(sess.usage || null);
  } catch {
    /* non-fatal */
  }
}

async function togglePlan() {
  try {
    const res = await api('/api/plan', {
      method: 'POST',
      body: JSON.stringify({ enabled: !state.plan }),
    });
    updatePlanChip(res.approval);
    toast(res.plan ? 'Plan mode on — tools cannot modify files.' : 'Plan mode off — ' + res.approval + ' mode.');
  } catch (err) {
    toast('Could not change plan mode: ' + err.message);
  }
}

async function undoLastTurn() {
  if (state.running) {
    toast('Wait for the current run to finish.');
    return;
  }
  if (!state.sessionId) {
    toast('There is no session to undo.');
    return;
  }
  if (!window.confirm('Revert the file changes from the last turn?')) return;
  try {
    const res = await api('/api/undo', {
      method: 'POST',
      body: JSON.stringify({ session_id: state.sessionId }),
    });
    const files = res.reverted || [];
    toast(files.length ? 'Reverted ' + files.map(compactPath).join(', ') : 'Nothing to undo.');
    await refreshSessionState();
  } catch (err) {
    toast('Could not undo: ' + err.message, 5000);
  }
}

/* ------------------------------------------------------------------ *
 * checkpoint review
 * ------------------------------------------------------------------ */

function openCheckpoints() {
  if (state.running) {
    toast('Wait for the current run to finish.');
    return;
  }
  if (!state.sessionId) {
    toast('There is no session to review.');
    return;
  }
  closeFilePalette();
  closeCommandPalette();
  $('#checkpointPalette').hidden = false;
  loadCheckpoints();
}

function closeCheckpoints() {
  $('#checkpointPalette').hidden = true;
}

async function loadCheckpoints() {
  const list = $('#checkpointsList');
  if (!state.sessionId) return;
  list.innerHTML = '';
  list.append(el('div', 'palette-empty', 'Loading checkpoints…'));
  try {
    const res = await api('/api/sessions/' + encodeURIComponent(state.sessionId) + '/checkpoints');
    list.innerHTML = '';
    const items = res.checkpoints || [];
    if (!items.length) {
      list.append(el('div', 'palette-empty', 'No checkpoints yet — one is kept for every turn that changes files.'));
      return;
    }
    for (const cp of items.slice().reverse()) list.append(checkpointRow(cp));
  } catch (err) {
    list.innerHTML = '';
    list.append(el('div', 'palette-empty', 'Could not list checkpoints: ' + err.message));
  }
}

function checkpointRow(cp) {
  const row = el('div', 'checkpoint-row');
  const head = el('button', 'checkpoint-head');
  head.type = 'button';
  const stats = el('span', 'checkpoint-stats');
  if (cp.added || cp.removed) {
    stats.append(
      el('span', 'cp-add', '+' + (cp.added || 0)),
      el('span', 'cp-del', '-' + (cp.removed || 0))
    );
  } else {
    stats.append(el('span', 'palette-desc', 'no changes'));
  }
  head.append(
    el('span', 'checkpoint-num', '#' + cp.number),
    el('span', 'checkpoint-turn', 'turn ' + cp.turn),
    stats,
    el('span', 'checkpoint-files', cp.files + ' file' + (cp.files === 1 ? '' : 's')),
    el('span', 'checkpoint-time', new Date(cp.time).toLocaleString())
  );
  const body = el('div', 'checkpoint-body');
  body.hidden = true;
  head.addEventListener('click', () => {
    if (!body.hidden) {
      body.hidden = true;
      return;
    }
    if (!body.childElementCount) loadCheckpointDiff(cp.number, body);
    body.hidden = false;
  });
  const rollback = el('button', 'btn small', 'Roll back here');
  rollback.type = 'button';
  rollback.title = 'Revert every file change from this turn onward';
  rollback.addEventListener('click', (e) => {
    e.stopPropagation();
    rollbackCheckpoint(cp.number, cp.turn);
  });
  row.append(head, body, rollback);
  return row;
}

async function loadCheckpointDiff(number, body) {
  body.append(el('div', 'palette-empty', 'Loading diff…'));
  try {
    const cp = await api(
      '/api/sessions/' + encodeURIComponent(state.sessionId) + '/checkpoints/' + number
    );
    body.innerHTML = '';
    const pre = el('pre', 'diff-pre');
    appendDiff(pre, cp.diff || '');
    if (!cp.diff) pre.append(el('span', 'diff-line meta', '(no recorded diff)'));
    body.append(pre);
  } catch (err) {
    body.innerHTML = '';
    body.append(el('div', 'palette-empty', 'Could not load diff: ' + err.message));
  }
}

async function rollbackCheckpoint(number, turn) {
  if (state.running) {
    toast('Wait for the current run to finish.');
    return;
  }
  const n = state.checkpoints || 0;
  const through = n - number + 1;
  if (
    !window.confirm(
      `Roll back through checkpoint #${number} (turn ${turn})?` +
        (through > 1 ? ` This reverts the file changes from ${through} turns.` : ' This reverts that turn\u2019s file changes.')
    )
  )
    return;
  try {
    const res = await api('/api/undo', {
      method: 'POST',
      body: JSON.stringify({ session_id: state.sessionId, checkpoint: number }),
    });
    const files = res.reverted || [];
    toast(files.length ? 'Rolled back ' + files.map(compactPath).join(', ') : 'Nothing to revert.');
    closeCheckpoints();
    await refreshSessionState();
  } catch (err) {
    toast('Could not roll back: ' + err.message, 5000);
  }
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

/* ------------------------------------------------------------------ *
 * observability dashboard
 * ------------------------------------------------------------------ */

function openStats() {
  if (state.stats.open) return;
  state.stats.open = true;
  restoreStatsPrefs();
  $('#statsOverlay').hidden = false;
  document.body.classList.add('stats-open');
  loadStats();
}

function closeStats() {
  state.stats.open = false;
  state.stats.pending = false;
  clearTimeout(state.stats.timer);
  state.stats.timer = null;
  $('#statsOverlay').hidden = true;
  document.body.classList.remove('stats-open');
  input.focus();
}

function restoreStatsPrefs() {
  const days = localStorage.getItem('codedeep-stats-days');
  if (days && Array.from($('#statsDays').options).some((o) => o.value === days)) {
    $('#statsDays').value = days;
  }
  const scope = localStorage.getItem('codedeep-stats-scope');
  if (scope && Array.from($('#statsScope').options).some((o) => o.value === scope)) {
    $('#statsScope').value = scope;
  }
}

function scheduleStatsRefresh() {
  if (!state.stats.open) return;
  clearTimeout(state.stats.timer);
  state.stats.timer = setTimeout(() => {
    state.stats.timer = null;
    loadStats();
  }, 500);
}

async function loadStats() {
  if (!state.stats.open) return;
  if (state.stats.loading) {
    state.stats.pending = true;
    return;
  }
  state.stats.loading = true;
  $('#statsBody').classList.add('busy');
  const days = Number($('#statsDays').value) || 30;
  const scope = $('#statsScope').value || 'all';
  try {
    const rep = await api(`/api/analytics?days=${days}&scope=${encodeURIComponent(scope)}`);
    renderStats(rep);
  } catch (err) {
    $('#statsSub').textContent = 'could not load analytics';
    toast('Analytics failed: ' + err.message);
  } finally {
    state.stats.loading = false;
    $('#statsBody').classList.remove('busy');
    if (state.stats.pending) {
      state.stats.pending = false;
      loadStats();
    }
  }
}

function renderStats(rep) {
  const s = rep.summary || {};
  const scope = rep.scope === 'workspace' ? 'this workspace' : 'all workspaces';
  const when = rep.generated ? new Date(rep.generated).toLocaleString() : '';
  $('#statsSub').textContent = `last ${rep.days} days · ${scope}${when ? ' · updated ' + when : ''}`;
  renderStatsCards(rep);
  renderStatsChart(rep.daily || []);
  renderCachePanel(s);
  renderModels(rep.models || [], rep.priced);
  renderTools(rep.tools || [], rep.heatmap || []);
  renderStatsSessions(rep.sessions || [], rep.priced);
}

function renderStatsCards(rep) {
  const s = rep.summary || {};
  const cards = $('#statsCards');
  cards.innerHTML = '';
  const cost = rep.priced ? fmtCost(s.cost_usd) || '~$0' : 'n/a';
  const add = (label, value, sub, title) => {
    const card = el('div', 'stats-card stats-metric');
    card.append(el('span', 'stats-metric-label', label));
    card.append(el('span', 'stats-metric-value', value));
    card.append(el('span', 'stats-metric-sub', sub));
    if (title) card.title = title;
    cards.append(card);
  };
  add('Sessions', String(s.sessions || 0), `${s.turns || 0} turns · ${s.active_days || 0} active days`);
  add(
    'Tokens',
    fmtShort(s.total_tokens),
    `↑ ${fmtShort(s.prompt_tokens)} · ↓ ${fmtShort(s.completion_tokens)}`,
    `${s.prompt_tokens || 0} prompt, ${s.completion_tokens || 0} completion`
  );
  const rate = s.prompt_tokens ? Math.round((s.cache_hit_rate || 0) * 100) : 0;
  add(
    'Cache hit',
    s.prompt_tokens ? rate + '%' : '—',
    `${fmtShort(s.cache_hit_tokens)} read · ${fmtShort(s.cache_write_tokens)} written`,
    s.prompt_tokens ? `${s.cache_hit_tokens || 0} of ${s.prompt_tokens} prompt tokens came from cache` : 'no prompt tokens recorded'
  );
  add('Cost', cost, rep.priced ? 'estimated' : 'no pricing configured');
  add('Tool calls', String(s.tool_calls || 0), `${(rep.tools || []).length} tool${(rep.tools || []).length === 1 ? '' : 's'} used`);
}

function renderStatsChart(days) {
  const chart = $('#statsChart');
  chart.innerHTML = '';
  $('#statsChartSub').textContent = '';
  $('#statsAxisStart').textContent = '';
  $('#statsAxisEnd').textContent = '';
  if (!days.length) {
    chart.append(el('div', 'stats-empty', 'No days in this range.'));
    return;
  }
  const max = Math.max(1, ...days.map((d) => Number(d.total_tokens) || 0));
  const total = days.reduce((sum, d) => sum + (Number(d.total_tokens) || 0), 0);
  if (total) $('#statsChartSub').textContent = fmtShort(total) + ' tok';
  $('#statsAxisStart').textContent = days[0].date;
  $('#statsAxisEnd').textContent = days[days.length - 1].date;
  for (const day of days) {
    const tokens = Number(day.total_tokens) || 0;
    const col = el('div', 'stats-chart-col' + (tokens ? '' : ' empty'));
    const bar = el('div', 'stats-bar');
    bar.style.height = Math.max(2, Math.round((tokens / max) * 100)) + '%';
    col.append(bar);
    const cost = Number(day.cost_usd) > 0 ? ' · ' + fmtCost(day.cost_usd) : '';
    col.title = `${day.date}: ${fmtShort(tokens)} tok · ${day.turns || 0} turns · ${day.tool_calls || 0} tool calls${cost}`;
    chart.append(col);
  }
}

function renderCachePanel(s) {
  const box = $('#statsCache');
  box.innerHTML = '';
  const prompt = Number(s.prompt_tokens) || 0;
  const hit = Number(s.cache_hit_tokens) || 0;
  const write = Number(s.cache_write_tokens) || 0;
  if (!prompt) {
    box.append(el('div', 'stats-empty', 'No prompt tokens recorded yet.'));
    return;
  }
  const rate = Math.max(0, Math.min(1, hit / prompt));
  const bar = el('div', 'stats-cache-bar');
  const used = el('div', 'stats-cache-hit');
  used.style.width = (rate * 100).toFixed(1) + '%';
  bar.append(used);
  box.append(bar);
  const rows = el('div', 'stats-cache-rows');
  const row = (label, value) => {
    const r = el('div', 'stats-cache-row');
    r.append(el('span', null, label));
    r.append(el('b', null, value));
    return r;
  };
  const miss = Math.max(0, prompt - hit - write);
  rows.append(
    row('Cache hit rate', (rate * 100).toFixed(1) + '%'),
    row('Read from cache', fmtShort(hit) + ' tok'),
    row('Written to cache', fmtShort(write) + ' tok'),
    row('Billed as input', fmtShort(miss) + ' tok')
  );
  box.append(rows);
}

function renderModels(models, priced) {
  const box = $('#statsModels');
  box.innerHTML = '';
  if (!models.length) {
    box.append(el('div', 'stats-empty', 'No model activity in this range.'));
    return;
  }
  for (const m of models) {
    const row = el('div', 'stats-model-row');
    row.append(el('span', 'stats-model-name', m.model || 'unknown'));
    const parts = [`${m.turns || 0} turns`, `${fmtShort(m.total_tokens)} tok`];
    if (m.sessions) parts.push(`${m.sessions} session${m.sessions === 1 ? '' : 's'}`);
    parts.push(`${Math.round((m.cache_hit_rate || 0) * 100)}% cached`);
    row.append(el('span', 'stats-model-meta', parts.join(' · ')));
    if (priced) row.append(el('span', 'stats-model-cost', fmtCost(m.cost_usd) || '~$0'));
    box.append(row);
  }
}

function renderTools(tools, heatmap) {
  const box = $('#statsTools');
  box.innerHTML = '';
  if (!tools.length) {
    box.append(el('div', 'stats-empty', 'No tool calls in this range.'));
    return;
  }
  const heat = new Map((heatmap || []).map((row) => [row.name, row.calls || []]));
  for (const t of tools) {
    const cell = el('div', 'stats-tool');
    const head = el('div', 'stats-tool-head');
    head.append(el('span', 'stats-tool-name', t.name));
    const parts = [`${t.calls || 0} calls`, `${fmtShort(t.tokens)} tok`];
    if (t.sessions) parts.push(`${t.sessions} session${t.sessions === 1 ? '' : 's'}`);
    if (t.cost_usd > 0) parts.push(fmtCost(t.cost_usd));
    head.append(el('span', 'stats-tool-meta', parts.join(' · ')));
    cell.append(head);
    const calls = heat.get(t.name);
    if (calls && calls.length) {
      const max = Math.max(1, ...calls);
      const grid = el('div', 'stats-heat');
      calls.forEach((n, hour) => {
        const c = el('div', 'heat-cell');
        if (n > 0) {
          const pct = Math.round(18 + (n / max) * 82);
          c.style.background = `color-mix(in srgb, var(--accent) ${pct}%, var(--panel-2))`;
        }
        c.title = `${hour}:00 — ${n} call${n === 1 ? '' : 's'}`;
        grid.append(c);
      });
      cell.append(grid);
    }
    box.append(cell);
  }
  const hours = el('div', 'stats-heat-hours');
  for (let h = 0; h < 24; h++) hours.append(el('span', null, h % 6 === 0 ? String(h) : ''));
  box.append(hours);
}

function renderStatsSessions(sessions, priced) {
  const box = $('#statsSessions');
  box.innerHTML = '';
  if (!sessions.length) {
    box.append(el('div', 'stats-empty', 'No sessions in this range.'));
    return;
  }
  for (const sess of sessions) {
    const row = el('button', 'stats-session-row');
    row.type = 'button';
    row.title = 'Open ' + (sess.title || sess.id);
    row.append(el('span', 'stats-session-title', sess.title || sess.id));
    const meta = [
      sess.model,
      relTime(sess.updated),
      `${sess.turns || 0} turns`,
      `${fmtShort(sess.total_tokens)} tok`,
      `${Math.round((sess.cache_hit_rate || 0) * 100)}% cached`,
    ]
      .filter(Boolean)
      .join(' · ');
    row.append(el('span', 'stats-session-meta', meta));
    if (priced) row.append(el('span', 'stats-session-cost', fmtCost(sess.cost_usd) || '~$0'));
    row.addEventListener('click', () => {
      closeStats();
      loadSession(sess.id);
    });
    box.append(row);
  }
}

/* ------------------------------------------------------------------ *
 * trace viewer
 * ------------------------------------------------------------------ */

function openTraces() {
  if (state.traces.open) return;
  state.traces.open = true;
  state.traces.compareBase = null;
  const kind = localStorage.getItem('codedeep-traces-kind');
  if (kind && Array.from($('#tracesKind').options).some((o) => o.value === kind)) {
    $('#tracesKind').value = kind;
  }
  $('#tracesOverlay').hidden = false;
  document.body.classList.add('stats-open');
  loadTraces();
}

function closeTraces() {
  state.traces.open = false;
  state.traces.compareBase = null;
  clearTimeout(state.traces.timer);
  state.traces.timer = null;
  $('#tracesOverlay').hidden = true;
  document.body.classList.remove('stats-open');
  input.focus();
}

function scheduleTracesRefresh() {
  if (!state.traces.open) return;
  clearTimeout(state.traces.timer);
  state.traces.timer = setTimeout(() => {
    state.traces.timer = null;
    loadTraces();
  }, 900);
}

async function loadTraces() {
  if (!state.traces.open || state.traces.loading) return;
  state.traces.loading = true;
  $('#tracesBody').classList.add('busy');
  const kind = $('#tracesKind').value || 'all';
  try {
    const rep = await api(`/api/traces?kind=${encodeURIComponent(kind)}`);
    state.traces.items = rep.traces || [];
    if (state.traces.selected && !state.traces.items.some((t) => t.id === state.traces.selected)) {
      state.traces.selected = null;
      state.traces.trace = null;
    }
    if (state.traces.compareBase && !state.traces.items.some((t) => t.id === state.traces.compareBase)) {
      state.traces.compareBase = null;
    }
    renderTraceList();
    updateTracesSub();
    if (!state.traces.selected && !state.traces.compareBase && state.traces.items.length && window.innerWidth > 900) {
      selectTrace(state.traces.items[0].id);
    }
  } catch (err) {
    $('#tracesSub').textContent = 'could not load traces';
    toast('Traces failed: ' + err.message);
  } finally {
    state.traces.loading = false;
    $('#tracesBody').classList.remove('busy');
  }
}

function updateTracesSub() {
  const items = state.traces.items || [];
  const evals = items.filter((t) => t.kind === 'eval').length;
  const live = items.filter((t) => t.kind === 'session').length;
  if (!items.length) {
    $('#tracesSub').textContent = 'no traces yet · run an eval or chat to create one';
    return;
  }
  $('#tracesSub').textContent = `${items.length} trace${items.length === 1 ? '' : 's'} · ${evals} eval · ${live} live`;
}

function filteredTraces() {
  const query = $('#tracesSearch').value.trim().toLowerCase();
  const items = state.traces.items || [];
  if (!query) return items;
  return items.filter((t) =>
    [t.title, t.task, t.model, t.provider, t.session_id, t.status, (t.tags || []).join(' ')]
      .join(' ')
      .toLowerCase()
      .includes(query)
  );
}

function renderTraceList() {
  const list = $('#tracesList');
  if (!list) return;
  list.innerHTML = '';
  const items = filteredTraces();
  if (!items.length) {
    list.append(el('div', 'stats-empty', 'No traces match this filter.'));
    return;
  }
  for (const meta of items) {
    const row = el('button', 'trace-row');
    row.type = 'button';
    if (meta.id === state.traces.selected) row.classList.add('active');
    if (meta.id === state.traces.compareBase) {
      row.classList.add('compare-base');
      row.title = 'Compare base · pick another trace to diff against this one';
    }
    const top = el('div', 'trace-row-top');
    if (meta.id === state.traces.compareBase) top.append(el('span', 'trace-compare-tag', 'A'));
    top.append(el('span', 'trace-kind ' + (meta.kind === 'eval' ? 'eval' : 'session'), meta.kind === 'eval' ? 'eval' : 'live'));
    top.append(el('span', 'trace-row-title', meta.title || meta.task || meta.id));
    top.append(el('span', 'trace-status ' + (meta.status || ''), meta.status || 'unknown'));
    row.append(top);
    const sub = el('div', 'trace-row-sub');
    const bits = [
      meta.kind === 'eval' ? meta.task : meta.model,
      meta.kind === 'eval' ? meta.mode : '',
      relTime(meta.created),
      meta.duration_ms ? fmtDuration(meta.duration_ms) : '',
      meta.total_tokens ? fmtShort(meta.total_tokens) + ' tok' : '',
    ].filter(Boolean);
    for (const bit of bits) sub.append(el('span', '', bit));
    row.append(sub);
    const failures = meta.failures || [];
    if (failures.length) {
      row.append(el('div', 'trace-row-sub', failures.length + ' issue' + (failures.length === 1 ? '' : 's') + ' · ' + truncate(failures[0], 90)));
    }
    row.addEventListener('click', () => selectTrace(meta.id));
    list.append(row);
  }
}

function fmtDuration(ms) {
  const n = Number(ms) || 0;
  if (n < 1000) return n + 'ms';
  if (n < 60000) return (n / 1000).toFixed(1) + 's';
  const minutes = Math.floor(n / 60000);
  return minutes + 'm ' + Math.round((n % 60000) / 1000) + 's';
}

async function selectTrace(id) {
  if (!id) return;
  if (state.traces.compareBase && id !== state.traces.compareBase) {
    await selectTraceCompare(state.traces.compareBase, id);
    return;
  }
  state.traces.selected = id;
  renderTraceList();
  const detail = $('#tracesDetail');
  detail.innerHTML = '';
  detail.append(el('div', 'stats-empty', 'Loading trace…'));
  try {
    const tr = await api('/api/traces/' + encodeURIComponent(id));
    if (state.traces.selected !== id) return;
    state.traces.trace = tr;
    renderTraceDetail(tr);
  } catch (err) {
    if (state.traces.selected !== id) return;
    detail.innerHTML = '';
    detail.append(el('div', 'stats-empty', 'Could not load trace: ' + err.message));
  }
}

async function deleteTrace(tr) {
  if (!window.confirm('Delete this trace? The saved event stream cannot be recovered.')) return;
  try {
    await api('/api/traces/' + encodeURIComponent(tr.id), { method: 'DELETE' });
    const wasCompareBase = state.traces.compareBase === tr.id;
    if (wasCompareBase) state.traces.compareBase = null;
    state.traces.items = state.traces.items.filter((t) => t.id !== tr.id);
    if (state.traces.selected === tr.id) {
      state.traces.selected = null;
      state.traces.trace = null;
      const detail = $('#tracesDetail');
      detail.innerHTML = '';
      detail.append(el('div', 'stats-empty', 'Trace deleted.'));
    }
    renderTraceList();
    updateTracesSub();
    if (wasCompareBase && state.traces.selected) selectTrace(state.traces.selected);
  } catch (err) {
    toast('Delete failed: ' + err.message);
  }
}

function startTraceCompare(id) {
  state.traces.compareBase = id;
  state.traces.selected = null;
  renderTraceList();
  const detail = $('#tracesDetail');
  detail.innerHTML = '';
  const box = el('div', 'trace-compare-pending');
  box.append(el('h3', 'trace-detail-title', 'Compare traces'));
  box.append(el('div', 'trace-item-note', 'Pick a second trace in the list to diff it against the highlighted one.'));
  const cancel = el('button', 'trace-compare', 'Cancel');
  cancel.type = 'button';
  cancel.addEventListener('click', cancelTraceCompare);
  box.append(cancel);
  detail.append(box);
  $('#tracesSub').textContent = 'compare · choose the second trace';
}

function cancelTraceCompare() {
  const base = state.traces.compareBase;
  state.traces.compareBase = null;
  renderTraceList();
  updateTracesSub();
  if (base) selectTrace(base);
}

async function selectTraceCompare(aID, bID) {
  state.traces.selected = bID;
  renderTraceList();
  const detail = $('#tracesDetail');
  detail.innerHTML = '';
  detail.append(el('div', 'stats-empty', 'Comparing traces…'));
  try {
    const cmp = await api(
      `/api/traces/compare?a=${encodeURIComponent(aID)}&b=${encodeURIComponent(bID)}`
    );
    if (state.traces.compareBase !== aID || state.traces.selected !== bID) return;
    renderTraceCompare(cmp);
  } catch (err) {
    if (state.traces.compareBase !== aID || state.traces.selected !== bID) return;
    detail.innerHTML = '';
    detail.append(el('div', 'stats-empty', 'Could not compare traces: ' + err.message));
  }
}

function renderTraceCompare(cmp) {
  const detail = $('#tracesDetail');
  detail.innerHTML = '';
  const head = el('div', 'trace-detail-head');
  const copy = el('div');
  copy.append(el('h3', 'trace-detail-title', 'Compare'));
  copy.append(
    el('span', 'trace-detail-sub', `${cmp.a.title || cmp.a.id} → ${cmp.b.title || cmp.b.id}`)
  );
  head.append(copy);
  const actions = el('div', 'trace-actions');
  const swap = el('button', 'trace-compare', 'Swap A/B');
  swap.type = 'button';
  swap.title = 'Reverse the comparison direction';
  swap.addEventListener('click', () => {
    state.traces.compareBase = cmp.b.id;
    selectTraceCompare(cmp.b.id, cmp.a.id);
  });
  const done = el('button', 'trace-compare', 'Done');
  done.type = 'button';
  done.addEventListener('click', () => {
    const keep = state.traces.selected;
    state.traces.compareBase = null;
    renderTraceList();
    updateTracesSub();
    if (keep) selectTrace(keep);
  });
  actions.append(swap, done);
  head.append(actions);
  detail.append(head);
  $('#tracesSub').textContent = cmp.same_kind
    ? `comparing 2 ${cmp.a.kind === 'eval' ? 'eval runs' : 'live sessions'}`
    : 'comparing 2 runs of different kinds';

  detail.append(renderCompareSides(cmp));
  detail.append(renderCompareMetrics(cmp.metrics));
  const problems = renderCompareProblems(cmp);
  if (problems) detail.append(problems);
  detail.append(renderCompareTools(cmp.tools));
  detail.append(renderCompareEvents(cmp.events));
  if (cmp.prompt && cmp.prompt.changed) detail.append(renderCompareText('Prompt diff', cmp.prompt));
  if (cmp.answer && cmp.answer.changed) detail.append(renderCompareText('Answer diff', cmp.answer));
  detail.scrollTop = 0;
}

function renderCompareSides(cmp) {
  const grid = el('div', 'trace-compare-grid');
  const side = (label, meta) => {
    const card = el('div', 'trace-side');
    const head = el('div', 'trace-side-head');
    head.append(el('span', 'trace-side-label', label));
    head.append(el('span', 'trace-status ' + (meta.status || ''), meta.status || 'unknown'));
    card.append(head);
    card.append(el('div', 'trace-side-title', meta.title || meta.id));
    const passed = meta.passed === true ? 'passed' : meta.passed === false ? 'failed' : '';
    const bits = [
      meta.kind === 'eval' ? 'eval run' : 'live session',
      meta.task,
      meta.mode,
      meta.model,
      passed,
      relTime(meta.created),
    ].filter(Boolean);
    card.append(el('div', 'trace-side-sub', bits.join(' · ')));
    return card;
  };
  grid.append(side('A', cmp.a), side('B', cmp.b));
  return grid;
}

function renderCompareMetrics(metrics) {
  const box = el('div', 'trace-metrics');
  const specs = [
    { label: 'Duration', d: metrics.duration_ms, fmt: (v) => (v ? fmtDuration(v) : '0ms'), delta: (v) => fmtDuration(v) },
    { label: 'Model calls', d: metrics.model_calls, fmt: String, delta: String },
    { label: 'Tool calls', d: metrics.tool_calls, fmt: String, delta: String },
    { label: 'Tokens', d: metrics.total_tokens, fmt: (v) => fmtShort(v), delta: (v) => fmtShort(v) },
    { label: 'Cost', d: metrics.cost_usd, fmt: (v) => fmtCost(v) || '$0', delta: (v) => '$' + Number(v).toFixed(4) },
    { label: 'Events', d: metrics.event_count, fmt: String, delta: String },
  ];
  for (const spec of specs) {
    const card = el('div', 'trace-metric');
    card.append(el('div', 'trace-metric-name', spec.label));
    const values = el('div', 'trace-metric-values');
    values.append(el('span', 'trace-metric-a', spec.fmt(spec.d.a)));
    values.append(el('span', 'trace-metric-arrow', '→'));
    values.append(el('span', 'trace-metric-b', spec.fmt(spec.d.b)));
    card.append(values);
    const delta = Number(spec.d.delta) || 0;
    const tone = delta === 0 ? 'same' : delta < 0 ? 'better' : 'worse';
    const text = delta === 0 ? 'no change' : (delta > 0 ? '+' : '-') + spec.delta(Math.abs(delta));
    card.append(el('span', 'trace-metric-delta ' + tone, text));
    box.append(card);
  }
  return box;
}

function renderCompareProblems(cmp) {
  const groups = [
    { title: 'Fixed in B', items: cmp.failures.only_a, tone: 'ok' },
    { title: 'New in B', items: cmp.failures.only_b, tone: 'bad' },
    { title: 'Still failing', items: cmp.failures.same, tone: 'same' },
    { title: 'Warnings only in A', items: cmp.warnings.only_a, tone: 'warn' },
    { title: 'Warnings only in B', items: cmp.warnings.only_b, tone: 'warn' },
    { title: 'Warnings in both', items: cmp.warnings.same, tone: 'same' },
  ].filter((group) => (group.items || []).length);
  if (!groups.length) return null;
  const block = el('div', 'trace-compare-section');
  block.append(el('div', 'trace-section-label', 'Problems'));
  for (const group of groups) {
    const box = el('div', 'trace-problem-group ' + group.tone);
    box.append(el('div', 'trace-problem-title', group.title));
    for (const item of group.items) box.append(el('div', 'trace-problem', item));
    block.append(box);
  }
  return block;
}

function renderCompareTools(tools) {
  const block = el('div', 'trace-compare-section');
  block.append(
    el(
      'div',
      'trace-section-label',
      `Tool sequence · ${tools.same} same · ${tools.added} added · ${tools.removed} removed`
    )
  );
  const list = el('div', 'trace-tool-diff');
  const glyph = { same: '=', added: '+', removed: '-' };
  for (const step of tools.steps || []) {
    const astep = step.a;
    const bstep = step.b;
    const shown = step.op === 'removed' ? astep : bstep;
    if (!shown) continue;
    const row = el('div', 'trace-tool-row ' + step.op);
    row.append(el('span', 'trace-tool-op', glyph[step.op] || '?'));
    row.append(el('span', 'trace-tool-name', shown.name));
    if (shown.depth) row.append(el('span', 'trace-sub', 'depth ' + shown.depth));
    if (shown.failed) row.append(el('span', 'trace-item-flag', 'failed'));
    if (step.op === 'same') {
      const aAt = fmtDuration(astep.offset_ms || 0);
      const bAt = fmtDuration(bstep.offset_ms || 0);
      row.append(el('span', 'trace-tool-offset', `${aAt} → ${bAt}`));
    } else {
      row.append(el('span', 'trace-tool-offset', '@' + fmtDuration(shown.offset_ms || 0)));
    }
    list.append(row);
  }
  if (!list.children.length) list.append(el('div', 'stats-empty', 'No tool calls in either trace.'));
  block.append(list);
  return block;
}

function renderCompareEvents(counts) {
  const block = el('div', 'trace-compare-section');
  block.append(el('div', 'trace-section-label', 'Event counts'));
  const chips = el('div', 'trace-event-counts');
  for (const count of counts || []) {
    const delta = (Number(count.b) || 0) - (Number(count.a) || 0);
    const chip = el('span', 'trace-count-chip' + (delta > 0 ? ' up' : delta < 0 ? ' down' : ''));
    chip.append(el('span', 'trace-count-type', count.type));
    chip.append(el('span', 'trace-count-num', `${count.a} → ${count.b}`));
    if (delta) chip.append(el('span', 'trace-count-delta', (delta > 0 ? '+' : '') + delta));
    chips.append(chip);
  }
  if (!chips.children.length) chips.append(el('div', 'stats-empty', 'No events in either trace.'));
  block.append(chips);
  return block;
}

function renderCompareText(title, td) {
  const block = el('div', 'trace-compare-section');
  const details = el('details', 'trace-text-diff');
  const changed = (td.lines || []).filter((line) => line.op !== ' ').length;
  details.append(
    el('summary', '', `${title} · ${changed} changed line${changed === 1 ? '' : 's'}`)
  );
  const pre = el('pre', 'diff');
  for (const line of td.lines || []) {
    const cls = line.op === '+' ? 'add' : line.op === '-' ? 'del' : '';
    pre.append(el('span', cls, line.op + line.text + '\n'));
  }
  details.append(pre);
  if (td.truncated) details.append(el('div', 'trace-item-note', 'Diff truncated to the first 400 lines.'));
  block.append(details);
  return block;
}

function renderTraceDetail(tr) {
  const detail = $('#tracesDetail');
  detail.innerHTML = '';
  const head = el('div', 'trace-detail-head');
  const copy = el('div');
  copy.append(el('h3', 'trace-detail-title', tr.title || tr.id));
  const sub = [tr.kind === 'eval' ? 'eval run' : 'live session', tr.task, tr.status]
    .filter(Boolean)
    .join(' · ');
  copy.append(el('span', 'trace-detail-sub', sub));
  head.append(copy);
  const actions = el('div', 'trace-actions');
  const compare = el('button', 'trace-compare', 'Compare');
  compare.type = 'button';
  compare.title = 'Compare this trace with another';
  compare.addEventListener('click', () => startTraceCompare(tr.id));
  const del = el('button', 'trace-delete', 'Delete');
  del.type = 'button';
  del.title = 'Delete this trace';
  del.addEventListener('click', () => deleteTrace(tr));
  actions.append(compare, del);
  head.append(actions);
  detail.append(head);
  detail.append(renderTraceMeta(tr));

  const failures = tr.failures || [];
  const warnings = tr.warnings || [];
  if (failures.length || warnings.length) {
    const problems = el('div', 'trace-problems');
    for (const f of failures) problems.append(el('div', 'trace-problem', f));
    for (const w of warnings) problems.append(el('div', 'trace-problem warn', w));
    detail.append(problems);
  }

  if (tr.prompt) {
    const block = el('div', 'trace-block');
    block.append(el('div', 'trace-block-title', 'Prompt'));
    block.append(el('pre', '', tr.prompt));
    detail.append(block);
  }
  if (tr.verifier) detail.append(renderTraceVerifier(tr.verifier));

  const events = tr.events || [];
  detail.append(el('div', 'trace-section-label', events.length + ' event' + (events.length === 1 ? '' : 's')));
  const timeline = el('div', 'trace-timeline');
  renderTraceTimeline(timeline, events, tr.created);
  detail.append(timeline);
  detail.scrollTop = 0;
}

function renderTraceMeta(tr) {
  const grid = el('div', 'trace-meta');
  const add = (label, value) => {
    if (value === undefined || value === null || value === '') return;
    const cell = el('div', 'trace-meta-cell');
    cell.append(el('span', 'trace-meta-label', label));
    const v = el('span', 'trace-meta-value', String(value));
    v.title = String(value);
    cell.append(v);
    grid.append(cell);
  };
  add('Created', tr.created ? new Date(tr.created).toLocaleString() : '');
  add('Duration', tr.duration_ms ? fmtDuration(tr.duration_ms) : '');
  add('Provider', tr.provider);
  add('Model', tr.model);
  add('Mode', tr.mode);
  add('Workspace', compactPath(tr.cwd));
  add('Session', tr.session_id);
  add('Model calls', tr.model_calls);
  add('Tool calls', tr.tool_calls);
  add('Tokens', tr.total_tokens ? fmtShort(tr.total_tokens) : '');
  add('Cost', tr.cost_usd ? fmtCost(tr.cost_usd) : '');
  add('Tags', (tr.tags || []).join(', '));
  return grid;
}

function renderTraceVerifier(v) {
  const block = el('div', 'trace-block');
  block.append(el('div', 'trace-block-title', 'Verifier · exit ' + (v.exit_code ?? 0)));
  if (v.command) block.append(el('pre', '', '$ ' + v.command));
  if (v.output) block.append(el('pre', '', v.output));
  return block;
}

function renderTraceTimeline(box, events, started) {
  const startMs = new Date(started).getTime();
  const toolCards = new Map();
  for (const ev of events) {
    const node = renderTraceEvent(ev, toolCards);
    if (!node) continue;
    const row = el('div', 'trace-event');
    row.append(el('div', 'trace-time', traceOffset(ev, startMs)));
    const body = el('div');
    body.append(node);
    row.append(body);
    box.append(row);
  }
  if (!box.children.length) box.append(el('div', 'stats-empty', 'No events in this trace.'));
}

function traceOffset(ev, startMs) {
  const t = new Date(ev && ev.time).getTime();
  if (!isFinite(t) || !isFinite(startMs)) return '';
  const delta = t - startMs;
  if (delta < 0) return '';
  if (delta < 1000) return '+' + delta + 'ms';
  if (delta < 60000) return '+' + (delta / 1000).toFixed(1) + 's';
  return '+' + Math.floor(delta / 60000) + 'm' + Math.round((delta % 60000) / 1000) + 's';
}

function markdownDiv(text) {
  const div = el('div', 'md');
  div.innerHTML = renderMarkdown(text);
  return div;
}

function traceItemHead(glyph, name, summary) {
  const head = el('div', 'trace-item-head');
  head.append(el('span', 'trace-item-glyph', glyph));
  head.append(el('span', 'trace-item-name', name));
  if (summary) head.append(el('span', 'trace-item-summary', summary));
  return head;
}

function traceDetails(label, body) {
  const details = el('details');
  details.append(el('summary', '', label));
  details.append(el('pre', '', String(body)));
  return details;
}

function traceDiffDetails(label, diff) {
  const details = el('details');
  details.append(el('summary', '', label));
  const pre = el('pre', 'diff');
  for (const line of String(diff).split('\n')) {
    let cls = '';
    if (line.startsWith('+') && !line.startsWith('+++')) cls = 'add';
    else if (line.startsWith('-') && !line.startsWith('---')) cls = 'del';
    pre.append(el('span', cls, line + '\n'));
  }
  details.append(pre);
  return details;
}

function renderTraceEvent(ev, toolCards) {
  switch (ev.type) {
    case 'user': {
      const item = el('div', 'trace-item user');
      item.append(traceItemHead('›', 'user'));
      if (ev.text) item.append(markdownDiv(ev.text));
      return item;
    }
    case 'text': {
      if (!ev.text) return null;
      const item = el('div', 'trace-item');
      if (ev.depth) {
        const head = traceItemHead('✎', 'subagent text');
        head.append(el('span', 'trace-sub', 'depth ' + ev.depth));
        item.append(head);
      }
      item.append(markdownDiv(ev.text));
      return item;
    }
    case 'reasoning': {
      if (!ev.text) return null;
      const item = el('div', 'trace-item');
      item.append(traceDetails('reasoning', ev.text));
      return item;
    }
    case 'tool_start':
      return traceToolCard(ev, toolCards);
    case 'tool_end':
      return traceToolResult(ev, toolCards);
    case 'diff': {
      if (!ev.diff) return null;
      const item = el('div', 'trace-item');
      item.append(traceItemHead('±', 'diff', ev.path || ''));
      item.append(traceDiffDetails('changes', ev.diff));
      return item;
    }
    default:
      return renderTraceOther(ev);
  }
}

function traceToolCard(ev, toolCards) {
  const args = parseMaybe(ev.args);
  const item = el('div', 'trace-item');
  const head = traceItemHead(toolGlyph(ev.name), ev.name || 'tool', summarizeArgs(ev.name, args));
  if (ev.depth) head.append(el('span', 'trace-sub', 'depth ' + ev.depth));
  item.append(head);
  if (args && typeof args === 'object' && Object.keys(args).length) {
    item.append(traceDetails('arguments', prettyArgs(args)));
  }
  if (ev.call_id) toolCards.set(ev.call_id, item);
  return item;
}

function traceToolResult(ev, toolCards) {
  const card = ev.call_id ? toolCards.get(ev.call_id) : null;
  const target = card || el('div', 'trace-item');
  if (!card) target.append(traceItemHead(toolGlyph(ev.name), ev.name || 'tool', 'result'));
  if (ev.is_error) {
    target.classList.add('error');
    const head = target.querySelector('.trace-item-head');
    if (head) head.append(el('span', 'trace-item-flag', 'failed'));
  }
  if (ev.output) target.append(traceDetails('output', ev.output));
  if (ev.diff) target.append(traceDiffDetails('diff', ev.diff));
  return null;
}

function traceNote(badge, text, tone) {
  const item = el('div', 'trace-item' + (tone ? ' ' + tone : ''));
  if (badge) {
    const head = el('div', 'trace-item-head');
    head.append(el('span', 'trace-badge', badge));
    item.append(head);
  }
  item.append(el('div', 'trace-item-note', text || ''));
  return item;
}

function traceUsage(usage, cost) {
  const bits = [];
  if (usage) {
    if (usage.total_tokens) bits.push(fmtShort(usage.total_tokens) + ' tokens');
    const io = [];
    if (usage.prompt_tokens) io.push('↑' + fmtShort(usage.prompt_tokens));
    if (usage.completion_tokens) io.push('↓' + fmtShort(usage.completion_tokens));
    if (io.length) bits.push(io.join(' '));
  }
  if (cost) bits.push(fmtCost(cost));
  return bits.join(' · ');
}

function renderTraceOther(ev) {
  switch (ev.type) {
    case 'session':
      return traceNote('session', ev.message || 'started');
    case 'compact':
      return traceNote('compact', ev.message || 'context compacted');
    case 'usage': {
      const text = traceUsage(ev.usage, ev.cost_usd);
      return text ? traceNote('usage', text) : null;
    }
    case 'approval_request': {
      const item = el('div', 'trace-item');
      const head = traceItemHead('?', ev.name || 'approval');
      head.append(el('span', 'trace-badge approval', 'approval'));
      item.append(head);
      if (ev.message) item.append(el('div', 'trace-item-note', ev.message));
      const args = parseMaybe(ev.args);
      if (args && typeof args === 'object' && Object.keys(args).length) {
        item.append(traceDetails('request', prettyArgs(args)));
      }
      return item;
    }
    case 'approval_result':
      return traceNote('approval', ev.message || (ev.is_error ? 'denied' : 'approved'), ev.is_error ? 'warn' : '');
    case 'subagent_start': {
      const summary = ev.message || summarizeArgs('agent', parseMaybe(ev.args));
      return traceNote('subagent', summary || 'started');
    }
    case 'subagent_end':
      return traceNote('subagent', ev.message || 'finished', ev.is_error ? 'error' : 'ok');
    case 'hook':
      return traceNote('hook', (ev.name ? ev.name + ' · ' : '') + (ev.message || ''), ev.is_error ? 'warn' : '');
    case 'verify_start':
      return traceNote('verify', ev.message || 'verification started');
    case 'verify': {
      const item = el('div', 'trace-item' + (ev.is_error ? ' error' : ' ok'));
      item.append(traceItemHead('✓', ev.message || 'verify'));
      if (ev.output) item.append(traceDetails('output', ev.output));
      return item;
    }
    case 'retry': {
      const r = ev.retry || {};
      const when = r.max ? `attempt ${r.attempt}/${r.max}` : '';
      const delay = r.delay_ms ? ' in ' + formatDelay(r.delay_ms) : '';
      const msg = [when + delay, r.message].filter(Boolean).join(' · ');
      return traceNote('retry', msg || ev.message || 'retrying', 'warn');
    }
    case 'diagnostics': {
      const list = ev.annotations || [];
      if (!list.length) return null;
      const item = el('div', 'trace-item' + (list.some((d) => /error/i.test(d.severity || '')) ? ' warn' : ''));
      item.append(traceItemHead('⚑', 'diagnostics', list.length + ' finding' + (list.length === 1 ? '' : 's')));
      for (const d of list) {
        const where = d.path ? compactPath(d.path) + (d.line ? ':' + d.line : '') : '';
        item.append(el('div', 'trace-item-note', [d.severity, where, d.message].filter(Boolean).join(' · ')));
      }
      return item;
    }
    case 'todos': {
      const todos = ev.todos || [];
      if (!todos.length) return null;
      const item = el('div', 'trace-item');
      item.append(traceItemHead('☰', 'plan'));
      for (const todo of todos) {
        const mark = todo.status === 'completed' ? '✓' : todo.status === 'in_progress' ? '▸' : '·';
        item.append(el('div', 'trace-item-note', mark + ' ' + todo.text));
      }
      return item;
    }
    case 'done': {
      const text = [ev.message || 'done', traceUsage(ev.total_usage, ev.total_cost)].filter(Boolean).join(' · ');
      return traceNote('done', text, 'ok');
    }
    case 'error':
      return traceNote('error', ev.message || 'error', 'error');
    case 'assistant_start':
    case 'saved':
      return null;
    default:
      return ev.type ? traceNote(ev.type, ev.message || '') : null;
  }
}

async function api(path, opts = {}) {
  const options = { ...opts, headers: { 'Content-Type': 'application/json', ...(opts.headers || {}) } };
  const res = await fetch(path, options);
  if (res.status === 401) {
    location.assign('/login');
    throw new Error('authentication required');
  }
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
