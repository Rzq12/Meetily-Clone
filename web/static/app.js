'use strict';

const App = (() => {
  const csrf = document.body.dataset.csrf || '';

  // api returns {ok, status, data}; a 401 means the session ended, so go back to login.
  async function api(method, url, body, opts = {}) {
    const headers = { 'X-CSRF-Token': csrf };
    let payload = body;
    if (body !== undefined && !(body instanceof FormData) && !(body instanceof Blob)) {
      headers['Content-Type'] = 'application/json';
      payload = JSON.stringify(body);
    }
    let res;
    try {
      res = await fetch(url, { method, headers, body: payload, credentials: 'same-origin', signal: opts.signal });
    } catch (err) {
      if (err.name === 'AbortError') throw err;
      return { ok: false, status: 0, data: { error: 'Tidak dapat terhubung ke server.' } };
    }
    if (res.status === 401) {
      window.location.href = '/login?expired=1';
      return { ok: false, status: 401, data: {} };
    }
    const text = await res.text();
    let data = {};
    try { data = text ? JSON.parse(text) : {}; } catch (e) { data = { error: text || 'Terjadi kesalahan pada server.' }; }
    return { ok: res.ok, status: res.status, data };
  }

  function el(tag, attrs = {}, children = []) {
    const node = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) {
      if (k === 'class') node.className = v;
      else if (k === 'text') node.textContent = v;
      else if (k.startsWith('on')) node.addEventListener(k.slice(2), v);
      else if (v !== false && v !== null && v !== undefined) node.setAttribute(k, v === true ? '' : v);
    }
    for (const c of [].concat(children)) if (c) node.append(c);
    return node;
  }

  function iconFromTemplate(kind) {
    const tpl = document.getElementById('tpl-toast-icons');
    const holder = tpl && tpl.content.querySelector(`[data-kind="${kind}"]`);
    return holder ? holder.firstElementChild.cloneNode(true) : null;
  }

  // Sonner-style toast, bottom-center, richColors, close button.
  function toast(kind, message, duration = 4000) {
    const box = document.getElementById('toaster');
    if (!box) return;
    const close = el('button', { type: 'button', class: 'toast-close', 'aria-label': 'Tutup' }, iconFromTemplate('close'));
    const node = el('div', { class: `toast toast-${kind}`, role: kind === 'error' ? 'alert' : 'status' },
      [close, iconFromTemplate(kind), el('div', { class: 'toast-title', text: message })]);
    const remove = () => node.remove();
    close.addEventListener('click', remove);
    box.append(node);
    setTimeout(remove, duration);
  }

  // Flash a toast on the next page, used before a redirect.
  function flash(kind, message) {
    try { sessionStorage.setItem('flash', JSON.stringify({ kind, message })); } catch (e) {}
  }

  function showFlash() {
    try {
      const raw = sessionStorage.getItem('flash');
      if (!raw) return;
      sessionStorage.removeItem('flash');
      const f = JSON.parse(raw);
      toast(f.kind, f.message);
    } catch (e) {}
  }

  // Dialog in the shadcn style. content is a node; returns {root, close}.
  function dialog({ title, description, content, footer, onClose }) {
    const previous = document.activeElement;
    const overlay = el('div', { class: 'dialog-overlay' });
    const closeBtn = el('button', { type: 'button', class: 'dialog-close', 'aria-label': 'Tutup' }, iconFromTemplate('close'));
    const box = el('div', { class: 'dialog', role: 'dialog', 'aria-modal': 'true' }, [
      el('div', { class: 'dialog-header' }, [
        el('h2', { class: 'dialog-title' }, title),
        description ? el('p', { class: 'dialog-description', text: description }) : null,
      ]),
      content || null,
      footer ? el('div', { class: 'dialog-footer' }, footer) : null,
      closeBtn,
    ]);
    const root = el('div', {}, [overlay, box]);
    function close() {
      root.remove();
      document.removeEventListener('keydown', onKey);
      if (onClose) onClose();
      if (previous && previous.focus) previous.focus();
    }
    function onKey(e) { if (e.key === 'Escape') close(); }
    overlay.addEventListener('click', close);
    closeBtn.addEventListener('click', close);
    document.addEventListener('keydown', onKey);
    document.body.append(root);
    const first = box.querySelector('input, textarea, select, button:not(.dialog-close)');
    if (first) first.focus();
    return { root, close };
  }

  // Confirmation in Meetily's ConfirmationModal wording, resolving true on confirm.
  function confirm({ title, text, confirmLabel = 'Lanjutkan', destructive = false }) {
    return new Promise((resolve) => {
      let answered = false;
      const cancel = el('button', { type: 'button', class: 'btn btn-outline', text: 'Batal' });
      const ok = el('button', { type: 'button', class: `btn ${destructive ? 'btn-destructive' : 'btn-default'}`, text: confirmLabel });
      const d = dialog({
        title: [title], description: text, footer: [cancel, ok],
        onClose: () => { if (!answered) resolve(false); },
      });
      cancel.addEventListener('click', () => d.close());
      ok.addEventListener('click', () => { answered = true; d.close(); resolve(true); });
    });
  }

  function setupDropdowns() {
    document.addEventListener('click', (e) => {
      const trigger = e.target.closest('[data-dropdown-trigger]');
      document.querySelectorAll('.dropdown-content').forEach((menu) => {
        if (!trigger || menu.parentElement !== trigger.parentElement) menu.hidden = true;
      });
      if (trigger) {
        const menu = trigger.parentElement.querySelector('.dropdown-content');
        menu.hidden = !menu.hidden;
        trigger.setAttribute('aria-expanded', String(!menu.hidden));
      }
    });
  }

  function setupSidebar() {
    const root = document.documentElement;
    const toggle = document.getElementById('sidebar-toggle');
    const setCollapsed = (collapsed) => {
      root.classList.toggle('sidebar-collapsed', collapsed);
      try { localStorage.setItem('sidebarCollapsed', collapsed ? '1' : '0'); } catch (e) {}
    };
    if (toggle) toggle.addEventListener('click', () => setCollapsed(!root.classList.contains('sidebar-collapsed')));
    document.querySelectorAll('[data-expand-sidebar]').forEach((b) => b.addEventListener('click', () => setCollapsed(false)));

    const list = document.getElementById('sidebar-meetings');
    const search = document.getElementById('sidebar-search');
    const clear = document.getElementById('sidebar-search-clear');
    const searching = document.getElementById('sidebar-searching');
    const tpl = document.getElementById('tpl-sidebar-item');
    if (!list || !tpl) return;
    const active = (document.body.dataset.active || '').replace('meeting:', '');

    let controller;
    async function load(q) {
      if (controller) controller.abort();
      controller = new AbortController();
      searching.hidden = !q;
      let res;
      try {
        res = await api('GET', '/api/meetings?q=' + encodeURIComponent(q), undefined, { signal: controller.signal });
      } catch (e) {
        return;
      }
      searching.hidden = true;
      list.replaceChildren();
      if (!res.ok) {
        list.append(el('p', { class: 'px-3 py-2 text-xs text-red-600', text: res.data.error || 'Gagal memuat rapat.' }));
        return;
      }
      if (res.data.items.length === 0) {
        list.append(el('p', { class: 'px-3 py-2 text-xs text-gray-500', text: q ? 'Belum ada rapat yang cocok.' : 'Belum ada rapat.' }));
        return;
      }
      for (const m of res.data.items) {
        const node = tpl.content.firstElementChild.cloneNode(true);
        node.href = '/meetings/' + m.id;
        node.querySelector('[data-title]').textContent = m.title;
        node.classList.add(...(m.id === active ? ['bg-blue-100', 'text-blue-700', 'font-medium'] : ['hover:bg-gray-50']));
        list.append(node);
      }
    }

    let timer;
    search.addEventListener('input', () => {
      clear.hidden = !search.value;
      clearTimeout(timer);
      timer = setTimeout(() => load(search.value.trim()), 300);
    });
    clear.addEventListener('click', () => { search.value = ''; clear.hidden = true; load(''); search.focus(); });
    load('');
  }

  function setupLogout() {
    document.querySelectorAll('[data-logout]').forEach((b) => b.addEventListener('click', async () => {
      const res = await api('POST', '/api/auth/logout');
      window.location.href = (res.data && res.data.redirect) || '/login';
    }));
  }

  // Meetily's formatRecordingTime style, used for elapsed time and durations.
  function clock(totalSec) {
    const s = Math.floor(totalSec);
    const pad = (n) => String(n).padStart(2, '0');
    return `${pad(Math.floor(s / 3600))}:${pad(Math.floor((s % 3600) / 60))}:${pad(s % 60)}`;
  }

  function duration(sec) {
    if (sec === null || sec === undefined) return '-';
    const pad = (n) => String(n).padStart(2, '0');
    return sec >= 3600 ? `${Math.floor(sec / 3600)}:${pad(Math.floor((sec % 3600) / 60))}:${pad(sec % 60)}` : `${Math.floor(sec / 60)}:${pad(sec % 60)}`;
  }

  function showFieldErrors(form, fields) {
    form.querySelectorAll('[aria-invalid]').forEach((n) => n.removeAttribute('aria-invalid'));
    form.querySelectorAll('.field-error').forEach((n) => n.remove());
    for (const [name, msg] of Object.entries(fields || {})) {
      const input = form.querySelector(`[name="${name}"]`);
      if (!input) continue;
      input.setAttribute('aria-invalid', 'true');
      input.insertAdjacentElement('afterend', el('p', { class: 'field-error', text: msg }));
    }
  }

  // Participant picker: search active users by name, show selected ones as chips.
  // excludeId is the owner, who is never a participant (FR-013).
  function participantPicker(root, users, selectedIds, excludeId) {
    const selected = new Set(selectedIds);
    const input = el('input', { class: 'input', type: 'search', placeholder: 'Cari nama pengguna...', autocomplete: 'off', 'aria-label': 'Cari peserta' });
    const listBox = el('div', { class: 'picker-list', role: 'listbox', hidden: true });
    const chips = el('div', { class: 'flex flex-wrap gap-2 mt-2' });
    const xIcon = () => iconFromTemplate('close');
    const byId = new Map(users.map((u) => [u.id, u]));

    function renderChips() {
      chips.replaceChildren(...[...selected].filter((id) => byId.has(id)).map((id) => el('span', { class: 'chip' }, [
        byId.get(id).full_name,
        el('button', { type: 'button', 'aria-label': 'Hapus ' + byId.get(id).full_name, onclick: () => { selected.delete(id); renderChips(); } }, xIcon()),
      ])));
      if (selected.size === 0) chips.append(el('span', { class: 'text-xs text-gray-500', text: 'Belum ada peserta.' }));
    }

    function renderList() {
      const q = input.value.trim().toLowerCase();
      const matches = users.filter((u) => u.id !== excludeId && !selected.has(u.id) && u.full_name.toLowerCase().includes(q)).slice(0, 50);
      listBox.replaceChildren(...(matches.length ? matches.map((u) => el('button', {
        type: 'button', class: 'dropdown-item', role: 'option',
        onclick: () => { selected.add(u.id); input.value = ''; listBox.hidden = true; renderChips(); input.focus(); },
      }, [el('span', { text: u.full_name }), el('span', { class: 'text-xs text-gray-500', text: { admin: 'Admin', notetaker: 'Notulis', viewer: 'Pembaca' }[u.role] })]))
        : [el('p', { class: 'px-2 py-1.5 text-sm text-gray-500', text: 'Tidak ada pengguna yang cocok.' })]));
      listBox.hidden = false;
    }

    input.addEventListener('input', renderList);
    input.addEventListener('focus', renderList);
    document.addEventListener('click', (e) => { if (!root.contains(e.target)) listBox.hidden = true; });
    root.replaceChildren(el('div', { class: 'relative' }, [input, listBox]), chips);
    renderChips();
    return { values: () => [...selected] };
  }

  document.addEventListener('DOMContentLoaded', () => {
    setupSidebar();
    setupLogout();
    setupDropdowns();
    showFlash();
  });

  return { api, el, toast, flash, dialog, confirm, clock, duration, showFieldErrors, participantPicker, csrf };
})();
