import { mediaURL } from './contracts.js';

// Capture decoded viewer pixels, preserving their full dimensions and alpha.
// All viewer sources pass through the same-origin authenticated media bridge.
export function viewerImagePNG(mount) {
  const bounds = mount.getBoundingClientRect(), middle = bounds.top + bounds.height / 2;
  const candidates = [...mount.querySelectorAll('.viewer-frame img, .viewer-frame video')]
    .filter(media => !media.hidden && (media.tagName === 'IMG'
      ? media.naturalWidth > 0 && media.naturalHeight > 0
      : media.readyState >= 2 && media.videoWidth > 0 && media.videoHeight > 0));
  candidates.sort((a, b) => {
    const distance = media => { const rect = media.getBoundingClientRect(); return Math.abs(rect.top + rect.height / 2 - middle); };
    return distance(a) - distance(b);
  });
  const media = candidates[0];
  if (!media) return null;
  return mediaPNG(media);
}

// Encode a decoded <img>/<video> at its native size as base64 PNG.
export function mediaPNG(media) {
  const canvas = document.createElement('canvas');
  canvas.width = media.naturalWidth || media.videoWidth;
  canvas.height = media.naturalHeight || media.videoHeight;
  const context = canvas.getContext('2d');
  if (!context) throw new Error('Не удалось подготовить изображение');
  context.drawImage(media, 0, 0, canvas.width, canvas.height);
  const data = canvas.toDataURL('image/png');
  if (!data.startsWith('data:image/png;base64,') || data.length <= 22) throw new Error('Не удалось подготовить изображение');
  return data.slice(22);
}

const imageExts = ['jpg', 'jpeg', 'png', 'gif', 'webp', 'avif', 'bmp'];
const videoExts = ['webm', 'mp4', 'mov', 'm4v'];
export const copyablePost = post => {
  const ext = String(post?.file?.ext || '').toLowerCase();
  return !!post?.file && (imageExts.includes(ext) || videoExts.includes(ext));
};

function decode(url, video, timeout = 60000) {
  return new Promise((resolve, reject) => {
    const media = document.createElement(video ? 'video' : 'img');
    let timer;
    const done = (fn, value) => { clearTimeout(timer); media.onload = media.onerror = media.onloadeddata = media.onseeked = null; fn(value); };
    timer = setTimeout(() => done(reject, new Error('Изображение не загрузилось')), timeout);
    media.onerror = () => done(reject, new Error('Не удалось загрузить изображение'));
    if (video) {
      media.muted = true; media.preload = 'auto';
      media.onloadeddata = () => { try { media.currentTime = Math.min(0.1, (media.duration || 1) / 2); } catch { done(resolve, media); } };
      media.onseeked = () => done(resolve, media);
    } else media.onload = () => done(resolve, media);
    media.src = url;
  });
}

// Copy a post the same way as the viewer does: decoded pixels as PNG, the
// downloaded original first, then the original URL, then the sample/preview.
export async function copyPostImage(post, api) {
  if (!copyablePost(post)) throw new Error('У поста нет изображения');
  if (!api.ClipboardSetImage) throw new Error('Буфер обмена изображений недоступен');
  const ext = String(post.file.ext || '').toLowerCase(), video = videoExts.includes(ext);
  const urls = [];
  try { const local = await api.LocalFileURL?.(post); if (local) urls.push(local); } catch {}
  for (const url of [post.file.url, post.sample_url, post.preview_url]) {
    const media = url && mediaURL(url, url === post.file.url ? ext : '');
    if (media && !urls.includes(media)) urls.push(media);
  }
  let last;
  for (const url of urls) {
    try {
      const png = mediaPNG(await decode(url, video && url !== mediaURL(post.preview_url || '')));
      const copied = await api.ClipboardSetImage(png);
      if (copied === false) throw new Error('Не удалось скопировать изображение');
      return true;
    } catch (err) { last = err; }
  }
  throw last || new Error('Не удалось загрузить изображение');
}
