import { el, setStatus, state } from './main.js';
import { postId, postKey, mediaURL, goldMediaPost } from './contracts.js';
import { postContent } from './grid.js';
import { viewerImagePNG } from './image-copy.js';
import { imageViewport } from './viewer-image.js';

import { OperationScope } from './operations.js';
import { dismissOverlay, animateIn } from './motion.js';
import { trackPostView } from './post-views.js';
import { locationLabel } from './download-state.js';
import { icon } from './icons.js';
import { fileKind } from './media-placeholder.js';
const api = () => window.go.main.App;
const isArchive = ext => ['zip', 'rar', '7z'].includes((ext || '').toLowerCase());
const isVideo = ext => ['webm', 'mp4', 'mov', 'm4v'].includes((ext || '').toLowerCase());
const isAudio = ext => ['mp3', 'm4a', 'ogg', 'wav', 'flac'].includes((ext || '').toLowerCase());
const isImage = ext => ['jpg', 'jpeg', 'png', 'gif', 'webp', 'avif', 'bmp'].includes((ext || '').toLowerCase());

export function standaloneAttachment(page) {
  const selected = structuredClone(page);
  selected.extra = { ...selected.extra, expanded_page: true, standalone_attachment: true };
  return selected;
}
export function archiveNote(page) {
  const extra = page.extra || {};
  if (extra.archive_extract === false) return 'Автоматическая распаковка отключена';
  return `Будет распакован ${extra.archive_to_artist ? 'в папку автора' : 'в отдельную подпапку'}${extra.archive_delete ? '; исходный архив будет удалён' : ''}`;
}

export async function showViewer(original, gallery, { download, openAuthor, updateDownloadStates, authorQueries = () => [] }) {
  const posts = gallery?.length ? [...gallery] : [original];
  let index = posts.findIndex(p => postKey(p) === postKey(original));
  if (index < 0) { posts.push(original); index = posts.length - 1; }
  const overlay = el('div', { class: 'viewer-overlay', role: 'dialog', 'aria-modal': true, 'aria-label': 'Просмотр поста', tabindex: '-1' });
  const box = el('div', { class: 'viewer-box' });
  const title = el('div', { class: 'viewer-title' });
  const closeBtn = action('✕', close); closeBtn.className = 'viewer-close'; closeBtn.title = 'Закрыть (Esc)'; closeBtn.setAttribute('aria-label', 'Закрыть');
  const mediaWrap = el('div', { class: 'viewer-media', tabindex: '0' });
  const caption = el('div', { class: 'viewer-caption' });
  const authors = el('div', { class: 'viewer-authors' });
  const info = el('div', { class: 'viewer-info' });
  const tags = el('div', { class: 'viewer-tags' });
  const actions = el('div', { class: 'viewer-actions' });
  const previousFocus = document.activeElement;
  let scope;
  let closed = false, version = 0, current = original, pages = [], primary, refreshButtons = [], busy = false, downloadBusy = false;
  let favoriteButton, favoriteRequest = 0;
  const queued = new Set();
  const unbind = [];
  const copyTimers = new Map();
  let mediaDisposers = [];
  function observeView(node, ready = false) {
    const tracker = trackPostView(node,{post:()=>current,api,queries:()=>authorQueries(current)});
    mediaDisposers.push(()=>tracker.dispose());
    if (ready) tracker.ready();
    return tracker;
  }
  function action(text, fn, primary = false) {
    return el('button', { type: 'button', class: 'pv-btn' + (primary ? '' : ' ghost'), text, onclick: async () => {
      try { await fn(); } catch (err) { setStatus(String(err), true); }
    } });
  }
  function stopMedia() {
    mediaDisposers.forEach(dispose => dispose()); mediaDisposers = [];
    for (const media of mediaWrap.querySelectorAll('video,audio')) { media.pause(); media.removeAttribute('src'); media.load(); }
    for (const image of mediaWrap.querySelectorAll('img')) image.removeAttribute('src');
  }
  function close() {
    if (closed) return;
    scope?.close(); closed = true; version++; clearCopyFeedback(); stopMedia(); unbind.forEach(fn => fn?.()); dismissOverlay(overlay);
    if (previousFocus?.isConnected) previousFocus.focus();
  }
  function loading(text = 'Загрузка…') {
    return el('div', { class: 'viewer-loading', role: 'status', 'aria-live': 'polite' }, el('span', { class: 'viewer-spinner', 'aria-hidden': true }), el('span', { text }));
  }
  function clearCopyFeedback() {
    for (const timer of copyTimers.values()) clearTimeout(timer);
    copyTimers.clear();
  }
  function flashCopy(button, label, text = 'Скопировано ✓') {
    if (closed || !button.isConnected) return;
    clearTimeout(copyTimers.get(button));
    button.textContent = text; button.classList.toggle('copy-confirmed', text === 'Скопировано ✓');
    copyTimers.set(button, setTimeout(() => {
      copyTimers.delete(button);
      if (closed || !button.isConnected) return;
      button.textContent = label; button.classList.remove('copy-confirmed');
    }, 1400));
  }
  function copyAction(label, getText, emptyLabel, generation) {
    const button = action(label, async () => {
      if (button.disabled) return;
      button.disabled = true;
      try {
        const text = await getText();
        if (!text) { if (version === generation) flashCopy(button, label, emptyLabel); return; }
        if (!window.runtime?.ClipboardSetText) throw new Error('Буфер обмена недоступен');
        const copied = await window.runtime.ClipboardSetText(text);
        if (copied === false) throw new Error('Не удалось скопировать в буфер обмена');
        if (version === generation) flashCopy(button, label);
      } catch (err) {
        if (version === generation) flashCopy(button, label, 'Ошибка копирования');
        throw err;
      } finally { button.disabled = false; }
    });
    button.classList.add('viewer-copy');
    return button;
  }
  function copyImageAction(generation) {
    const label = 'Копировать';
    const button = action(label, async () => {
      if (button.disabled || closed || version !== generation) return;
      button.disabled = true;
      try {
        const png = viewerImagePNG(mediaWrap);
        if (!png) { flashCopy(button, label, 'Нет изображения'); return; }
        if (!api().ClipboardSetImage) throw new Error('Буфер обмена изображений недоступен');
        const copied = await api().ClipboardSetImage(png);
        if (copied === false) throw new Error('Не удалось скопировать изображение');
        if (!closed && version === generation) flashCopy(button, label);
      } catch (err) {
        if (!closed && version === generation) { flashCopy(button, label, 'Ошибка копирования'); throw err; }
      } finally { button.disabled = false; }
    });
    button.classList.add('viewer-copy'); button.title = 'Скопировать изображение в буфер обмена';
    return button;
  }
  function setFavorite(button, on) {
    button.textContent = on ? '★ В избранном' : '☆ В избранное';
    button.title = on ? 'Убрать пост из избранного' : 'Добавить пост в избранное';
    button.setAttribute('aria-pressed', String(!!on));
    button.classList.toggle('is-favorite', !!on);
  }
  async function refreshFavorite() {
    const button = favoriteButton, generation = version, post = current, request = ++favoriteRequest;
    if (!button || closed) return;
    const on = await api().FavIs(post.site, postId(post));
    if (!closed && version === generation && button === favoriteButton && request === favoriteRequest && !button.disabled) setFavorite(button, on);
  }
  async function refresh() {
    const generation = version, post = current;
    try {
      await updateDownloadStates([post]);
      if (closed || generation !== version) return;
      if (post.downloaded) queued.delete(postKey(post));
      if (primary) {
        const pending = queued.has(postKey(post));
        primary.textContent = post.downloaded ? locationLabel : pending ? 'Добавлено в очередь…' : pages.length > 1 ? `Скачать альбом (${pages.length})` : isArchive(post.file?.ext) ? 'Скачать архив' : 'Скачать';
        primary.title = `${primary.textContent} (Пробел)`;
        primary.disabled = busy || downloadBusy || (!post.downloaded && (pending || !post.file));
      }
      await Promise.all(refreshButtons.map(fn => fn()));
    } catch (err) { if (!closed && generation === version) setStatus(String(err), true); }
  }
  async function primaryAction() {
    if (busy || downloadBusy || queued.has(postKey(current))) return;
    const post = current, generation = version;
    downloadBusy = true; primary.disabled = true;
    try {
      await updateDownloadStates([post]);
      if (closed || generation !== version) return;
      if (post.downloaded) { await api().RevealPost(post); return; }
      if (!post.file) return;
      const count = await download([post]); if (count > 0) queued.add(postKey(post));
    } finally { downloadBusy = false; if (!closed) await refresh(); }
  }
  function attachmentCard(page, generation) {
    const ext = page.file.ext, archive = isArchive(ext);
    const name = page.extra?.download_name || `file.${ext}`;
    const key = `${postKey(page)}\0${page.page_index || 0}`;
    let submitting = false;
    const btn = action(archive ? 'Скачать архив' : 'Скачать вложение', async () => {
      if (submitting) return;
      submitting = true; btn.disabled = true;
      try {
        if (await api().LocalMedia(page)) { await api().RevealPost(page); return; }
        if (queued.has(key)) return;
        const count = await download([standaloneAttachment(page)]);
        if (count > 0) queued.add(key);
      } finally { submitting = false; await update(); }
    }, true);
    const update = async () => {
      const path = await api().LocalMedia(page);
      if (closed || generation !== version) return;
      if (path) queued.delete(key);
      btn.textContent = path ? locationLabel : queued.has(key) ? 'Добавлено в очередь…' : archive ? 'Скачать архив' : 'Скачать вложение';
      btn.disabled = submitting || (!path && queued.has(key)); btn.title = path || '';
    };
    refreshButtons.push(update); update().catch(() => {});
    const card = el('div', { class: 'viewer-attachment', dataset: { kind: fileKind(ext) } }, el('span', { class: 'viewer-attachment-icon' }, icon(archive ? 'archive' : fileKind(ext) === 'document' ? 'fileText' : fileKind(ext) === 'video' ? 'film' : fileKind(ext) === 'audio' ? 'music' : 'file', 40)),
      el('strong', { text: `${archive ? 'АРХИВ' : 'ВЛОЖЕНИЕ'} .${ext.toUpperCase()}` }),
      el('div', { class: 'attachment-name', text: name }), archive ? el('p', { class: 'muted', text: archiveNote(page) }) : null, btn);
    observeView(card, true);
    return card;
  }
  function mediaFrame(page, generation, album) {
    const file = page.file, ext = (file?.ext || '').toLowerCase();
    if (isArchive(ext) || (file && !isImage(ext) && !isVideo(ext) && !isAudio(ext))) return attachmentCard(page, generation);
    const source = isVideo(ext) || isAudio(ext) || ext === 'gif' ? file?.url : page.sample_url || file?.url || page.preview_url;
    if (!source) return file ? attachmentCard(page, generation) : null;
    const frame = el('div', { class: 'viewer-frame' }), placeholder = loading();
    const media = el(isVideo(ext) ? 'video' : isAudio(ext) ? 'audio' : 'img', { alt: page.extra?.download_name || '', hidden: true });
    let sourceVersion = 0, fallback = false;
    const viewed = observeView(media);
    function ready() {
      if (closed || version !== generation) return;
      if (frame.classList.contains('loaded')) return;
      placeholder.hidden = true; media.hidden = false; frame.classList.add('loaded');
      media.classList.add('media-ready');
      viewed.ready();
    }
    function load(url, cacheVideo = false) {
      viewed.unready();
      sourceVersion++; placeholder.replaceChildren(el('span', { class: 'viewer-spinner' }), el('span', { text: cacheVideo ? 'Загрузка видео в кеш…' : 'Загрузка…' }));
      placeholder.hidden = false; media.hidden = true; frame.classList.remove('loaded'); media.classList.remove('media-ready'); media.src = url;
    }
    async function loadRemote() {
      const requested = sourceVersion;
      let cacheVideo = false;
      if (isVideo(ext)) {
        try { cacheVideo = await api().GetSetting('cache_videos') === true; } catch {}
      }
      if (!closed && version === generation && requested === sourceVersion) load(mediaURL(source, ext), cacheVideo);
    }
    media.addEventListener(media.tagName === 'IMG' ? 'load' : 'loadeddata', ready);
    media.addEventListener('canplay', ready);
    media.addEventListener('error', () => {
      if (closed || version !== generation) return;
      viewed.unready();
      if (!fallback && file?.url && media.tagName === 'IMG' && file.url !== source) { fallback = true; load(mediaURL(file.url)); return; }
      placeholder.hidden = false; media.hidden = true;
      placeholder.classList.add('failed');
      placeholder.replaceChildren(icon(isVideo(ext) ? 'film' : isAudio(ext) ? 'music' : 'imageOff', 34), el('span', { text: 'Не удалось загрузить контент' }), action('Повторить', () => { placeholder.classList.remove('failed'); fallback = false; return loadRemote(); }));
    });
    if (media.tagName !== 'IMG') { media.controls = true; media.autoplay = !album; media.loop = isVideo(ext); }
    else {
      frame.classList.add('viewer-image-frame');
      const canvas = el('div', { class: 'viewer-image-canvas' }, media);
      const viewport = el('div', { class: 'viewer-image-viewport' }, canvas);
      const imageView = imageViewport(media, viewport, canvas, { album });
      mediaDisposers.push(() => imageView.dispose());
      if (!album) viewport.addEventListener('click', e => {
        if (media.hidden || e.target !== media) return;
        const bounds = viewport.getBoundingClientRect(); navigate(e.clientX - bounds.left < bounds.width / 2 ? -1 : 1);
      });
      let showingOriginal = false;
      const original = action('Оригинал', () => {
        showingOriginal = !showingOriginal;
        if (showingOriginal) { fallback = true; imageView.original(); load(mediaURL(file?.url || source)); }
        else imageView.reset();
        original.textContent = showingOriginal ? 'Сброс' : 'Оригинал';
        original.title = showingOriginal ? 'Вписать изображение в область просмотра' : 'Загрузить оригинал и показать в масштабе 100%';
      });
      original.title = 'Загрузить оригинал и показать в масштабе 100%';
      frame.append(el('div', { class: 'viewer-image-tools' }, original), viewport);
    }
    frame.prepend(placeholder);
    if (media.tagName !== 'IMG') frame.append(media);
    {
      // Prefer the downloaded original before starting a remote transfer.
      api().LocalFileURL(page).then(url => {
        if (!closed && version === generation) { fallback = !!url; if (url) load(url); else return loadRemote(); }
      }).catch(() => loadRemote());
      return frame;
    }
  }
  function renderDetails(post, generation) {
    const artistNames = post.artists || [];
    title.textContent = `#${postId(post)} — ${artistNames.join(', ') || 'неизвестный автор'} · ${index + 1}/${posts.length}`;
    authors.replaceChildren(el('span', { text: artistNames.length ? artistNames.length === 1 ? 'Автор: ' : 'Авторы: ' : 'неизвестный автор' }));
    artistNames.forEach((name, i) => {
      if (i) authors.append(document.createTextNode(' · '));
      const link = el('span', { text: name }); authors.append(link);
      api().ArtistQuery(name, post).then(query => {
        if (!query || closed || version !== generation) return;
        link.replaceWith(el('button', { class: 'viewer-author-link', type: 'button', text: name, title: 'клик по имени автора — все его работы', onclick: () => openAuthor(post.site, query, name) }));
      }).catch(() => {});
    });
    const file = post.file, metadata = [];
    if (file) metadata.push(`${file.width && file.height ? `${file.width}×${file.height} ` : ''}${file.ext}${file.size ? ` · ${Math.round(file.size / 1024)} КБ` : ''}`);
    if (post.page_count > 1) metadata.push(`${post.page_count} ${post.extra?.files ? 'файлов' : 'изображений'}`);
    metadata.push(`▲ ${post.score || 0}   ♥ ${post.fav_count || 0}   рейтинг: ${post.rating || '?'}`);
    if (post.created_at) metadata.push(`загружено: ${new Date(post.created_at * 1000).toLocaleString('ru-RU')}`);
    info.textContent = metadata.join('   ·   ');
    tags.textContent = (post.tags || []).join(' '); tags.hidden = !post.tags?.length;
    caption.replaceChildren(postContent(post)); caption.classList.toggle('text-only', !post.file);
    clearCopyFeedback(); actions.replaceChildren();
    const prev = action('←', () => navigate(-1)); prev.disabled = index <= 0; prev.title = 'Предыдущий пост (←)';
    const next = action('→', () => navigate(1)); next.disabled = index >= posts.length - 1; next.title = 'Следующий пост (→)';
    primary = action('Скачать', primaryAction, true); primary.title = 'Скачать (Пробел)'; primary.disabled = busy || !post.file;
    const fav = action('☆ В избранное', async () => {
      if (fav.disabled) return;
      fav.disabled = true; favoriteRequest++;
      try {
        const on = await api().FavToggle(post);
        if (!closed && version === generation && fav === favoriteButton) setFavorite(fav, on);
        setStatus(on ? 'Добавлено в избранное' : 'Удалено из избранного');
        window.dispatchEvent(new Event('favorites-changed'));
      } finally { fav.disabled = false; }
    });
    favoriteButton = fav; fav.classList.add('viewer-favorite'); setFavorite(fav, false);
    refreshFavorite().catch(err => { if (!closed && version === generation) setStatus(String(err), true); });
    actions.append(prev, next, el('span', { class: 'viewer-position', text: `${index + 1} / ${posts.length}` }), primary,
      fav,
      action('Открыть на сайте', async () => { const url = await api().Permalink(post); if (url) window.runtime?.BrowserOpenURL(url); }),
      copyImageAction(generation),
      copyAction('Копировать теги', () => (post.tags || []).join(' '), 'Нет тегов', generation), action('Закрыть', close));
  }
  async function showPost() {
    scope?.close(); const requestScope = scope = new OperationScope(api());
    const generation = ++version, post = goldMediaPost(posts[index], state.filtersState[posts[index].site]); current = post; busy = true; pages = []; refreshButtons = [];
    stopMedia(); mediaWrap.replaceChildren(loading()); mediaWrap.scrollTop = 0;
    animateIn(mediaWrap, { duration: 140 });
    renderDetails(post, generation);
    try {
      let resolved = await requestScope.call('ResolvePost', post);
      resolved = goldMediaPost(resolved, state.filtersState[resolved.site]);
      if (closed || version !== generation) return;
      const expanded = await requestScope.call('ExpandPost', resolved);
      if (closed || version !== generation) return;
      current = resolved; posts[index] = resolved; pages = expanded?.length ? expanded : [resolved];
      mediaWrap.replaceChildren(); mediaWrap.classList.toggle('album', pages.length > 1);
      for (const page of pages) { const frame = mediaFrame(page, generation, pages.length > 1); if (frame) mediaWrap.append(frame); }
      mediaWrap.hidden = !mediaWrap.childElementCount;
      busy = false; renderDetails(resolved, generation);
      if (!resolved.file) observeView(caption.firstElementChild || caption, true);
      await refresh();
    } catch (err) {
      if (closed || version !== generation) return;
      busy = false; pages = [post]; mediaWrap.replaceChildren(); mediaWrap.hidden = false; mediaWrap.classList.remove('album');
      const frame = mediaFrame(post, generation, false); if (frame) mediaWrap.append(frame);
      mediaWrap.append(el('div', { class: 'viewer-load-error', text: `Не удалось получить вложения: ${err}` }), action('Повторить', showPost));
      renderDetails(post, generation); await refresh(); setStatus(String(err), true);
    }
  }
  function navigate(delta) {
    const next = Math.max(0, Math.min(posts.length - 1, index + delta));
    if (next !== index) { index = next; mediaWrap.hidden = false; showPost(); }
  }
  overlay.addEventListener('click', e => { if (e.target === overlay) close(); });
  overlay.addEventListener('keydown', e => {
    if (closed) return;
    // An author gallery can be opened above this viewer. Only its top modal handles shortcuts.
    if ([...document.querySelectorAll('.viewer-overlay')].filter(node=>!node.dataset.closing).at(-1) !== overlay) return;
    if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); close(); return; }
    if (e.target.closest('input,textarea,select,[contenteditable="true"]')) return;
    if (e.key === 'ArrowRight' || e.key === 'ArrowLeft') { e.preventDefault(); e.stopPropagation(); navigate(e.key === 'ArrowRight' ? 1 : -1); }
    if ((e.code === 'Space' || e.key === ' ') && !e.repeat) { e.preventDefault(); e.stopPropagation(); primaryAction().catch(err => setStatus(String(err), true)); }
  });
  const refreshEvent = () => { if (!closed) refresh(); };
  unbind.push(window.runtime?.EventsOn('job:finished', async () => {
    // Failed/cancelled jobs should allow retry as well as successful location reveal.
    try {
      const jobs = await api().Jobs();
      const active = new Set((jobs || []).filter(j => ['queued','running'].includes(j.status)).map(j => postKey({site:j.site,id:j.post_id})));
      for (const job of jobs || []) if (['failed', 'cancelled', 'done', 'skipped'].includes(job.status)) {
        const key = postKey({site:job.site,id:job.post_id});
        if (!active.has(key)) queued.delete(key);
        queued.delete(`${key}\0${job.page_index || 0}`);
      }
    } catch {}
    refreshEvent();
  }));
  unbind.push(window.runtime?.EventsOn('history:changed', refreshEvent));
  unbind.push(window.runtime?.EventsOn('library:changed', () => {
    if (closed || busy) return;
    const generation = ++version; stopMedia(); refreshButtons = []; mediaWrap.replaceChildren();
    for (const page of pages.length ? pages : [current]) { const frame=mediaFrame(page,generation,pages.length>1); if(frame)mediaWrap.append(frame); }
    renderDetails(current,generation); refreshEvent();
  }));
  const favoritesChanged = () => refreshFavorite().catch(err => { if (!closed) setStatus(String(err), true); });
  window.addEventListener('favorites-changed', favoritesChanged);
  unbind.push(() => window.removeEventListener('favorites-changed', favoritesChanged));
  const mediaPolicyChanged = () => { if (!closed && current.site === 'danbooru') showPost(); };
  window.addEventListener('media-policy-changed', mediaPolicyChanged);
  unbind.push(() => window.removeEventListener('media-policy-changed', mediaPolicyChanged));
  box.append(el('div', { class: 'viewer-header' }, title, closeBtn), mediaWrap, caption, authors, info, tags, actions);
  overlay.append(box); document.body.append(overlay); overlay.focus(); await showPost();
}
