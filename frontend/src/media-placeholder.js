// Stand-ins for media that has no preview (text posts, archives, documents)
// or is still loading. Shared by the grid, feed, viewer and library.
import { icon } from './icons.js';

const SETS = {
  image: ['jpg', 'jpeg', 'png', 'gif', 'webp', 'avif', 'bmp', 'tif', 'tiff', 'jxl', 'heic'],
  video: ['webm', 'mp4', 'mov', 'm4v', 'mkv', 'avi'],
  audio: ['mp3', 'm4a', 'ogg', 'wav', 'flac', 'opus', 'aac'],
  archive: ['zip', 'rar', '7z', 'tar', 'gz', 'tgz', 'bz2', 'xz'],
  document: ['pdf', 'txt', 'doc', 'docx', 'rtf', 'odt', 'epub', 'md', 'html', 'psd', 'clip', 'kra', 'sai', 'xcf', 'swf', 'blend', 'obj', 'fbx'],
};
const META = {
  text: { icon: 'fileText', label: 'Текст' },
  image: { icon: 'imageOff', label: 'Нет превью' },
  video: { icon: 'film', label: 'Видео' },
  audio: { icon: 'music', label: 'Аудио' },
  archive: { icon: 'archive', label: 'Архив' },
  document: { icon: 'file', label: 'Документ' },
  file: { icon: 'file', label: 'Файл' },
  attachments: { icon: 'paperclip', label: 'Вложения' },
  locked: { icon: 'lock', label: 'Закрыто' },
};

export function fileKind(ext) {
  const value = String(ext || '').toLowerCase();
  for (const [kind, list] of Object.entries(SETS)) if (list.includes(value)) return kind;
  return 'file';
}
const attachmentCount = post => Number(post?.attachment_count) || ['files', 'media', 'attachments']
  .reduce((n, key) => n + (Array.isArray(post?.extra?.[key]) ? post.extra[key].length : 0), 0);
export function postKind(post) {
  if (post?.file) return fileKind(post.file.ext);
  if (attachmentCount(post) > 0) return 'attachments';
  if (Number(post?.extra?.locked_count) > 0) return 'locked';
  return 'text';
}
// First words of the post: title, otherwise the caption with markup removed.
export function postSnippet(post, max = 140) {
  const raw = post?.extra?.title || String(post?.extra?.content || '').replace(/<br\s*\/?>/gi, ' ').replace(/<[^>]*>/g, ' ');
  const text = raw.replace(/&nbsp;/g, ' ').replace(/&amp;/g, '&').replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"').replace(/\s+/g, ' ').trim();
  return text.length > max ? text.slice(0, max - 1).trimEnd() + '…' : text;
}

const node = (tag, cls, text) => { const n = document.createElement(tag); n.className = cls; if (text) n.textContent = text; return n; };

// kind: one of META keys; ext adds a format badge; title a short text excerpt.
export function mediaPlaceholder(kind, { ext = '', title = '', sub = '', label = '', size = 30 } = {}) {
  const meta = META[kind] || META.file;
  const root = node('div', 'media-ph');
  root.dataset.kind = kind;
  const head = node('div', 'mp-head');
  const badge = node('span', 'mp-icon'); badge.append(icon(meta.icon, size));
  head.append(badge);
  const caption = node('div', 'mp-caption');
  caption.append(node('span', 'mp-label', label || meta.label));
  if (ext) caption.append(node('span', 'mp-ext', String(ext).toUpperCase()));
  head.append(caption);
  root.append(head);
  if (title) root.append(node('p', 'mp-title', title));
  if (sub) root.append(node('span', 'mp-sub', sub));
  root.setAttribute('role', 'img');
  root.setAttribute('aria-label', [label || meta.label, ext && String(ext).toUpperCase(), title].filter(Boolean).join(' · '));
  return root;
}
export function postPlaceholder(post, options = {}) {
  const kind = postKind(post), count = attachmentCount(post), locked = Number(post?.extra?.locked_count) || 0;
  const sub = [count > 0 && kind === 'attachments' ? `${count} ${plural(count, 'файл', 'файла', 'файлов')}` : '',
    locked > 0 ? `закрыто: ${locked}` : ''].filter(Boolean).join(' · ');
  return mediaPlaceholder(kind, { ext: post?.file?.ext && kind !== 'image' ? post.file.ext : '', title: postSnippet(post), sub, ...options });
}
export function plural(n, one, few, many) {
  const a = Math.abs(n) % 100, b = a % 10;
  return a > 10 && a < 20 ? many : b > 1 && b < 5 ? few : b === 1 ? one : many;
}
// Shimmer shown while a preview loads; the icon hints what is coming.
export function mediaSkeleton(kind = 'image', size = 26) {
  const root = node('div', 'media-skel');
  root.setAttribute('aria-hidden', 'true');
  root.append(icon(kind === 'video' ? 'film' : kind === 'audio' ? 'music' : 'image', size));
  return root;
}
