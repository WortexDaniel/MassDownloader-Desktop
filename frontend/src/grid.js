import { state, el, setStatus, checkEl, flushFilterSettings } from './main.js';
import { postId, postKey, selectionRange, goldMediaPost, postVisible } from './contracts.js';
import { ensureAuth, confirmDownload } from './dialogs.js';
import { showViewer } from './viewer.js';
import { renderPoll } from './poll.js';
import { showPostMenu } from './context-menu.js';

import { OperationScope } from './operations.js';
import { revealOnce, forgetReveal, dismissOverlay, motionDuration } from './motion.js';
import { trackPostView } from './post-views.js';
import { isPostSubmitting, queueClock } from './download-state.js';
import { icon } from './icons.js';
import { mediaPlaceholder, mediaSkeleton, postPlaceholder, fileKind } from './media-placeholder.js';
import { viewPrefs, setViewPref, postAspect } from './view-prefs.js';
let searchScope, searchRequest=0;
export const grid = { posts: [], selected: new Set(), next: 1, site: '', query: '', hasMore: false, busy: false, version: 0, anchor: '', seenCursors: new Set() };
const siteViews=new Map();
// Thumbnails load over the asset-server media proxy (shared disk cache,
// thumbnail network priority) instead of base64 IPC events.
let thumbGeneration = 0;
export const thumbURL = url => `/media?url=${encodeURIComponent(url)}&image_cache=1&thumb=1${thumbGeneration ? `&g=${thumbGeneration}` : ''}`;
let thumbObserver, sweepTimer;
const observedThumbs = new Set();
function sweepThumbs() {
  sweepTimer = null;
  for (const node of observedThumbs) if (!node.isConnected) { thumbObserver?.unobserve(node); observedThumbs.delete(node); }
}
function observeThumb(img, url) {
  if (typeof IntersectionObserver === 'undefined') { img.src = thumbURL(url); return; }
  thumbObserver ??= new IntersectionObserver(entries => {
    for (const entry of entries) if (entry.isIntersecting) {
      const target = entry.target;
      thumbObserver.unobserve(target); observedThumbs.delete(target);
      if (target.isConnected) target.src = thumbURL(target.dataset.url);
    }
  }, { rootMargin: '500px' });
  observedThumbs.add(img); thumbObserver.observe(img);
  sweepTimer ??= setTimeout(sweepThumbs, 2000);
}
const cardPosts = new WeakMap();
const cardViewOptions = new WeakMap(), thumbnailViews = new WeakMap();
window.addEventListener('text-filters-changed', () => {
  for (const card of document.querySelectorAll('.card')) {
    const post = cardPosts.get(card);
    if (post) card.hidden = !postVisible(post, state.filtersState);
  }
});
let hideDone = false;
const api = () => window.go.main.App;
const video = ext => ['webm', 'mp4', 'mov', 'm4v'].includes((ext || '').toLowerCase());
const audio = ext => ['mp3', 'm4a', 'ogg', 'wav', 'flac'].includes((ext || '').toLowerCase());
const image = ext => ['jpg', 'jpeg', 'png', 'gif', 'webp', 'avif', 'bmp'].includes((ext || '').toLowerCase());
const activeTab = () => document.getElementById('content')?.dataset.tab || 'preview';
const pagingMounts = new WeakSet(), pendingPaging = new WeakSet();

function maybeLoadPreviewPage(mount) {
  if (!mount.isConnected || document.getElementById('content') !== mount || activeTab() !== 'preview' || window.isFeedWindow) return;
  if (mount.clientHeight > 0 && mount.scrollHeight - mount.scrollTop - mount.clientHeight <= 336) loadMore();
}
function schedulePreviewPaging(mount) {
  if (pendingPaging.has(mount)) return;
  pendingPaging.add(mount);
  const schedule = window.requestAnimationFrame?.bind(window) || (fn => setTimeout(fn, 0));
  schedule(() => { pendingPaging.delete(mount); maybeLoadPreviewPage(mount); });
}
function bindPreviewPaging(mount) {
  if (pagingMounts.has(mount)) return;
  pagingMounts.add(mount);
  mount.addEventListener('scroll', () => maybeLoadPreviewPage(mount), { passive: true });
  bindScrollChrome(mount);
  if (window.ResizeObserver) new window.ResizeObserver(() => schedulePreviewPaging(mount)).observe(mount);
}

// Scroll affordances of the content area: a hairline under the toolbar once
// the list has moved, and a back-to-top button after a screen of scrolling.
function bindScrollChrome(mount) {
  const wrap = mount.parentElement;
  if (!wrap || wrap.querySelector(':scope > .to-top')) return;
  const top = el('button', { type: 'button', class: 'to-top', title: 'Наверх (Home)', 'aria-label': 'Наверх', tabindex: '-1',
    onclick: () => mount.scrollTo?.({ top: 0, behavior: motionDuration(1) ? 'smooth' : 'auto' }) }, icon('arrowUp', 18));
  wrap.append(top);
  let pending = false;
  const sync = () => {
    pending = false;
    const shown = mount.scrollTop > Math.max(600, mount.clientHeight);
    wrap.classList.toggle('scrolled', mount.scrollTop > 4);
    top.classList.toggle('show', shown); top.tabIndex = shown ? 0 : -1;
  };
  mount.addEventListener('scroll', () => {
    if (pending) return;
    pending = true; (window.requestAnimationFrame?.bind(window) || (fn => setTimeout(fn, 16)))(sync);
  }, { passive: true });
}

// Returns the card's visual: a loading skeleton + lazy <img>, or a typed
// placeholder when the post has nothing to preview.
function thumbnail(p) {
  p = goldMediaPost(p, state.filtersState[p.site]);
  const url = p.preview_url || (image(p.file?.ext) ? p.sample_url : '');
  if (!url) return el('div', { class: 'thumb' }, postPlaceholder(p));
  const kind = p.file ? fileKind(p.file.ext) : 'image';
  const wrap = el('div', { class: 'thumb loading' });
  const img = el('img', { 'data-url': url, loading: 'lazy', decoding: 'async', alt: p.extra?.title || '' });
  img.addEventListener('load', () => {
    img.classList.add('media-ready'); img.classList.remove('thumb-failed');
    wrap.classList.remove('loading', 'failed'); wrap.querySelector('.media-ph')?.remove();
  });
  img.addEventListener('error', () => {
    if (!img.getAttribute('src')) return;
    img.classList.add('thumb-failed'); wrap.classList.remove('loading'); wrap.classList.add('failed');
    if (!wrap.querySelector('.media-ph')) wrap.append(mediaPlaceholder(kind === 'image' ? 'image' : kind, { ext: p.file?.ext || '', label: kind === 'image' ? 'Нет превью' : '', title: kind === 'image' ? '' : 'превью недоступно' }));
  });
  wrap.append(mediaSkeleton(kind), img);
  // Request after the caller has attached the card.
  queueMicrotask(() => { if (img.isConnected) observeThumb(img,url); });
  return wrap;
}
function renderCardThumbnail(card, post) {
  const previous = card.querySelector(':scope > .thumb');
  thumbnailViews.get(previous)?.dispose();
  const thumb = thumbnail(post), options = cardViewOptions.get(card) || {}, img = thumb.querySelector('img');
  if (img) {
    const viewed = trackPostView(img, {post:()=>post,api,queries:()=>options.currentArtistQuery ? [options.currentArtistQuery] : []});
    thumbnailViews.set(thumb, viewed);
    img.addEventListener('load',()=>viewed.ready());
    img.addEventListener('error',()=>viewed.unready());
    if (img.complete && img.naturalWidth > 0) viewed.ready();
  }
  if (previous) previous.replaceWith(thumb); else card.prepend(thumb);
}
export function bindGridEvents() {
	window.runtime?.EventsOn('credentials:changed', () => {
	  thumbGeneration++;
	  for (const card of document.querySelectorAll('.card')) {
	    const post=cardPosts.get(card); if(post)renderCardThumbnail(card,post);
	  }
	});
  window.addEventListener('media-policy-changed', () => {
    for (const card of document.querySelectorAll('.card')) {
      const post = cardPosts.get(card);
      if (post?.site === 'danbooru') renderCardThumbnail(card,post);
    }
  });

  window.runtime?.EventsOn('wave:finished', async () => {
    if (window.isFeedWindow) { await refreshVisibleStates(); return; }
    await updateDownloadStates(grid.posts); if (activeTab() === 'preview') renderGridOrEmpty(); await refreshScoreboard();
  });
  let statusTimer;
  const refresh = () => {
    if (statusTimer) return;
    statusTimer=setTimeout(()=> { statusTimer=undefined; refreshVisibleStates(); },50);
  };
  for (const event of ['job:queued','job:started','job:finished']) window.runtime?.EventsOn(event, refresh);
  window.runtime?.EventsOn('history:changed', refresh);
  window.addEventListener('download-settings-changed', refresh);
  window.addEventListener('download-submissions-changed', refresh);
  window.runtime?.EventsOn('job:progress', updateSpeed);
  window.addEventListener('site-changed', e => {
	searchRequest++;
   searchScope?.close(); searchScope = new OperationScope(api());
   if(grid.site)siteViews.set(grid.site,{posts:grid.posts,selected:new Set(grid.selected),next:grid.next,query:grid.query,hasMore:grid.hasMore,anchor:grid.anchor,seenCursors:new Set(grid.seenCursors)});
   grid.version++;Object.assign(grid,siteViews.get(e.detail)||{posts:[],selected:new Set(),next:1,query:'',hasMore:false,anchor:'',seenCursors:new Set()},{site:e.detail,busy:false});
   updateDownloadStates(grid.posts).then(()=>{if(grid.site===e.detail)renderGridOrEmpty();});renderGridOrEmpty();syncControls();
  });
}
export async function refreshVisibleStates() {
 try {
  const cards=[...document.querySelectorAll('.card')].filter(c=>cardPosts.has(c)),posts=[...new Set([...grid.posts,...cards.map(c=>cardPosts.get(c))])];
  await updateDownloadStates(posts);
  for(const card of cards){if(card.isConnected)decorateCard(card,cardPosts.get(card));}
  if(hideDone&&activeTab()==='preview')renderGridOrEmpty();
 }catch(e){setStatus(String(e),true);}
}
function decorateCard(card, p) {
  const progress = p.download_progress, status = progress?.queued || isPostSubmitting(p) ? 'queued' : progress?.state || (p.downloaded ? 'complete' : 'none');
  const labels = { queued: 'В очереди / загружается', complete: 'Полностью скачано', archive_only: 'Скачан архив', partial: 'Частично скачано', none: 'Не скачано' };
  const count = progress?.total ? ` (${progress.downloaded}/${progress.total})` : '';
  const label = (labels[status] || labels.none) + (status === 'partial' || status === 'archive_only' || status === 'queued' && progress?.downloaded > 0 ? count : '');
  card.dataset.downloadState = status;
  const badge = card.querySelector('.state-badge');
  if (status === 'queued') { if (!badge.querySelector('.queue-clock')) badge.replaceChildren(queueClock()); }
  else badge.textContent = status === 'complete' ? '✓' : status === 'archive_only' ? 'ARC✓' : status === 'partial' ? `◐${progress?.total ? ` ${progress.downloaded}/${progress.total}` : ''}` : '';
  badge.hidden = status !== 'queued' && !badge.textContent; badge.title = label; badge.setAttribute('aria-label', label);
  card.title = `${p.extra?.title || postId(p)}\nсостояние: ${label}`;
}
const downloadStateRequests = new WeakMap();
let downloadStateRequest = 0;
export async function updateDownloadStates(posts) {
  if (!posts.length) return;
  const request = ++downloadStateRequest;
  for (const p of posts) downloadStateRequests.set(p,request);
  const states = await api().DownloadStates(posts);
  for (const p of posts) if (downloadStateRequests.get(p) === request) { p.download_progress = states[postKey(p)]; p.downloaded = p.download_progress?.state === 'complete'; }
}
export async function runSearch() {
  if (!state.currentSite) return;
  const request=++searchRequest, site=state.currentSite.key;
  if(!await ensureAuth(state.currentSite.key))return;
  if(request!==searchRequest || site!==state.currentSite?.key)return;
  try { await flushFilterSettings(); } catch (e) { setStatus(String(e), true); return; }
  if(request!==searchRequest || site!==state.currentSite?.key)return;
  searchScope?.close(); searchScope = new OperationScope(api());
  await api().RefreshMetadata?.();
  if(request!==searchRequest || site!==state.currentSite?.key)return;
  grid.version++; grid.site = state.currentSite.key; grid.query = state.query; grid.next = 1;
  grid.posts = []; grid.selected.clear(); grid.anchor = ''; grid.hasMore = true; grid.busy = false; grid.searched = true;
  grid.seenCursors = new Set(); grid.failed = false;
  const mount = document.getElementById('content'); if (mount) mount.scrollTop = 0;
  renderGridOrEmpty();
  await loadMore();
}
async function loadMore() {
  if (grid.busy || !grid.hasMore) return;
  const version = grid.version; grid.busy = true; syncControls(); setStatus('Поиск…');
  try {
    // Skip empty/duplicate pages and stop even when a site cycles cursors.
    let page, added, cursor = grid.next, hasMore;
    const visited = new Set(grid.seenCursors), known = new Set(grid.posts.map(postKey));
    do {
      visited.add(String(cursor));
      page = await (searchScope ??= new OperationScope(api())).call('SearchPage', grid.site, grid.query, cursor);
      if (version !== grid.version) return;
      cursor = page.next;
      hasMore = !!page.has_more && cursor != null && !visited.has(String(cursor));
      added = (page.posts || []).filter(p => { const key = postKey(p); if (known.has(key)) return false; known.add(key); return true; });
    } while (!added.length && hasMore);
    await updateDownloadStates(added);
    if (version !== grid.version) return;
    grid.next = cursor; grid.hasMore = hasMore; grid.seenCursors = visited;
    grid.posts.push(...added); renderGridOrEmpty(); setStatus(`Найдено: ${grid.posts.length}`);
    grid.failed = false;
  } catch (e) { if (version === grid.version) { grid.failed = true; setStatus(String(e), true); } }
  finally { if (version === grid.version) { grid.busy = false; syncControls(); } }
}
const selectablePosts = () => grid.posts.filter(p => p.file && (!hideDone || !p.downloaded) && postVisible(p, state.filtersState));
function syncControls() {
  const more = document.getElementById('btn-more'), search = document.getElementById('btn-search');
  // Pages load on scroll; the button is only a manual retry after a failure.
  if (more) { more.disabled = grid.busy || !grid.hasMore; more.hidden = !(grid.failed && grid.hasMore); }
  if (search) search.disabled = grid.busy;
  const count = document.getElementById('pv-count');
  if (count) {
    count.textContent = grid.posts.length ? `${grid.posts.length} постов · выбрано ${grid.selected.size}` : (grid.busy ? 'Поиск…' : '');
    count.title = grid.posts.length ? `${grid.hasMore ? 'Прокрутите вниз, чтобы загрузить ещё' : 'Все страницы загружены'}\nКлик — открыть · ☐ на карточке или Ctrl+клик — выделить (Ctrl+клик выделяет диапазон от предыдущего)\nCtrl+A — выделить всё · Esc — снять выделение` : '';
  }
  const dl = document.getElementById('btn-dl-selected');
  if (dl) {
    dl.disabled = !grid.selected.size;
    const label = dl.querySelector('.btn-label'); if (label) label.textContent = grid.selected.size ? `Скачать выбранные (${grid.selected.size})` : 'Скачать выбранные';
  }
  const all = document.getElementById('btn-select-all'); if (all) all.disabled = !grid.posts.length;
  const clear = document.getElementById('btn-clear-sel'); if (clear) clear.hidden = !grid.selected.size;
}
function selectAll() { for (const p of selectablePosts()) grid.selected.add(postKey(p)); renderGridOrEmpty(); }
function clearSelection() { if (!grid.selected.size) return false; grid.selected.clear(); grid.anchor = ''; renderGridOrEmpty(); return true; }
window.addEventListener('grid-key', ({ detail: e }) => {
  if (activeTab() !== 'preview') return;
  const key = e.key.toLowerCase();
  if ((e.ctrlKey || e.metaKey) && key === 'a') { e.preventDefault(); selectAll(); }
  else if (e.key === 'Escape') { if (clearSelection()) e.preventDefault(); }
  else if ((e.ctrlKey || e.metaKey) && key === 'd') { e.preventDefault(); document.getElementById('btn-dl-selected')?.click(); }
});
export function renderGridOrEmpty() {
  if (activeTab() !== 'preview') return;
  const mount = document.getElementById('content'); if (!mount) return;
  bindPreviewPaging(mount);
  if (!grid.posts.length) {
    mount.replaceChildren(emptyState()); syncControls(); return;
  }
  let g = mount.firstElementChild;
  if (!g?.classList.contains('grid')) { g = el('div', { class: 'grid' }); mount.replaceChildren(g); }
  const visiblePosts = () => grid.posts.filter(p => (!hideDone || !p.downloaded) && postVisible(p, state.filtersState));
  const visible = visiblePosts(), keys = new Set(visible.map(postKey));
  const existing = new Map([...g.children].map(card => [postKey(cardPosts.get(card)), card]));
  for (const [key, card] of existing) if (!keys.has(key)) { forgetReveal(card); card.remove(); }
  let position = g.firstElementChild;
  for (const p of visible) {
    let card = existing.get(postKey(p));
    if (card && cardPosts.get(card) !== p) { if (position === card) position = card.nextElementSibling; forgetReveal(card); card.remove(); card = null; }
    if (!card) { card = postCard(p); bindCardSelection(card, p, visiblePosts, grid, () => { renderGridOrEmpty(); syncControls(); }); }
    else decorateCard(card, p);
    const selected = grid.selected.has(postKey(p));
    card.classList.toggle('sel', selected);
    card.querySelector('.card-pick')?.setAttribute('aria-pressed', String(selected));
    if (card !== position) g.insertBefore(card, position);
    position = card.nextElementSibling;
  }
  syncControls(); schedulePreviewPaging(mount);
}
function emptyState() {
  const searched = grid.searched && !grid.hasMore && !grid.busy && !grid.failed && grid.site === state.currentSite?.key;
  if (searched) return el('div', { class: 'empty' }, el('div', { class: 'glyph' }, icon('searchX', 34)), el('h2', { text: 'Ничего не найдено' }),
    el('p', { text: 'По этому запросу постов нет. Ослабьте фильтры: уберите теги, снизьте порог Score или смените рейтинг.' }));
  return el('div', { class: 'empty' }, el('div', { class: 'glyph' }, icon('search', 34)), el('h2', { text: 'Здесь появятся результаты' }),
    el('p', { text: 'Задайте фильтры слева и нажмите «Показать» (Enter в строке запроса или F5).' }),
    el('div', { class: 'steps' },
      el('div', { class: 'step' }, el('span', { class: 'n', text: '1' }), el('span', { class: 'st', text: 'Выберите сайт' })),
      el('div', { class: 'step' }, el('span', { class: 'n', text: '2' }), el('span', { class: 'st', text: 'Добавьте теги или автора' })),
      el('div', { class: 'step' }, el('span', { class: 'n', text: '3' }), el('span', { class: 'st', text: 'Показать или скачать всё' }))));
}
function bindCardSelection(card, p, posts, selection, refresh, authorQueries) {
  // Visible checkbox: plain-click selection without modifier keys.
  const pick = el('button', { type: 'button', class: 'card-pick', title: 'Выделить', 'aria-label': 'Выделить пост',
    onclick: e => {
      e.stopPropagation();
      const key = postKey(p);
      if (selection.selected.has(key)) selection.selected.delete(key); else selection.selected.add(key);
      selection.anchor = key; refresh();
    } });
  card.append(pick);
  card.addEventListener('click', e => {
    const key = postKey(p);
    if (e.ctrlKey || e.metaKey) {
      e.preventDefault();
      const keys = e.ctrlKey ? selectionRange(posts(), selection.anchor, key) : [key];
      if (keys.length === 1 && selection.selected.has(key)) selection.selected.delete(key);
      else for (const k of keys) selection.selected.add(k);
      selection.anchor = key; refresh();
    } else { selection.anchor = key; openViewer(p, posts(), authorQueries); }
  });
}
export function postCard(p, onOpen, options = {}) {
  const card = el('div', { class: 'card', title: p.extra?.title || postId(p) });
  card.style?.setProperty?.('--ar', String(postAspect(p)));
  cardPosts.set(card,p);
  cardViewOptions.set(card,options);
  card.hidden = !postVisible(p, state.filtersState);
  if (options.animate !== false) revealOnce(card);
  renderCardThumbnail(card,p);
  const animated = video(p.file?.ext) || p.file?.ext === 'gif';
  card.append(el('div', { class: 'card-meta' }, el('span', { title: 'Оценка' }, icon('star', 11), String(p.score || 0)),
    p.page_count > 1 ? el('span', { title: 'Файлов в посте' }, icon('layers', 11), String(p.page_count)) : null,
    animated ? el('span', { class: 'kind', title: video(p.file?.ext) ? 'Видео' : 'GIF' }, icon('play', 11), video(p.file?.ext) ? null : 'GIF') : null));
  card.append(el('span', { class: 'state-badge' })); decorateCard(card,p);
  card.addEventListener('contextmenu', e => { e.preventDefault(); e.stopPropagation(); showPostMenu(p, e, { api, download, openAuthor, artistHints:state.filtersState[p.site]?.artists || [], ...options }); });
  if (onOpen) card.addEventListener('click', () => onOpen(p)); return card;
}
async function download(posts, artistHints) {
  await flushFilterSettings();
  posts = posts.map(p => goldMediaPost(p, state.filtersState[p.site]));
  for(const site of new Set(posts.map(p=>p.site)))if(!await ensureAuth(site))return;
  const settings = await api().GetAllSettings();
  const n = await api().DownloadSelection(posts, settings.download_root || 'downloads', settings.subfolder_by_artist !== false, artistHints ?? state.filtersState[state.currentSite?.key]?.artists ?? []);
  setStatus(`В очереди: ${n}`); await refreshScoreboard(); return n;
}
function button(text, fn, ghost = true) { return el('button', { class: 'pv-btn' + (ghost ? ' ghost' : ''), text, type: 'button', onclick: async () => { try { await fn(); } catch (e) { setStatus(String(e), true); } } }); }

// Sanitise captions using an allowlist; only HTTP(S) external links survive.
export function postContent(p) {
  const content = el('div', { class: 'post-content' });
  if (p.extra?.title) content.append(el('h3', { text: p.extra.title }));
  const parsed = new DOMParser().parseFromString(String(p.extra?.content || ''), 'text/html');
  const allowed = new Set(['P', 'BR', 'DIV', 'SPAN', 'B', 'STRONG', 'I', 'EM', 'U', 'S', 'UL', 'OL', 'LI', 'BLOCKQUOTE', 'PRE', 'CODE', 'H1', 'H2', 'H3', 'H4', 'A', 'HR']);
  function append(source, dest) {
    for (const child of source.childNodes) {
      if (child.nodeType === 3) { dest.append(document.createTextNode(child.textContent)); continue; }
      if (child.nodeType !== 1 || ['SCRIPT','STYLE','IFRAME','OBJECT','FORM'].includes(child.tagName)) continue;
      if (!allowed.has(child.tagName)) { append(child, dest); continue; }
      const node = document.createElement(child.tagName.toLowerCase());
      if (child.tagName === 'A') { try { const u = new URL(child.getAttribute('href')); if (['https:', 'http:'].includes(u.protocol)) { node.href = u.href; node.addEventListener('click', e => { e.preventDefault(); window.runtime?.BrowserOpenURL(u.href); }); } } catch {} }
      append(child, node); dest.append(node);
    }
  }
  append(parsed.body, content);
  const embed = p.extra?.embed; if (embed?.url) { try { const u = new URL(embed.url); if (['https:', 'http:'].includes(u.protocol)) content.append(button(embed.subject || embed.title || u.href, () => window.runtime?.BrowserOpenURL(u.href))); } catch {} }
  const poll = renderPoll(p.extra?.poll); if (poll) content.append(poll);
  if (p.extra?.locked_count) content.append(el('p', { text: `Закрытых вложений: ${p.extra.locked_count}` }));
  return content;
}
export async function openViewer(original, gallery = [original], authorQueries) {
  await flushFilterSettings();
  return showViewer(original, gallery.filter(p => postVisible(p, state.filtersState)), { download, openAuthor, updateDownloadStates, authorQueries });
}
export function openAuthor(site, query, name, { hideText = false } = {}) {
  const searchQuery = hideText ? `${query} hide:text` : query;
  const scope = new OperationScope(api());
  const confirmation = new AbortController();
  const overlay = el('div', { class: 'viewer-overlay dialog-overlay' }), box = el('div', { class: 'dialog-box artist-box' });
  const posts = [], gallery = el('div', { class: 'grid' }), count = el('span', { class: 'muted' });
  const loading = el('div', { class: 'viewer-loading author-loading', role: 'status', 'aria-live': 'polite' },
    el('span', { class: 'viewer-spinner', 'aria-hidden': true }), el('span', { text: 'Загрузка…' }));
  const selection = { selected: new Set(), anchor: '' };
  let seenCursors = new Set(), pagingScheduled = false, pagingFailed = false;
  function maybeLoadAuthorPage() {
    if (closed || !box.isConnected || downloading || busy || loadPromise || !hasMore || pagingFailed) return;
    if (box.clientHeight > 0 && box.scrollHeight - box.scrollTop - box.clientHeight <= 336) load().catch(() => {});
  }
  function scheduleAuthorPaging() {
    if (pagingScheduled || closed) return;
    pagingScheduled = true;
    const schedule = window.requestAnimationFrame?.bind(window) || (fn => setTimeout(fn, 0));
    schedule(() => { pagingScheduled = false; maybeLoadAuthorPage(); });
  }
  box.addEventListener('scroll', () => { pagingFailed = false; maybeLoadAuthorPage(); }, { passive: true });
  const resizeObserver = window.ResizeObserver ? new window.ResizeObserver(scheduleAuthorPaging) : null;
  resizeObserver?.observe(box);
  let next = 1, closed = false, busy = false, downloading = false, collectingAll = false, hasMore = true, loadPromise = null, favoriteRequest = 0;
  const downloadStatus = el('div', { class: 'author-download-status muted', role: 'status', 'aria-live': 'polite' });
  const downloadAll = button('Скачать всё', () => downloadAuthor(true));
  const downloadSelected = button('Скачать выбранные', () => downloadAuthor(false));
  function report(text, pending = false, error = false) {
    if (closed) return;
    downloadStatus.replaceChildren(...(pending ? [el('span', { class: 'viewer-spinner', 'aria-hidden': true })] : []), document.createTextNode(text));
    downloadStatus.classList.toggle('error', error);
    downloadStatus.setAttribute('aria-busy', String(pending));
  }
  function syncDownloadButtons() {
    downloadAll.disabled = downloading;
    downloadSelected.disabled = downloading || !posts.some(p => p.file && selection.selected.has(postKey(p)));
    more.disabled = busy || downloading || !hasMore;
  }
  function refreshSelection() {
    for (const card of gallery.children) card.classList.toggle('sel', selection.selected.has(postKey(cardPosts.get(card))));
    count.textContent = `${name} · загружено ${posts.length} постов · выбрано ${selection.selected.size} · ${hasMore ? 'есть ещё' : 'все страницы загружены'}`;
    syncDownloadButtons();
  }
  const previousFocus = document.activeElement;
  const close = () => { if(closed)return;scope.close(); closed = true; confirmation.abort(); resizeObserver?.disconnect(); favoriteRequest++; window.removeEventListener('favorites-changed', favoritesChanged); for(const card of gallery.children)forgetReveal(card); dismissOverlay(overlay);if(previousFocus?.isConnected)previousFocus.focus(); };
  const more = button('Ещё', () => load().catch(() => {})), star = button('☆ В избранное', async () => {
    if (star.disabled || closed) return;
    star.disabled = true; favoriteRequest++;
    try {
      const on = await api().AuthorToggle(name, query, site);
      if (!closed) setFavorite(on);
      setStatus(on ? 'Автор добавлен в избранное' : 'Автор удалён из избранного');
      window.dispatchEvent(new Event('favorites-changed'));
    } finally { star.disabled = false; }
  });
  function setFavorite(on) {
    star.textContent = on ? '★ В избранном' : '☆ В избранное';
    star.title = on ? 'Убрать автора из избранного' : 'Добавить автора в избранное';
    star.setAttribute('aria-pressed', String(!!on));
    star.classList.toggle('is-favorite', !!on);
  }
  async function refreshFavorite() {
    const request = ++favoriteRequest;
    const authors = await api().Authors();
    if (!closed && request === favoriteRequest && !star.disabled) setFavorite((authors || []).some(a => a.site === site && a.query === query));
  }
  function favoritesChanged() { refreshFavorite().catch(e => { if (!closed) setStatus(String(e), true); }); }
  star.classList.add('viewer-favorite', 'author-favorite'); setFavorite(false);
  window.addEventListener('favorites-changed', favoritesChanged);
  function load() {
    if (loadPromise) return loadPromise;
    if (closed || !hasMore) return Promise.resolve();
    loadPromise = loadPage().then(() => scheduleAuthorPaging()).finally(() => { loadPromise = null; });
    return loadPromise;
  }
  async function loadPage() {
    pagingFailed = false;
    busy = true; syncDownloadButtons();
    gallery.setAttribute('aria-busy', 'true'); box.append(loading);
    try {
      await flushFilterSettings();
      if (closed) return;
      if (!await ensureAuth(site)) throw new Error('Загрузка отменена: требуется авторизация');
      const known = new Set(posts.map(postKey)), cursors = new Set(seenCursors);
      let cursor = next, page, added;
      do {
        cursors.add(String(cursor));
        page = await scope.call('SearchPage', site, searchQuery, cursor); if (closed) return;
        if (page.has_more && (page.next == null || cursors.has(String(page.next)))) throw new Error('Источник повторил страницу автора');
        cursor = page.next;
        added = (page.posts || []).filter(p => { const key = postKey(p); if (known.has(key)) return false; known.add(key); return true; });
      } while (!added.length && page.has_more);
      await updateDownloadStates(added); if (closed) return;
      posts.push(...added);
      for (const p of added) {
        const card = postCard(p, null, {currentArtistQuery:query});
        bindCardSelection(card, p, () => posts.filter(p=>postVisible(p,state.filtersState)), selection, refreshSelection, ()=>[query]);
        gallery.append(card);
      }
      next = cursor; hasMore = !!page.has_more; seenCursors = cursors; refreshSelection();
      if (collectingAll) report(`Сканирование всех страниц автора… Найдено: ${posts.length}`, true);
      loading.remove(); gallery.setAttribute('aria-busy', 'false');
    } catch (e) { pagingFailed = true; report(String(e), false, true); setStatus(String(e), true); throw e; }
    finally { busy = false; loading.remove(); gallery.setAttribute('aria-busy', 'false'); syncDownloadButtons(); }
  }
  async function downloadAuthor(all) {
    if (downloading || closed) return;
    // Snapshot selection before any awaited calls; later clicks cannot change it.
    let chosen = posts.filter(p => p.file && selection.selected.has(postKey(p)));
    if (!all && !chosen.length) return;
    downloading = true; collectingAll = all; syncDownloadButtons();
    const trigger = all ? downloadAll : downloadSelected;
    trigger.setAttribute('aria-busy', 'true'); trigger.textContent = all ? 'Сканирование…' : 'Подготовка…';
    report(all ? 'Сканирование всех страниц автора…' : `Подготовка выбранных постов: ${chosen.length}…`, true);
    try {
      if (all) {
        // Use the same query, pagination and explicit-download path as the gallery.
        // StartRun is unavailable in the feed window and inherits unrelated limits.
        if (loadPromise) await loadPromise;
        while (hasMore && !closed) await load();
        chosen = posts.filter(p => p.file);
      }
      if (closed) return;
      collectingAll = false;
      const settings = all ? await api().GetAllSettings() : null;
      if (closed) return;
      if (all && chosen.length && settings.confirm_download_author !== false) {
        trigger.textContent = 'Ожидание подтверждения…';
        report(`Сканирование завершено: ${posts.length} постов, с файлами: ${chosen.length}. Ожидание подтверждения.`);
        const accepted = await confirmDownload(`Автор: ${name}. Найдено ${posts.length} постов, с файлами: ${chosen.length}. Скачать все их вложения?`, confirmation.signal);
        if (closed) return;
        if (!accepted) { report('Скачивание отменено'); return; }
      }
      trigger.textContent = 'Подготовка…';
      report(`Подготовка вложений из ${chosen.length} постов…`, true);
      const n = chosen.length ? await download(chosen, []) : 0;
      if (n === undefined) report('Загрузка отменена: требуется авторизация', false, true);
      else report(`В очередь добавлено: ${n} файлов из ${chosen.length} постов${n === 0 ? ' (новых файлов нет)' : ''}`);
    } catch (e) { report(String(e), false, true); setStatus(String(e), true); }
    finally {
      downloading = false; collectingAll = false; trigger.textContent = all ? 'Скачать всё' : 'Скачать выбранные';
      trigger.setAttribute('aria-busy', 'false'); syncDownloadButtons();
    }
  }
  const closer = button('✕', close); closer.className = 'viewer-close';
  refreshSelection();
  box.append(closer, el('h2', { text: `Работы автора — ${name}` }), el('div', { class: 'viewer-actions' }, star, downloadAll, downloadSelected, more), count,
    el('p', { class: 'muted', text: '«Скачать всё» сначала сканирует все страницы автора. Один пост может содержать несколько файлов.' }), downloadStatus, gallery);
  overlay.append(box); overlay.onclick = e => { if (e.target === overlay) close(); }; overlay.onkeydown = e => { if (e.key === 'Escape') close(); }; document.body.append(overlay); overlay.tabIndex = -1; overlay.focus(); favoritesChanged(); load().catch(() => {});
}
// Layout (equal tiles / mosaic by aspect ratio) and tile size. Both are CSS
// switches on <html>; cards are not rebuilt.
function segmented(label, key, options) {
  const group = el('div', { class: 'seg', role: 'group', 'aria-label': label });
  const sync = () => { for (const b of group.children) b.setAttribute('aria-pressed', String(b.dataset.value === viewPrefs()[key])); };
  for (const [value, title, content] of options) group.append(el('button', { type: 'button', title, 'aria-label': title, dataset: { value },
    onclick: () => {
      const mount = document.getElementById('content');
      // Keep the reading position: the same share of the list stays in view.
      const share = mount && mount.scrollHeight > mount.clientHeight ? mount.scrollTop / mount.scrollHeight : 0;
      setViewPref(key, value, state.settings, api()); sync();
      if (mount && share) mount.scrollTop = share * mount.scrollHeight;
    } }, content));
  sync();
  return group;
}
function viewControls() {
  return [
    segmented('Раскладка', 'mode', [['tiles', 'Плитка: одинаковые ячейки', icon('tiles', 15)], ['mosaic', 'Мозаика: по пропорциям изображений', icon('mosaic', 15)]]),
    segmented('Размер плиток', 'size', [['s', 'Мелкие плитки', 'S'], ['m', 'Средние плитки', 'M'], ['l', 'Крупные плитки', 'L']]),
  ];
}
export function renderControls() {
  const bar = document.getElementById('controls-bar');
  const search = button('Показать', runSearch, false); search.id = 'btn-search'; search.title = 'Найти посты по запросу (Enter в строке запроса, F5)';
  const more = button('Ещё', loadMore); more.id = 'btn-more'; more.title = 'Повторить загрузку следующей страницы';
  const all = button('Выделить всё', selectAll); all.id = 'btn-select-all'; all.title = 'Ctrl+A';
  const clear = button('Снять', clearSelection); clear.id = 'btn-clear-sel'; clear.title = 'Снять выделение (Esc)'; clear.hidden = true;
  const dl = el('button', { id: 'btn-dl-selected', class: 'pv-btn', type: 'button', title: 'Ctrl+D',
    onclick: async () => { try { await download(grid.posts.filter(p => p.file && grid.selected.has(postKey(p)))); } catch (e) { setStatus(String(e), true); } } },
    icon('download', 15), el('span', { class: 'btn-label', text: 'Скачать выбранные' }));
  bar.replaceChildren(search, more, el('span', { id: 'pv-count', class: 'pv-count' }), el('span', { class: 'pv-spacer' }),
    el('label', { class: 'toggle' }, checkEl(hideDone, on => { hideDone = on; renderGridOrEmpty(); }, { 'aria-label': 'Скрыть скачанное' }), 'Скрыть скачанное'),
    ...viewControls(), all, clear, dl);
  syncControls();
}
let scoreboardRequest = 0;
export async function refreshScoreboard() {
  // The separate feed has no main-window counters or Summary/IsRunning bridge methods.
  if (window.isFeedWindow) return;
  const request = ++scoreboardRequest;
  try {
    const [s, running] = await Promise.all([api().Summary(), api().IsRunning()]);
    if (request !== scoreboardRequest) return;
    const total = s.total || 0, done = (s.done || 0) + (s.skipped || 0), pending = (s.queued || 0) + (s.running || 0);
    const set = (id, value) => {
      const node = document.getElementById(id); if (!node) return;
      node.textContent = String(value || 0);
      node.parentElement?.classList.toggle('zero', !value);
    };
    set('score-done', done); set('score-total', total); set('stat-scanned', s.scanned); set('stat-pending', pending); set('stat-dupes', s.duplicates); set('stat-skipped', s.skipped); set('stat-failed', s.failed);
    // The strip only takes space while there is something to report.
    document.getElementById('run-strip')?.classList.toggle('idle', !running && !total && !s.scanned && !s.failed);
    const stop = document.getElementById('btn-stop'), start = document.getElementById('btn-dl-all'); if (stop) stop.disabled = !running; if (start) start.disabled = running;
    const progress = document.getElementById('progress-bar'); if (progress) progress.style.width = total ? `${100 * (done + (s.failed || 0) + (s.cancelled || 0)) / total}%` : '0';
    window.setJobTabCount?.(s.session_total ?? total);
  } catch (e) { if (request === scoreboardRequest) setStatus(String(e), true); }
}
let speedAt = 0, speedBytes = 0; const receivedByJob = new Map();
function updateSpeed(id, bytes) {
  const previous = receivedByJob.get(id) || 0; receivedByJob.set(id, bytes); speedBytes += Math.max(0, bytes - previous);
  const now = Date.now(); if (!speedAt) speedAt = now;
  if (now - speedAt >= 500) { const node = document.getElementById('status-speed'); if (node) node.textContent = `${(speedBytes / ((now - speedAt) / 1000) / 1048576).toFixed(1)} МБ/с`; speedBytes = 0; speedAt = now; }
}
export function initMainArea() { renderControls(); bindGridEvents(); }
