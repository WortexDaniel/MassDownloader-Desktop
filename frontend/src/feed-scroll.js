// Keep the same visible post across asynchronous media/album layout changes.
// Browser anchoring is disabled on this scroller, so every correction has one owner.
export class FeedScrollAnchor {
  constructor(scroll, timeline, bottomUp) {
    this.scroll = scroll;
    this.timeline = timeline;
    this.bottomUp = bottomUp;
    this.pinned = bottomUp();
    this.lastTop = scroll.scrollTop || 0;
    this.onScroll = () => {
      if (Math.abs(this.scroll.scrollTop - this.lastTop) < .5) return;
      if (this.pinned && this.distanceFromBottom() > 2) this.pinned = false;
      // A user may scroll between ResizeObserver delivery and our animation frame.
      // Preserve that movement while compensating the pending layout change.
      if (this.frame != null) this.correct();
      this.pinned = this.bottomUp() && this.distanceFromBottom() <= 2;
      this.remember();
    };
    scroll.addEventListener('scroll', this.onScroll, { passive: true });
    this.resize = typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(() => this.schedule());
    this.resize?.observe(scroll);
    this.resize?.observe(timeline);
  }
  distanceFromBottom() { return this.scroll.scrollHeight - this.scroll.clientHeight - this.scroll.scrollTop; }
  observe(node) { this.resize?.observe(node); }
  unobserve(node) { this.resize?.unobserve(node); }
  capture() {
    const nodes = this.timeline.children, top = this.scroll.getBoundingClientRect().top;
    // Cards are in visual order in either display direction. Avoid scanning the
    // entire accumulated feed on every scroll event.
    let low = 0, high = nodes.length;
    while (low < high) {
      const middle = (low + high) >>> 1;
      if (nodes[middle].getBoundingClientRect().bottom <= top) low = middle + 1;
      else high = middle;
    }
    const node = nodes[low];
    return node ? { node, top: node.getBoundingClientRect().top - top } : null;
  }
  remember() { this.anchor = this.capture(); this.lastTop = this.scroll.scrollTop || 0; }
  schedule() {
    if (this.frame != null || this.closed) return;
    const request = window.requestAnimationFrame?.bind(window) || (fn => setTimeout(fn, 16));
    this.frame = request(() => { this.frame = null; this.correct(); });
  }
  correct() {
    if (this.closed || !this.scroll.isConnected) return;
    if (this.bottomUp() && this.pinned) {
      this.scroll.scrollTop = Math.max(0, this.scroll.scrollHeight - this.scroll.clientHeight);
    } else if (this.anchor?.node.isConnected) {
      const moved = this.scroll.scrollTop - this.lastTop;
      const top = this.anchor.node.getBoundingClientRect().top - this.scroll.getBoundingClientRect().top;
      const delta = top - (this.anchor.top - moved);
      if (Math.abs(delta) > .5) this.scroll.scrollTop += delta;
    }
    this.remember();
  }
  beforeUpdate() { this.correct(); return this.capture(); }
  afterUpdate(anchor, reset = false) {
    if (reset) {
      this.pinned = this.bottomUp();
      this.scroll.scrollTop = this.pinned ? Math.max(0, this.scroll.scrollHeight - this.scroll.clientHeight) : 0;
    } else {
      this.anchor = anchor;
      this.correct();
    }
    this.remember();
  }
  close() {
    this.closed = true;
    this.resize?.disconnect();
    this.scroll.removeEventListener('scroll', this.onScroll);
    if (this.frame != null) (window.cancelAnimationFrame?.bind(window) || clearTimeout)(this.frame);
  }
}
