import { el, setStatus } from './main.js';
import { postId, postKey, mediaURL } from './contracts.js';
import { postContent } from './grid.js';
import { standaloneAttachment } from './viewer.js';

import { OperationScope } from './operations.js';
import { forgetReveal } from './motion.js';
import { trackPostView } from './post-views.js';
import { locationLabel, isPostSubmitting, queueClock } from './download-state.js';
import { icon } from './icons.js';
import { mediaPlaceholder, fileKind } from './media-placeholder.js';
const images = new Set(['jpg','jpeg','png','webp','gif','avif','bmp']);
const videos = new Set(['mp4','webm','mov','m4v']);
const audio = new Set(['mp3','m4a','ogg','wav','flac']);
const archives = new Set(['zip','rar','7z']);
const loading = () => el('div', { class: 'feed-loading', role: 'status' }, el('span', { class: 'viewer-spinner' }), 'Загрузка…');
const unavailable = (label, ext = '') => el('div', { class: 'feed-unavailable' }, mediaPlaceholder(ext ? fileKind(ext) : 'image', { label, ext }));
function reserveMedia(frame, width, height) {
  if (!(width > 0 && height > 0)) return;
  frame.classList.add('reserved');
  frame.style.aspectRatio = `${width} / ${height}`;
}

export function feedCard(post, { api, open, menu, save, siteLabel, queries = () => [] }) {
  const scope = new OperationScope(api);
  let current = post, hydrated = false, hydration, firstAttachment, disposed = false;
  const attachmentUpdates = [];
  const viewTrackers = [];
  const author = post.artists?.join(', ') || post.feed_author || 'Неизвестный автор';
  const avatar = el('span', { class: 'feed-avatar', text: author.slice(0, 1).toUpperCase() });
  const header = el('header', { class: 'feed-post-head' }, avatar,
    el('div', {}, el('strong', { text: author }), el('div', { class: 'feed-source', text: siteLabel || post.site })),
    el('button', { class: 'feed-menu', text: '⋯', title: 'Действия с постом', onclick: e => menu(current, { clientX: e.clientX, clientY: e.clientY }) }));
  const media = el('div', { class: 'feed-media' });
  const caption = el('div', { class: 'feed-caption' }, postContent(post));
  const stamp = post.created_at ? new Date(post.created_at * 1000).toLocaleString('ru-RU', { day:'numeric',month:'long',year:'numeric',hour:'2-digit',minute:'2-digit' }) : `#${postId(post)}`;
  const status = el('span', { class: 'feed-download-state' });
  const article = el('article', { class: 'feed-post', dataset: { key: postKey(post) } }, header, media, caption,
    el('footer', { class: 'feed-post-foot' }, status, el('time', { text: stamp })));
  article.addEventListener('contextmenu', e => { e.preventDefault(); e.stopPropagation(); menu(current, e); });
  function observeView(node, ready = false) {
    const tracker = trackPostView(node,{post:()=>current,api:()=>api,queries});
    viewTrackers.push(tracker);
    if (ready) tracker.ready();
    return tracker;
  }
  function attachmentMenu(node, page) {
    node.addEventListener('contextmenu', e => {
      e.preventDefault(); e.stopPropagation();
      if (!disposed) menu(current,e,standaloneAttachment(page));
    });
    return node;
  }
  function part(page, preview) {
    const ext = String(page.file?.ext || '').toLowerCase();
    if (!page.file) return null;
    if (!images.has(ext) && !videos.has(ext) && !audio.has(ext)) {
      const selected = standaloneAttachment(page), label = archives.has(ext) ? 'Скачать архив' : 'Скачать вложение';
      const action = el('span', { class: 'feed-attachment-action', text: label });
      let submitting = false, request = 0;
      const button = el('button', { class: 'feed-attachment', title: label, onclick: async () => {
        if (submitting || disposed) return;
        submitting = true; button.disabled = true;
        try {
          if (await api.LocalMedia?.(selected)) await api.RevealPost(selected);
          else await save([selected]);
        } catch (error) { if (!disposed) setStatus(String(error), true); }
        finally { submitting = false; await refresh().catch(error => { if (!disposed) setStatus(String(error), true); }); }
      } },
        el('span', { class: 'feed-file-icon', dataset: { kind: fileKind(ext) } }, icon(archives.has(ext) ? 'archive' : fileKind(ext) === 'document' ? 'fileText' : 'file', 26)),
        el('span', {}, el('strong', { text: archives.has(ext) ? `Архив ${ext.toUpperCase()}` : `Вложение ${ext.toUpperCase()}` }),
          el('span', { class: 'feed-file-name', text: page.extra?.download_name || `#${postId(page)}` }), action));
      async function refresh() {
        const generation = ++request, path = await api.LocalMedia?.(selected);
        if (disposed || !article.isConnected || generation !== request) return;
        action.textContent = button.title = path ? locationLabel : label;
        button.disabled = submitting;
      }
      attachmentUpdates.push(refresh); refresh().catch(() => {});
      observeView(button, true);
      return attachmentMenu(button, page);
    }
    const kind = videos.has(ext) ? 'video' : audio.has(ext) ? 'audio' : 'img';
    const source = kind === 'img' && ext !== 'gif' ? page.sample_url || page.file.url || page.preview_url : page.file.url;
    if (!source) return attachmentMenu(unavailable('Нет доступного медиафайла'),page);
    const frame = attachmentMenu(el('div', { class: `feed-media-frame ${kind}` }),page), placeholder = loading();
    if (kind !== 'audio') reserveMedia(frame,page.file.width,page.file.height);
    // Every album image keeps a low-res stand-in until its original is decoded,
    // so frames never blink through an empty shimmer.
    if (!preview && kind === 'img' && page.preview_url) preview = el('img', { src:mediaURL(page.preview_url), alt:'', onerror:e => e.currentTarget.remove() });
    if (preview && kind === 'img') {
      preview.className='feed-retained-preview';frame.classList.add('has-preview');frame.append(preview);
      preview.addEventListener('click', () => open(current));
    }
    // The card itself is hydrated by IntersectionObserver. A hidden lazy image
    // never starts loading in Chromium, so media inside the visible card is eager.
    const node = el(kind, { alt:page.extra?.download_name || '', hidden:true, loading:'eager', preload:'none' });
    if (kind !== 'img') { node.controls = true; node.preload = 'metadata'; if (page.preview_url && kind === 'video') node.poster = mediaURL(page.preview_url); }
    else node.addEventListener('click', () => open(current));
    const viewed = observeView(node);
    let decoding = false;
    const show = () => {
      if(disposed || node.classList.contains('media-ready'))return;
      if (!frame.classList.contains('reserved') && kind !== 'audio') reserveMedia(frame,node.naturalWidth || node.videoWidth,node.naturalHeight || node.videoHeight);
      placeholder.remove(); preview?.remove(); frame.classList.remove('has-preview'); frame.classList.add('media-loaded');
      node.hidden = false; node.classList.add('media-ready'); if(kind==='img')viewed.ready();
    };
    // 'load' fires before the bitmap is decoded. Swapping the preview out at that
    // moment paints one empty frame, which is the visible blink on every image.
    const ready = () => {
      if(disposed || decoding || node.classList.contains('media-ready'))return;
      if (kind !== 'img' || typeof node.decode !== 'function') { show(); return; }
      decoding = true;
      node.decode().then(() => { decoding = false; show(); }, () => { decoding = false; if (node.naturalWidth) show(); });
    };
    node.addEventListener(kind === 'img' ? 'load' : 'loadedmetadata',ready);
    if (kind !== 'img') node.addEventListener('loadeddata',()=>viewed.ready());
    let fallback = false;
    node.addEventListener('error', () => {
      if(disposed)return;
      viewed.unready();
      if (!fallback && kind === 'img' && page.file.url && source !== page.file.url) { fallback = true; node.src = mediaURL(page.file.url); return; }
      node.hidden = true; node.classList.remove('media-ready'); placeholder.classList.add('failed'); placeholder.replaceChildren(icon(kind === 'video' ? 'film' : kind === 'audio' ? 'music' : 'imageOff', 22), el('span', { text:'Не удалось загрузить' }),
        el('button', { class:'pv-btn ghost', text:'Повторить', onclick: () => { node.classList.remove('media-ready'); node.src = mediaURL(fallback ? page.file.url : source, ext); } }));
      if (!placeholder.isConnected) frame.prepend(placeholder);
    });
    frame.append(placeholder,node);
    const load= url => {if(!disposed && article.isConnected)node.src=url || mediaURL(source,ext);};
    Promise.resolve(api.LocalFileURL?.(page)).then(load).catch(()=>load(''));
    return frame;
  }
  if (post.file) {
    if (post.preview_url && images.has(String(post.file.ext).toLowerCase())) {
      const frame = el('div',{class:'feed-media-frame initial-preview'});
      reserveMedia(frame,post.file.width,post.file.height);
      frame.classList.add('loading');
      frame.append(el('img', { class:'feed-preview', src:mediaURL(post.preview_url), loading:'lazy', alt:'',
        onload: () => frame.classList.remove('loading'),
        onerror: e => { frame.classList.remove('loading'); e.currentTarget.hidden = true; if (!frame.querySelector('.media-ph')) frame.append(mediaPlaceholder('image', { label: 'Нет превью' })); },
        oncontextmenu:async e => {
        e.preventDefault(); e.stopPropagation();
        await hydrate();
        if (!disposed && article.isConnected && hydrated && firstAttachment) menu(current,e,standaloneAttachment(firstAttachment));
      } }));
      media.append(frame);
    } else {
      const frame=el('div',{class:`feed-media-frame ${videos.has(String(post.file.ext).toLowerCase())?'video':''}`},loading());
      if (!audio.has(String(post.file.ext).toLowerCase())) reserveMedia(frame,post.file.width,post.file.height);
      media.append(frame);
    }
  } else media.hidden = true;
  function hydrate() {
    if (disposed || hydrated) return Promise.resolve();
    return hydration ||= hydrateParts().finally(()=> { hydration = null; });
  }
  async function hydrateParts() {
    try {
      current = await scope.call('ResolvePost', post);
      if (disposed || !article.isConnected) return;
      const parts = await scope.call('ExpandPost', current);
      if (disposed || !article.isConnected) return;
      hydrated = true;
      viewTrackers.splice(0).forEach(tracker=>tracker.dispose());
      // Move the already decoded preview instead of cloning it: a clone is a new
      // element that has to be decoded and painted again.
      const preview=media.querySelector('.feed-preview:not([hidden])');
      media.replaceChildren();
      const list = parts?.length ? parts : [current];
      firstAttachment = list.find(page=>page.file);
      media.classList.toggle('album', list.filter(p => images.has(String(p.file?.ext).toLowerCase())).length > 1);
      for (const [index,page] of list.entries()) { const frame = part(page,index===0?preview:null); if (frame) media.append(frame); }
      media.hidden = !media.childNodes.length;
      caption.replaceChildren(postContent(current));
      if (!current.file) observeView(caption.firstElementChild || caption, true);
    } catch (error) {
      if (disposed || !article.isConnected) return;
      media.hidden = false; media.replaceChildren(el('div', { class:'feed-unavailable', text:String(error) }),
        el('button', { class:'pv-btn ghost', text:'Повторить', onclick:hydrate }));
    }
  }
  function update(progress) {
    for (const p of [post, current]) { p.download_progress = progress; p.downloaded = progress?.state === 'complete'; }
    const value = progress?.queued || isPostSubmitting(post) ? 'queued' : progress?.state;
    if (value === 'queued') {
      if (!status.querySelector('.queue-clock')) status.replaceChildren(queueClock(),document.createTextNode(''));
      status.lastChild.textContent = ` В очереди${progress?.downloaded > 0 ? ` (${progress.downloaded}/${progress.total})` : ''}`;
    } else status.textContent = value === 'complete' ? '✓ Сохранено' : value === 'archive_only' ? 'ARC✓ Архив сохранён' : value === 'partial' ? `◐ ${progress.downloaded}/${progress.total}` : '';
    article.dataset.downloadState = value || 'none';
    for (const refresh of attachmentUpdates) refresh().catch(() => {});
  }
  function dispose() { disposed = true; viewTrackers.splice(0).forEach(tracker=>tracker.dispose()); forgetReveal(article); scope.close(); for (const node of media.querySelectorAll('img,video,audio')) { node.pause?.(); node.removeAttribute('src'); node.load?.(); } }
  return { node: article, hydrate, update, dispose, post };
}
