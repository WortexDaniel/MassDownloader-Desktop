import { setStatus } from './main.js';

// A load event alone is prefetch, not a view. Observe the actual viewport and
// ancestor clipping without the feed's hydration margin.
const tracked = new Map();
let observer, cleanup;
function visibleModal(node) {
  const top = [...document.querySelectorAll('.viewer-overlay')].filter(n=>!n.dataset.closing).at(-1);
  return !top || top.contains(node);
}
function submit(item) {
  if (item.sent || !item.ready || !item.visible || !item.node.isConnected || item.node.closest?.('[hidden]') || document.visibilityState === 'hidden' || !visibleModal(item.node)) return;
  const backend = item.api();
  if (!backend?.MarkPostViewed) return;
  item.sent = true;
  Promise.resolve(backend.MarkPostViewed(item.post(), item.queries())).then(changed=> {
    if (changed) window.dispatchEvent(new Event('favorites-changed'));
  }).catch(error=> { item.sent = false; setStatus(`Учёт просмотра: ${error}`,true); });
}
function release(node) { observer?.unobserve(node); tracked.delete(node); }
export function trackPostView(node, { post, api, queries = () => [] }) {
  const item = {node, post, api, queries, ready:false,visible:false,sent:false};
  tracked.set(node,item);
  if (typeof IntersectionObserver !== 'undefined') {
    observer ||= new IntersectionObserver(entries=> {
      for (const entry of entries) {
        const value = tracked.get(entry.target);
        if (value) { value.visible = entry.isIntersecting && entry.intersectionRatio > 0; submit(value); }
      }
    }, {threshold:.01});
    queueMicrotask(()=> { if(tracked.has(node) && node.isConnected) observer.observe(node); });
  }
  const Mutation = globalThis.MutationObserver || window.MutationObserver;
  if (!cleanup && Mutation) {
    cleanup = new Mutation(()=> {
      for(const [target,value] of tracked) { if(!target.isConnected) release(target); else submit(value); }
    });
    cleanup.observe(document.body,{childList:true,subtree:true});
    document.addEventListener('visibilitychange',()=> { for(const item of tracked.values()) submit(item); });
  }
  return {
    ready() {
      if (!tracked.has(node)) return;
      item.ready = true;
      if (typeof IntersectionObserver === 'undefined' && node.getBoundingClientRect) {
        const r = node.getBoundingClientRect();
        item.visible = r.width > 0 && r.height > 0 && r.bottom > 0 && r.right > 0 && r.top < window.innerHeight && r.left < window.innerWidth;
      }
      submit(item);
    },
    unready() { item.ready = false; },
    dispose() { release(node); },
  };
}
