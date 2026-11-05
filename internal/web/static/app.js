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
  cost: 0,
  plan: false,
  checkpoints: 0,
  files: { open: false, mention: false, path: '', seq: 0 },
  palette: { open: false, items: [], index: 0 },
  images: [],
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
  resizeInput();
  input.focus();
}

function bindUI() {
  $('#sendBtn').addEventListener('click', send);
  $('#stopBtn').addEventListener('click', stopRun);
  $('#newChatBtn').addEventListener('click', newChat);
  $('#refreshBtn').addEventListener('click', refreshSessions);
  $('#themeBtn').addEventListener('click', toggleTheme);
  $('#planChip').addEventListener('click', togglePlan);
  $('#undoBtn').addEventListener('click', undoLastTurn);
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

async function loadSession(id) {
  if (state.running) stopRun();
  try {
    const sess = await api('/api/sessions/' + encodeURIComponent(id));
    state.sessionId = sess.id;
    clearChat();
    renderHistory(sess);
    clearImages();
    setTitle(sess.title || 'Session');
    updateExportButton();
    if (sess.model) $('#modelChip').textContent = sess.model;
    setUsage(sess.usage || null);
    setCheckpoints((sess.checkpoints || []).length);
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
  clearChat();
  clearImages();
  showEmpty();
  setTitle('New session');
  setUsage(null);
  setCheckpoints(0);
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
    clearImages();
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
    await refreshSessionState();
    await refreshSessions();
    input.focus();
  }
}

function stopRun() {
  if (state.abort) state.abort.abort();
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
  $('#filesHint').textContent = mention ? 'Click a file to insert @path' : 'Click a file to insert its path';
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
  try {
    const entries = await api('/api/files/search?q=' + encodeURIComponent(query));
    if (seq !== state.files.seq) return;
    $('#filesPath').textContent = '“' + truncate(query, 40) + '”';
    $('#filesUpBtn').disabled = true;
    renderFileEntries(entries || [], true);
  } catch (err) {
    if (seq === state.files.seq) renderFileMessage('Search failed: ' + err.message);
  }
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
  for (const entry of entries) {
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
    list.append(row);
  }
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
        'Slash commands: /compact, /new, /plan, /undo, /skills, /skill, /open, /export md|json, /sessions, /help',
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
    case 'diagnostics':
      if (ev.is_error) {
        const target = ev.path || ev.name || 'changed file';
        addNote(`Diagnostics failed for ${target}: ${truncate(ev.output || '', 600)}`, 'warn');
      }
      break;
    case 'error':
      if (ev.message && ev.message !== 'context canceled') addErrorCard(ev.message);
      setStatus('error', 'error');
      break;
    case 'done':
      if (ev.message === 'stopped') addNote('Stopped by user', 'warn');
      if (typeof ev.checkpoints === 'number') setCheckpoints(ev.checkpoints);
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

function setUsage(usage, totalCost) {
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
  chip.title =
    `${usage.prompt_tokens || 0} prompt tokens, ${usage.completion_tokens || 0} completion tokens` +
    (cost ? `, estimated ${cost}` : '');
}

function setCheckpoints(count) {
  state.checkpoints = Number(count) || 0;
  const btn = $('#undoBtn');
  if (!btn) return;
  btn.hidden = state.checkpoints === 0;
  btn.title =
    state.checkpoints === 0
      ? 'No file changes to undo'
      : 'Undo file changes from the last turn' + (state.checkpoints > 1 ? ` (${state.checkpoints} checkpoints)` : '');
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
