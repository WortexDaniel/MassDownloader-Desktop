import { el } from './main.js';
import { animateIn } from './motion.js';

// Reserve the image's space before fetching/decoding to keep the dialog stable.
export function reversePreview(src, { alt = '', emptyText = 'Нет превью', lazy = false, onReady } = {}) {
  const frame = el('div', { class: 'rev-image-frame', 'data-state': src ? 'loading' : 'empty', 'aria-busy': String(!!src) });
  const placeholder = el('div', { class: 'rev-image-placeholder', role: 'status' },
    src ? el('span', { class: 'viewer-spinner', 'aria-hidden': 'true' }) : null,
    el('span', { text: src ? 'Загрузка…' : emptyText }));
  frame.append(placeholder);
  let disposed = false;
  const img = src ? el('img', { alt, decoding: 'async', loading: lazy ? 'lazy' : 'eager' }) : null;
  frame.disposePreview = () => { disposed = true; img?.removeAttribute('src'); };
  if (img) {
    img.addEventListener('load', () => {
      if (disposed) return;
      frame.dataset.state = 'ready'; frame.setAttribute('aria-busy', 'false'); placeholder.remove();
      animateIn(img, { duration: 180 }); onReady?.(img);
    });
    img.addEventListener('error', () => {
      if (disposed) return;
      frame.dataset.state = 'error'; frame.setAttribute('aria-busy', 'false');
      img.remove(); placeholder.replaceChildren(el('span', { text: 'Превью недоступно' }));
    });
    frame.append(img); img.src = src;
  }
  return frame;
}
