(() => {
  'use strict';

  // The page lives at the chat's base path, e.g. "/" or "/chat/".
  const base = location.pathname.replace(/[^/]*$/, '');
  const $ = (id) => document.getElementById(id);
  const el = (tag, cls) => { const e = document.createElement(tag); if (cls) e.className = cls; return e; };

  let cfg = null;
  let t = {};
  let source = null;
  let typingTimer = null;

  // ---- safe rich text: everything is built from DOM nodes, never from HTML strings ----

  function link(url, label) {
    const a = el('a');
    a.textContent = label;
    try {
      const u = new URL(url);
      if (u.protocol === 'http:' || u.protocol === 'https:') {
        a.href = u.href;
        a.target = '_blank';
        a.rel = 'noopener noreferrer nofollow';
      }
    } catch (_) { /* not a valid URL: stays plain text */ }
    return a;
  }

  const inlineRe = /`([^`\n]+)`|\*\*([^*\n]+)\*\*|\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)|(https?:\/\/[^\s<>]+)/g;

  function inline(parent, s) {
    inlineRe.lastIndex = 0;
    let last = 0;
    let m;
    while ((m = inlineRe.exec(s))) {
      let end = inlineRe.lastIndex;
      if (m.index > last) parent.append(document.createTextNode(s.slice(last, m.index)));
      if (m[1] !== undefined) {
        const c = el('code'); c.textContent = m[1]; parent.append(c);
      } else if (m[2] !== undefined) {
        const b = el('strong'); b.textContent = m[2]; parent.append(b);
      } else if (m[4] !== undefined) {
        parent.append(link(m[4], m[3]));
      } else {
        // a bare URL: keep trailing punctuation out of the link
        let url = m[5];
        const trail = url.match(/[.,;:!?)\]'"]+$/);
        if (trail) {
          url = url.slice(0, url.length - trail[0].length);
          end -= trail[0].length;
          inlineRe.lastIndex = end;
        }
        parent.append(link(url, url));
      }
      last = end;
    }
    if (last < s.length) parent.append(document.createTextNode(s.slice(last)));
  }

  const itemRe = /^\s*(?:[-*•]|\d+[.)])\s+(.*)$/;

  function rich(parent, text) {
    const lines = text.replace(/\r\n?/g, '\n').split('\n');
    let para = [];
    const flush = () => {
      if (!para.length) return;
      const p = el('p');
      inline(p, para.join('\n'));
      parent.append(p);
      para = [];
    };
    let i = 0;
    while (i < lines.length) {
      const line = lines[i];
      if (/^```/.test(line)) {
        flush();
        const code = [];
        i++;
        while (i < lines.length && !/^```\s*$/.test(lines[i])) code.push(lines[i++]);
        i++;
        const pre = el('pre');
        const c = el('code');
        c.textContent = code.join('\n');
        pre.append(c);
        parent.append(pre);
        continue;
      }
      if (itemRe.test(line)) {
        flush();
        const list = el(/^\s*\d/.test(line) ? 'ol' : 'ul');
        let m;
        while (i < lines.length && (m = lines[i].match(itemRe))) {
          const li = el('li');
          inline(li, m[1]);
          list.append(li);
          i++;
        }
        parent.append(list);
        continue;
      }
      const h = line.match(/^#{1,6}\s+(.*)$/);
      if (h) {
        flush();
        const p = el('p', 'h');
        inline(p, h[1]);
        parent.append(p);
        i++;
        continue;
      }
      if (line.trim() === '') flush(); else para.push(line);
      i++;
    }
    flush();
  }

  // ---- UI ----

  function addMessage(role, text, files) {
    const box = el('div', 'msg ' + role);
    if (role === 'bot') rich(box, text); else box.textContent = text;
    if (files && files.length) {
      const f = el('span', 'files');
      f.textContent = '\u{1F4CE} ' + files.join(', ');
      box.append(f);
    }
    const list = $('messages');
    const stick = list.scrollHeight - list.scrollTop - list.clientHeight < 80;
    list.append(box);
    if (stick || role === 'user') list.scrollTop = list.scrollHeight;
  }

  function addError(text) { addMessage('error', text); }

  function setStatus(text) { $('status').textContent = text || ''; }

  function showTyping() {
    setStatus(t.web_typing);
    clearTimeout(typingTimer);
    typingTimer = setTimeout(() => setStatus(''), 6000);
  }

  function hideTyping() { clearTimeout(typingTimer); setStatus(''); }

  async function api(method, path, body) {
    const res = await fetch(base + path, {
      method,
      credentials: 'same-origin',
      headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    let data = null;
    try { data = await res.json(); } catch (_) { /* no body */ }
    return { ok: res.ok, status: res.status, data };
  }

  function applyStrings() {
    document.documentElement.lang = cfg.lang || 'en';
    document.title = cfg.title;
    $('title').textContent = cfg.title;
    $('input').placeholder = t.web_placeholder || '';
    $('input').setAttribute('aria-label', t.web_placeholder || '');
    $('send').textContent = t.web_send || 'Send';
    $('reset').textContent = t.web_new_chat || 'New chat';
    $('login-prompt').textContent = t.web_code_prompt || '';
    $('login-submit').textContent = t.web_code_submit || 'OK';
    $('code').setAttribute('aria-label', t.web_code_prompt || '');
  }

  function showLogin() {
    $('chat').hidden = true;
    $('reset').hidden = true;
    $('login').hidden = false;
    $('code').focus();
  }

  async function showChat() {
    $('login').hidden = true;
    $('chat').hidden = false;
    $('reset').hidden = false;
    const res = await api('GET', 'api/history');
    if (res.ok && res.data && Array.isArray(res.data.messages)) {
      for (const m of res.data.messages) addMessage(m.role === 'user' ? 'user' : 'bot', m.text, m.attachments);
    }
    connect();
    $('input').focus();
  }

  function connect() {
    if (source) source.close();
    source = new EventSource(base + 'api/events');
    source.onopen = () => { if ($('status').dataset.conn) { setStatus(''); delete $('status').dataset.conn; } };
    source.addEventListener('message', (ev) => {
      hideTyping();
      try { addMessage('bot', JSON.parse(ev.data).text); } catch (_) { /* ignore malformed event */ }
    });
    source.addEventListener('typing', showTyping);
    source.onerror = () => {
      // The browser retries by itself; a closed stream means the server refused us.
      if (source.readyState === EventSource.CLOSED) {
        setTimeout(init, 5000);
      } else {
        $('status').dataset.conn = '1';
        setStatus(t.web_reconnecting);
      }
    };
  }

  // Files chosen for the next message.
  let pending = [];

  function renderFiles() {
    const list = $('files');
    list.replaceChildren();
    for (const [i, f] of pending.entries()) {
      const li = el('li');
      const name = el('span', 'name');
      name.textContent = f.name;
      const rm = el('button');
      rm.type = 'button';
      rm.textContent = '\u00d7';
      rm.title = t.web_attach_remove || 'Remove';
      rm.setAttribute('aria-label', (t.web_attach_remove || 'Remove') + ' ' + f.name);
      rm.addEventListener('click', () => { pending.splice(i, 1); renderFiles(); });
      li.append(name, rm);
      list.append(li);
    }
    list.hidden = pending.length === 0;
  }

  function addFiles(files) {
    for (const f of files) {
      if (pending.length >= (cfg.maxFiles || 1)) { addError(t.web_attach_too_many || 'Too many files.'); break; }
      if (f.size > cfg.maxFileBytes) { addError(f.name + ': ' + (t.web_attach_too_big || 'Too large.')); continue; }
      pending.push(f);
    }
    renderFiles();
  }

  async function send(text, files) {
    let res;
    if (files && files.length) {
      const form = new FormData();
      form.append('text', text);
      for (const f of files) form.append('files', f, f.name);
      const r = await fetch(base + 'api/send', { method: 'POST', credentials: 'same-origin', body: form });
      let data = null;
      try { data = await r.json(); } catch (_) { /* no body */ }
      res = { ok: r.ok, status: r.status, data };
    } else {
      res = await api('POST', 'api/send', { text });
    }
    if (res.ok) return true;
    if (res.status === 401) { init(); return false; }
    let key = { 413: 'web_too_long', 429: 'web_rate_limited' }[res.status] || 'web_error';
    if (res.status === 413 && res.data && /file/.test(res.data.error || '')) {
      key = /many/.test(res.data.error) ? 'web_attach_too_many' : 'web_attach_too_big';
    }
    addError(t[key] || t.web_error || 'Error');
    return false;
  }

  // Starting over clears the list *before* the command is sent: the server's
  // confirmation can arrive before the request has even returned, and must not be wiped.
  function isReset(text) { return text.toLowerCase() === '/reset'; }

  async function submitMessage() {
    const input = $('input');
    const text = input.value.trim();
    const files = pending;
    if (!text && files.length === 0) return;
    input.value = '';
    pending = [];
    renderFiles();
    autosize();
    if (isReset(text) && files.length === 0) $('messages').replaceChildren();
    else addMessage('user', text, files.map((f) => f.name));
    await send(text, files);
  }

  function autosize() {
    const input = $('input');
    input.style.height = 'auto';
    input.style.height = Math.min(input.scrollHeight, 160) + 'px';
  }

  async function init() {
    const res = await api('GET', 'api/config');
    if (!res.ok) {
      $('chat').hidden = true;
      $('login').hidden = true;
      document.body.textContent = (res.data && res.data.error) || 'Error';
      return;
    }
    cfg = res.data;
    t = cfg.strings || {};
    applyStrings();
    $('input').maxLength = cfg.maxMessage;
    $('attach').hidden = !cfg.attachments;
    $('attach').title = t.web_attach || '';
    $('attach').setAttribute('aria-label', t.web_attach || 'Attach a file');
    if (cfg.needsCode && !cfg.authed) showLogin(); else await showChat();
  }

  $('composer').addEventListener('submit', (e) => { e.preventDefault(); submitMessage(); });
  $('input').addEventListener('input', autosize);
  $('input').addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); submitMessage(); }
  });
  $('attach').addEventListener('click', () => $('file').click());
  $('file').addEventListener('change', () => { addFiles($('file').files); $('file').value = ''; });
  const dropTarget = $('composer');
  for (const ev of ['dragenter', 'dragover']) {
    dropTarget.addEventListener(ev, (e) => {
      if (!cfg || !cfg.attachments || !e.dataTransfer || !Array.from(e.dataTransfer.types).includes('Files')) return;
      e.preventDefault();
      dropTarget.classList.add('drag');
    });
  }
  for (const ev of ['dragleave', 'drop']) dropTarget.addEventListener(ev, () => dropTarget.classList.remove('drag'));
  dropTarget.addEventListener('drop', (e) => {
    if (!cfg || !cfg.attachments || !e.dataTransfer || e.dataTransfer.files.length === 0) return;
    e.preventDefault();
    addFiles(e.dataTransfer.files);
  });
  $('input').addEventListener('paste', (e) => {
    if (!cfg || !cfg.attachments || !e.clipboardData || e.clipboardData.files.length === 0) return;
    e.preventDefault();
    addFiles(e.clipboardData.files);
  });
  $('reset').addEventListener('click', async () => {
    $('messages').replaceChildren();
    await send('/reset');
  });
  $('login').addEventListener('submit', async (e) => {
    e.preventDefault();
    $('login-error').textContent = '';
    const res = await api('POST', 'api/login', { code: $('code').value });
    if (res.ok) { $('code').value = ''; await init(); return; }
    $('login-error').textContent = res.status === 429 ? (t.web_rate_limited || '') : (t.web_code_wrong || '');
  });

  init();
})();
