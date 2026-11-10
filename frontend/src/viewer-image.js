// Keep the scaled image in layout so every edge remains reachable by scrolling.
export function imageViewport(image, viewport, canvas, { album = false } = {}) {
  let ratio = 0, fit = 0, original = false, drag, suppressClick = false, resizeFrame;
  let active = !album;
  image.draggable = false;
  function setActive(value) {
    active = value;
    viewport.classList.toggle('image-active', value);
    if (!value && drag) finish({ pointerId: drag.id });
  }
  function outsidePointer(event) {
    if (!viewport.contains(event.target)) setActive(false);
  }
  if (album) {
    viewport.tabIndex = 0;
    viewport.title = 'Нажмите на изображение, чтобы перемещать его; клик вне изображения вернёт листание альбома';
    viewport.addEventListener('click', event => {
      if (event.target !== image || image.hidden) return;
      setActive(true); viewport.focus({ preventScroll: true });
    });
    viewport.addEventListener('blur', () => setActive(false));
    image.ownerDocument.addEventListener('pointerdown', outsidePointer, true);
  }
  function render(next, anchor) {
    const oldWidth = parseFloat(image.style.width) || 0;
    const oldHeight = parseFloat(image.style.height) || 0;
    const x = anchor?.x ?? viewport.clientWidth / 2;
    const y = anchor?.y ?? viewport.clientHeight / 2;
    const centerX = oldWidth ? (viewport.scrollLeft + x - Math.max(0, (viewport.clientWidth - oldWidth) / 2)) / oldWidth : .5;
    const centerY = oldHeight ? (viewport.scrollTop + y - Math.max(0, (viewport.clientHeight - oldHeight) / 2)) / oldHeight : .5;
    ratio = next;
    const width = image.naturalWidth * ratio, height = image.naturalHeight * ratio;
    image.style.width = canvas.style.width = `${width}px`;
    image.style.height = canvas.style.height = `${height}px`;
    viewport.scrollLeft = Math.max(0, centerX * width - x + Math.max(0, (viewport.clientWidth - width) / 2));
    viewport.scrollTop = Math.max(0, centerY * height - y + Math.max(0, (viewport.clientHeight - height) / 2));
    viewport.classList.toggle('can-pan', width > viewport.clientWidth || height > viewport.clientHeight);
  }
  function resize() {
    if (!image.naturalWidth || !image.naturalHeight) return;
    const width = viewport.offsetWidth || viewport.clientWidth;
    const height = viewport.offsetHeight || viewport.clientHeight;
    if (!width || (!album && !height)) return;
    const nextFit = Math.min(1, width / image.naturalWidth,
      album ? Infinity : height / image.naturalHeight);
    const zoom = fit ? ratio / fit : 1;
    fit = nextFit;
    if (album) viewport.style.height = `${image.naturalHeight * fit}px`;
    render(original ? 1 : fit * zoom);
  }
  function finish(event) {
    if (!drag || event.pointerId !== drag.id) return;
    const id = drag.id;
    drag = null;
    viewport.classList.remove('is-panning');
    if (image.hasPointerCapture?.(id)) image.releasePointerCapture(id);
  }
  viewport.addEventListener('pointerdown', event => {
    suppressClick = false;
    if (event.button !== 0 || !event.isPrimary || !active || !viewport.classList.contains('can-pan')) return;
    // Leave scrollbar interactions to the browser.
    if (event.target !== image && event.target !== canvas) return;
    drag = { id: event.pointerId, x: event.clientX, y: event.clientY, left: viewport.scrollLeft, top: viewport.scrollTop };
    image.setPointerCapture?.(event.pointerId);
  });
  viewport.addEventListener('pointermove', event => {
    if (!drag || drag.id !== event.pointerId) return;
    const dx = event.clientX - drag.x, dy = event.clientY - drag.y;
    if (!suppressClick && Math.hypot(dx, dy) < 4) return;
    suppressClick = true;
    event.preventDefault();
    viewport.classList.add('is-panning');
    viewport.scrollLeft = drag.left - dx;
    viewport.scrollTop = drag.top - dy;
  });
  for (const name of ['pointerup', 'pointercancel', 'lostpointercapture']) viewport.addEventListener(name, finish);
  viewport.addEventListener('click', event => {
    if (!suppressClick) return;
    event.preventDefault(); event.stopImmediatePropagation();
  }, true);
  image.addEventListener('dragstart', event => event.preventDefault());
  viewport.addEventListener('wheel', event => {
    if (album && !active) {
      // Inactive album viewports pass ordinary wheel scrolling to the album.
      // Swallow Ctrl+wheel until activation to avoid zooming the whole WebView.
      if (event.ctrlKey) event.preventDefault();
      return;
    }
    if (album && !event.ctrlKey) return;
    event.preventDefault();
    if (!ratio || !fit || !event.deltaY) return;
    const bounds = viewport.getBoundingClientRect();
    original = false;
    render(Math.min(Math.max(6 * fit, 1), Math.max(.25 * fit, ratio * (event.deltaY < 0 ? 1.15 : .87))),
      { x: event.clientX - bounds.left, y: event.clientY - bounds.top });
  }, { passive: false });
  image.addEventListener('load', resize);
  // Album height depends on its width. Defer writes outside the observer's
  // delivery phase so adjusting that height cannot create a resize loop.
  const observer = typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(() => {
    if (resizeFrame) return;
    resizeFrame = requestAnimationFrame(() => { resizeFrame = undefined; resize(); });
  });
  observer?.observe(viewport);
  return {
    original() { original = true; resize(); viewport.scrollLeft = viewport.scrollTop = 0; },
    reset() { if (album) setActive(false); original = false; ratio = fit; resize(); viewport.scrollLeft = viewport.scrollTop = 0; },
    dispose() { if (album) image.ownerDocument.removeEventListener('pointerdown', outsidePointer, true); observer?.disconnect(); if (resizeFrame) cancelAnimationFrame(resizeFrame); if (drag) finish({ pointerId: drag.id }); },
  };
}
