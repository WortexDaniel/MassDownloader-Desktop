// Shared motion policy. Rendering and cancellation never depend on animation support.
const modes = new Set(['full', 'minimal', 'off']);
const transitions = new WeakMap();
const revealOptions = new WeakMap();
let revealObserver;

export function applyMotion(value) {
  document.documentElement.dataset.motion = modes.has(value) ? value : 'full';
  // A settings change also stops animations that are already in flight.
  if (!motionDuration(1)) {
    for (const animation of document.getAnimations?.() || []) animation.cancel();
  }
}

export function motionDuration(duration) {
  const mode = document.documentElement.dataset.motion || 'full';
  if (mode === 'off' || window.matchMedia?.('(prefers-reduced-motion: reduce)').matches) return 0;
  return mode === 'minimal' ? Math.min(duration, 100) : duration;
}

export function animateIn(node, { delay = 0, duration = 180, translate = true } = {}) {
  transitions.get(node)?.cancel();
  const time = motionDuration(duration);
  if (!time || !node.animate || !node.isConnected) return;
  const minimal = document.documentElement.dataset.motion === 'minimal';
  const animation = node.animate([
    { opacity: 0, transform: minimal || !translate ? 'none' : 'translateY(6px)' },
    { opacity: 1, transform: 'none' },
  ], { duration: time, delay: minimal ? 0 : Math.min(delay, 70), easing: 'cubic-bezier(.2,.8,.2,1)' });
  transitions.set(node, animation);
  animation.finished.catch(() => {}).finally(() => {
    if (transitions.get(node) === animation) transitions.delete(node);
  });
}

export function revealOnce(node, options = {}) {
  revealOptions.set(node,options);
  // Offscreen cards stay visible in their normal layout and animate only once.
  queueMicrotask(() => {
    if (!node.isConnected || !node.animate || !motionDuration(1)) return;
    if (!window.IntersectionObserver) { animateIn(node,options); return; }
    revealObserver ??= new window.IntersectionObserver(entries => {
      let order = 0;
      for (const entry of entries) if (entry.isIntersecting || !entry.target.isConnected) {
        revealObserver.unobserve(entry.target);
        if (entry.target.isConnected) animateIn(entry.target, { ...revealOptions.get(entry.target), delay: order++ * 12 });
      }
    });
    revealObserver.observe(node);
  });
}

export function forgetReveal(node) { revealObserver?.unobserve(node); }

export function dismissOverlay(overlay) {
  if (overlay.dataset.closing) return;
  overlay.dataset.closing = 'true';
  overlay.inert = true;
  overlay.style.pointerEvents = 'none';
  overlay.setAttribute('aria-hidden', 'true');
  const duration = motionDuration(130);
  if (!duration || !overlay.animate) { overlay.remove(); return; }
  const animation = overlay.animate([{ opacity: 1 }, { opacity: 0 }], { duration, easing: 'ease-out', fill: 'forwards' });
  if (document.documentElement.dataset.motion !== 'minimal') overlay.firstElementChild?.animate?.(
    [{ transform: 'none' }, { transform: 'translateY(6px) scale(.98)' }], { duration, easing: 'ease-in', fill: 'forwards' });
  animation.finished.catch(() => {}).finally(() => overlay.remove());
}
