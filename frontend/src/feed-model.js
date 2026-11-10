import { postKey, hasAttachments } from './contracts.js';
import { runAuthorChecks } from './author-checks.js';

import { OperationScope } from './operations.js';
export function matchesFeedSource(post, source) {
  if (source.hide_text && !hasAttachments(post)) return false;
  const tags = new Set((post.tags || []).map(t=>String(t).toLowerCase()));
  const text = `${post.extra?.title || ''} ${post.extra?.content || ''} ${(post.tags || []).join(' ')}`.toLowerCase();
  const matches = term => source.text_match ? text.includes(String(term).toLowerCase()) : tags.has(String(term).toLowerCase());
  if (source.match_any?.length && !source.match_any.some(clause => (clause.tags || []).every(matches) && !(clause.exclude || []).some(matches))) return false;
  return (source.match_tags || []).every(matches) && !(source.exclude_tags || []).some(matches);
}

// Every source keeps its own engine cursor. Late replies from a previous filter
// cannot enter the new timeline; failed sources do not interrupt other sites.
export class FeedModel {
  constructor(api, sites, onChange = () => {}) { this.api = api; this.sites = sites; this.onChange = onChange; this.version = 0; this.posts = []; this.sources = []; this.errors = []; this.busy = false; }
  get hasMore() { return this.sources.some(s => s.more && !s.error); }
  async select(id) {
    this.scope?.close(); this.scope = new OperationScope(this.api);
    const version = ++this.version;
    this.posts = []; this.sources = []; this.errors = []; this.busy = true; this.id = id; this.completed = 0; this.total = 0; this.onChange();
    try {
      if (this.api.RefreshMetadata) await this.api.RefreshMetadata();
      const sources = await this.api.FeedSources(id);
      if (version !== this.version) return;
      this.sources = (sources || []).map(s => ({ ...s, cursor: 1, more: true, posts: [] }));
      this.busy = false;
      await this.more();
    } catch (e) { if (version === this.version) { this.errors.push({ name: 'Лента', error: String(e) }); this.busy = false; this.onChange(); } }
  }
  close() { this.version++; this.scope?.close(); this.busy = false; }
  async more() {
    return this.loadSources(this.sources.filter(s => s.more && !s.error));
  }
  async retryErrors() {
    if (this.busy || !this.errors.length) return;
    if (!this.sources.length) return this.select(this.id);
    const sources = this.sources.filter(s => s.more && s.error);
    for (const source of sources) source.error = false;
    return this.loadSources(sources, true);
  }
  async loadSources(sources, retry = false) {
    if (this.busy) return;
    const scope = this.scope, version = this.version, errors = retry ? [] : this.errors;
    // Keep the previous errors visible while retrying, then replace them with
    // the remaining failures. Successful sources retain their posts and cursors.
    this.busy = true; this.completed = 0; this.total = sources.length; this.onChange();
    const blockedHosts = new Set();
    const hostFor = s => this.sites.find(site => site.key === s.site)?.domain || s.site;
    await runAuthorChecks(sources, async source => {
      if (version !== this.version || blockedHosts.has(hostFor(source))) return;
      const seen = new Set(source.posts.map(postKey));
      const cursors = new Set();
      let added;
      do {
        const cursor = String(source.cursor);
        if (cursors.has(cursor)) throw new Error('Источник повторил курсор ленты');
        cursors.add(cursor);
        const page = await scope.call('FeedPage', source.site, source.query, source.cursor);
        if (version !== this.version) return;
        added = 0;
        for (const post of page.posts || []) {
          if (!matchesFeedSource(post,source) || seen.has(postKey(post))) continue;
          seen.add(postKey(post)); added++;
          source.posts.push({ ...post, feed_author: source.name });
        }
        source.more = !!page.has_more && page.next != null && String(page.next) !== cursor;
        source.cursor = page.next;
      } while (source.hide_text && !added && source.more);
      if (this.sources.some(s=>s.sort)) {
        // Site rankings aren't comparable. Interleave sources by API rank,
        // preserving each source's selected order across pages and OR branches.
        const merged = [], keys = new Set();
        const length = Math.max(0,...this.sources.map(s=>s.posts.length));
        for (let i=0;i<length;i++) for (const s of this.sources) {
          const p = s.posts[i];
          if (p && !keys.has(postKey(p))) { keys.add(postKey(p)); merged.push(p); }
        }
        this.posts = merged;
      } else {
        const keys = new Set(this.posts.map(postKey));
        for (const p of source.posts) if (!keys.has(postKey(p))) { keys.add(postKey(p)); this.posts.push(p); }
        // Stable sorting retains an API's newest-first order when dates are absent.
        this.posts.sort((a, b) => (Number(b.created_at) || 0) - (Number(a.created_at) || 0));
      }
    }, {
      hostFor,
      onError: (source, error) => {
        if (version !== this.version) return;
        source.error = true;
        if (/HTTP 429/.test(String(error))) {
          const host = hostFor(source);
          for (const other of this.sources) if (other.more && hostFor(other)===host) other.error=true;
          if (blockedHosts.has(host)) return;
          blockedHosts.add(host);
          errors.push({name:host,error:'HTTP 429 — запросы к сайту приостановлены. Повтори проверку позднее.'});
        } else errors.push({ name: source.name || source.site, error: String(error) });
      },
      onProgress: ({ completed, total }) => { if (version === this.version) { this.completed = completed; this.total = total; this.onChange(); } },
    });
    if (version === this.version) { this.errors = errors; this.busy = false; this.onChange(); }
  }
}
