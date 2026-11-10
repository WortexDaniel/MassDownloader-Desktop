import { el, setStatus } from './main.js';
import { postId } from './contracts.js';
import { dismissOverlay } from './motion.js';
import { copyablePost, copyPostImage } from './image-copy.js';
import { locationLabel, queuedLabel, refreshDownloadState, isPostQueued, submitPostDownload } from './download-state.js';

let dismissMenu;
export async function showPostMenu(post, position, { api, download, openAuthor, currentArtistQuery = '', artistHints = [], canDownload = p => !!p.file, downloadLabel = 'Скачать', downloadPost = post }) {
  dismissMenu?.();
  const menu = el('div', { class: 'post-context-menu', role: 'menu', 'aria-label': 'Действия с постом', tabindex: '-1' });
  const previousFocus = document.activeElement;
  let closed = false;
  const unbind = [];
  const outside = e => { if (!menu.contains(e.target)) close(); };
  function close() {
    if (closed) return;
    closed = true; dismissOverlay(menu);
    document.removeEventListener('pointerdown', outside, true);
    window.removeEventListener('resize', close);
    unbind.forEach(fn => fn?.());
    if (dismissMenu === close) dismissMenu = undefined;
    if (previousFocus?.isConnected) previousFocus.focus();
  }
  dismissMenu = close;
  function place(node, x, y) {
    const bounds = node.getBoundingClientRect?.() || { width: 260, height: 200 };
    node.style.left = `${Math.max(8, Math.min(x || 0, (window.innerWidth || 1280) - bounds.width - 8))}px`;
    node.style.top = `${Math.max(8, Math.min(y || 0, (window.innerHeight || 800) - bounds.height - 8))}px`;
  }
  function item(text, fn, disabled = false) {
    const btn = el('button', { class: 'context-item', role: 'menuitem', type: 'button', text, title: text, disabled, onclick: async () => {
      if (closed || btn.disabled) return;
      close(); try { await fn(); } catch (err) { setStatus(String(err), true); }
    } });
    return btn;
  }
  function separator() { menu.append(el('div', { class: 'context-separator', role: 'separator' })); }
  function group(label, entries) {
    const wrapper = el('div', { class: 'context-group' });
    const head = el('button', { class: 'context-item context-group-head', role: 'menuitem', type: 'button', text: `${label} ›`, 'aria-haspopup': 'menu', 'aria-expanded': 'false' });
    const sub = el('div', { class: 'context-submenu', role: 'menu', hidden: true });
    for (const [text, fn] of entries) sub.append(item(text, fn));
    function show() {
      for (const other of menu.querySelectorAll('.context-submenu')) other.hidden = true;
      for (const other of menu.querySelectorAll('.context-group-head')) other.setAttribute('aria-expanded', 'false');
      sub.hidden = false; head.setAttribute('aria-expanded', 'true');
      const bounds = head.getBoundingClientRect?.() || { right: 260, left: 0, top: 8 };
      const width = sub.getBoundingClientRect?.().width || 260;
      place(sub, bounds.right + width + 8 > (window.innerWidth || 1280) ? bounds.left - width : bounds.right, bounds.top);
    }
    head.addEventListener('click', show); wrapper.addEventListener('mouseenter', show);
    wrapper.addEventListener('mouseleave', () => { sub.hidden = true; head.setAttribute('aria-expanded', 'false'); });
    head.addEventListener('keydown', e => { if (e.key === 'ArrowRight') { e.preventDefault(); e.stopPropagation(); show(); sub.querySelector('button')?.focus(); } });
    wrapper.append(head, sub); menu.append(wrapper);
  }
  const changed = () => window.dispatchEvent(new Event('favorites-changed'));
  const downloadSlot = el('div', { class: 'context-download-action' });
  const queueText = el('div', { class: 'context-item context-queue-state', text: queuedLabel, role: 'status', 'aria-live': 'polite' });
  const downloadButton = item(downloadLabel, async () => {
    if (await isPostQueued(downloadPost, api())) return;
    if (await refreshDownloadState(downloadPost, api())) await api().RevealPost(downloadPost);
    else if (canDownload(downloadPost)) await submitPostDownload(downloadPost, download);
  }, true);
  downloadSlot.append(downloadButton);
  const copyTarget = copyablePost(downloadPost) ? downloadPost : post;
  const copyButton = item('Копировать', async () => {
    setStatus('Копирование изображения…');
    await copyPostImage(copyTarget, api());
    setStatus('Изображение скопировано в буфер обмена');
  }, !copyablePost(copyTarget));
  copyButton.title = 'Скопировать изображение в буфер обмена';
  let downloadRequest = 0;
  async function refreshDownloadButton() {
    const request = ++downloadRequest;
    const queued = await isPostQueued(downloadPost, api());
    const saved = !queued && await refreshDownloadState(downloadPost, api());
    if (closed || request !== downloadRequest) return;
    downloadButton.textContent = downloadButton.title = saved ? locationLabel : downloadLabel;
    downloadButton.disabled = queued || (!saved && !canDownload(downloadPost));
    const focused = document.activeElement === downloadButton;
    const node = queued ? queueText : downloadButton;
    if (downloadSlot.firstElementChild !== node) downloadSlot.replaceChildren(node);
    if (queued && focused) menu.focus();
  }
  let refreshing, refreshAgain = false;
  function refreshDownload() {
    refreshAgain = true;
    if (refreshing) return refreshing;
    refreshing = (async () => {
      // Large download waves emit many events; retain at most one active lookup.
      while (refreshAgain && !closed) {
        refreshAgain = false;
        try { await refreshDownloadButton(); }
        catch (err) { if (!closed) setStatus(String(err), true); }
      }
    })().finally(() => {
      refreshing = undefined;
      if (refreshAgain && !closed) return refreshDownload();
    });
    return refreshing;
  }
  const downloadState = refreshDownload();
  for (const event of ['job:queued', 'job:started', 'job:finished', 'history:changed', 'wave:finished']) unbind.push(window.runtime?.EventsOn?.(event, refreshDownload));
  window.addEventListener('download-submissions-changed', refreshDownload);
  unbind.push(()=>window.removeEventListener('download-submissions-changed', refreshDownload));
  menu.append(el('div', { class: 'context-loading', text: 'Загрузка…' })); document.body.append(menu);
  place(menu, position.clientX, position.clientY); menu.focus();
  document.addEventListener('pointerdown', outside, true); window.addEventListener('resize', close);
  menu.addEventListener('contextmenu', e => e.preventDefault());
  menu.addEventListener('keydown', e => {
    if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); close(); return; }
    if (e.key === 'ArrowLeft' && e.target.closest('.context-submenu')) {
      e.preventDefault(); const sub = e.target.closest('.context-submenu'); sub.hidden = true;
      const head = sub.parentElement.querySelector('.context-group-head'); head.setAttribute('aria-expanded', 'false'); head.focus(); return;
    }
    if (!['ArrowUp','ArrowDown','Home','End'].includes(e.key)) return;
    e.preventDefault(); e.stopPropagation();
    const root = e.target.closest('.context-submenu') || menu;
    const items = [...root.querySelectorAll('button')].filter(b => !b.disabled && (b.closest('.context-submenu') === root || root === menu && !b.closest('.context-submenu')));
    if (!items.length) return;
    const index = items.indexOf(e.target), next = e.key === 'Home' ? 0 : e.key === 'End' ? items.length - 1 : (index + (e.key === 'ArrowUp' ? -1 : 1) + items.length) % items.length;
    items[next].focus();
  });
  try {
    const [authors, favorite, subscriptions] = await Promise.all([
      Promise.resolve(api().ArtistsForPost ? api().ArtistsForPost(post, artistHints) : post.artists || []).then(names => Promise.all((names || []).map(async name => [name, await api().ArtistQuery(name, post)]))),
      api().FavIs(post.site, postId(post)), api().Authors(),
    ]);
    await downloadState;
    if (closed) return;
    menu.replaceChildren();
    const searchable = authors.filter(([, query]) => query);
    const nav = searchable.filter(([, query]) => query !== currentArtistQuery);
    if (!searchable.length) menu.append(item('Неизвестный автор', null, true));
    else if (nav.length === 1) menu.append(item('Работы автора', () => openAuthor(post.site, nav[0][1], nav[0][0])));
    else if (nav.length > 1) group('Работы автора', nav.map(([name, query]) => [name, () => openAuthor(post.site, query, name)]));
    if (menu.childElementCount) separator();
    menu.append(item(favorite ? '★ Убрать из избранного' : '☆ В избранное', async () => { await api().FavToggle(post); changed(); }));
    const starred = query => (subscriptions || []).some(a => a.site === post.site && a.query === query);
    const toggle = (name, query) => async () => { await api().AuthorToggle(name, query, post.site); changed(); };
    if (searchable.length === 1) {
      const [name, query] = searchable[0];
      menu.append(item(starred(query) ? `★ Убрать автора: ${name}` : `☆ Автор в избранное: ${name}`, toggle(name, query)));
    } else if (searchable.length > 1) group('Автор в избранное', searchable.map(([name, query]) => [starred(query) ? `★ ${name}` : name, toggle(name, query)]));
    separator(); menu.append(copyButton, downloadSlot);
    place(menu, position.clientX, position.clientY); menu.querySelector('button:not([disabled])')?.focus();
  } catch (err) {
    await downloadState;
    if (closed) return;
    menu.replaceChildren(copyButton, downloadSlot);
    setStatus(String(err), true);
  }
}
