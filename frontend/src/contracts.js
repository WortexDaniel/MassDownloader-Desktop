import { discoverySite, normalizeDiscoveryFilters, normalizeDiscoveryQuery } from './discovery-filters.js';

export function postId(p) {
  const id = p?.id;
  if (id && typeof id === 'object') return String(id.IsNu ? id.Num : id.Str);
  return id == null ? '' : String(id);
}
export function postKey(p) { return `${p.site}\0${postId(p)}`; }
export function hasAttachments(post) {
  return !!post?.file || Number(post?.attachment_count) > 0 || Number(post?.extra?.locked_count) > 0
    || ['files', 'media', 'attachments'].some(key => Array.isArray(post?.extra?.[key]) && post.extra[key].length > 0);
}
export function postVisible(post, filters = {}) { return !filters[post.site]?.hide_text || hasAttachments(post); }
export function mediaURL(url, ext = '') {
  const video = ['webm', 'mp4', 'mov', 'm4v'].includes(ext.toLowerCase());
  const image = ['jpg','jpeg','png','gif','webp','avif','bmp'].includes(ext.toLowerCase()) || !ext && /(?:\.(?:jpg|jpeg|png|gif|webp|avif|bmp)(?:\?|$)|^danbooru-media:)/i.test(url || '');
  return url ? `/media?url=${encodeURIComponent(url)}${video ? `&video_ext=${ext.toLowerCase()}` : image ? '&image_cache=1' : ''}` : '';
}
export function normalizeFilterValue(f,v) {
 if(f.kind==='threshold'&&Array.isArray(v)) return {enabled:!!v[0],op:v[1]||'>=',n:Number(v[2])||0};
 return v;
}
export function initialFilters(sites,saved,settings={}) {
 const value=saved&&Object.keys(saved).length?saved:{artists:settings.artists||[],include:settings.include_tags||[],exclude:settings.exclude_tags||[],score:[!!settings.score_enabled,settings.score_op||'>=',settings.min_score||100],faves:[!!settings.faves_enabled,settings.faves_op||'>=',settings.min_faves||50],rating:settings.rating==='any'?'':settings.rating||''};
 const nested=Object.keys(value).every(k=>sites.some(s=>s.key===k));
 const result={};
 for(const site of sites){const raw=nested?value[site.key]:value;if(!raw)continue;result[site.key]={};for(const f of site.filters||[]) if(Object.hasOwn(raw,f.key))result[site.key][f.key]=normalizeFilterValue(f,raw[f.key]);}
 for(const site of sites) if(result[site.key]) normalizeDiscoveryFilters(site,result[site.key]);
 return result;
}
export function buildQuery(site, vals) {
  const tokens = [];
  for (const f of site?.filters || []) {
    const v = vals[f.key];
    if (f.local_only || (f.enabled_if && !vals[f.enabled_if])) continue;
    switch (f.kind) {
      case 'tags': tokens.push(...(Array.isArray(v) ? v : [])); break;
      case 'exclude': tokens.push(...(Array.isArray(v) ? v.map(t => '-' + t) : [])); break;
      case 'authors': {
        const list = Array.isArray(v) ? v : [];
        if (site.engine === 'pixiv') {
          tokens.push(...list.map(a => 'user_id:' + a.trim().replace(/^user_id:/, '')));
          break;
        }
        const prefix = { twitter: 'from:', misskey: 'user_id:', furaffinity: 'by:', deviantart: 'by:' }[site.engine];
        if (prefix) tokens.push(...list.map(a => prefix + a.replace(/^[@~]+/, '')));
        else tokens.push(...list.map(a => list.length > 1 ? '~' + a : a));
        break;
      }
      case 'threshold': { const val=normalizeFilterValue(f,v);if (val?.enabled) tokens.push(`${f.token}:${val.op === '=' ? '' : val.op}${val.n}`);break; }
      case 'choice': if (v) tokens.push((f.token ? f.token + ':' : '') + (f.value_map?.[v] ?? v)); break;
      case 'text': tokens.push(...String(v || '').split(/[\s,]+/).filter(Boolean).map(t => `${f.token}:${t}`)); break;
      case 'bool': if (v ? f.token : f.false_token) tokens.push(v ? f.token : f.false_token); break;
    }
  }
  return discoverySite(site) ? normalizeDiscoveryQuery(tokens.join(' ')) : tokens.join(' ');
}

// Gold tags select media routing; they are never search constraints.
export function goldMediaPost(post, values = {}) {
  if (post?.site !== 'danbooru') return post;
  const wanted = new Set((values.gold_tags || []).map(tag => String(tag).trim().toLowerCase()).filter(Boolean));
  const force = !!values.use_gold && (post.tags || []).some(tag => wanted.has(String(tag).trim().toLowerCase()));
  const rewrite = ref => {
    if (!ref?.startsWith('danbooru-media://')) return ref;
    try {
      const url = new URL(ref), site = url.searchParams.get('site');
      if (site && site !== 'danbooru') return ref;
      if (force) url.searchParams.set('force_assets', '1'); else url.searchParams.delete('force_assets');
      return url.href;
    } catch { return ref; }
  };
  return { ...post, preview_url: rewrite(post.preview_url), sample_url: rewrite(post.sample_url),
    file: post.file ? { ...post.file, url: rewrite(post.file.url) } : post.file };
}
export function encodeBytes(bytes) {
  let binary = '';
  for (let i = 0; i < bytes.length; i += 32768) binary += String.fromCharCode(...bytes.subarray(i, i + 32768));
  return btoa(binary);
}
export function selectionRange(posts, from, to) {
  const a = posts.findIndex(p => postKey(p) === from), b = posts.findIndex(p => postKey(p) === to);
  return a < 0 || b < 0 ? [to] : posts.slice(Math.min(a, b), Math.max(a, b) + 1).map(postKey);
}

// Inverse of buildQuery for booru-style grammars. Returns the filter values
// that rebuild an equivalent query (same token multiset), or null with the
// tokens the sidebar cannot represent.
const tokensOf = query => String(query || '').trim().split(/\s+/).filter(Boolean);
const escapeRegExp = text => String(text).replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
export function parseQuery(site, query, previous = {}) {
  if (!site || site.tag_phrase) return { values: null, unmatched: tokensOf(query) };
  const vals = structuredClone(previous || {});
  const active = (site.filters || []).filter(f => !f.local_only && !(f.enabled_if && !vals[f.enabled_if]));
  const byKind = kind => active.find(f => f.kind === kind);
  const authorPrefix = { pixiv: 'user_id:', twitter: 'from:', misskey: 'user_id:', furaffinity: 'by:', deviantart: 'by:' }[site.engine];
  const knownAuthors = new Set((byKind('authors') && Array.isArray(previous?.[byKind('authors').key]) ? previous[byKind('authors').key] : []).map(a => String(a).toLowerCase()));
  for (const f of active) {
    switch (f.kind) {
      case 'tags': case 'exclude': case 'authors': vals[f.key] = []; break;
      case 'threshold': {
        const old = normalizeFilterValue(f, vals[f.key]) || {};
        vals[f.key] = { op: old.op || (f.op_tokens || '>=').split(' ')[0], n: old.n ?? f.default_value ?? 0, enabled: false };
        break;
      }
      case 'choice': if ((f.options || []).some(o => o.value === '')) vals[f.key] = ''; break;
      case 'text': vals[f.key] = ''; break;
      case 'bool': if (f.token) vals[f.key] = false; else if (f.false_token) vals[f.key] = true; break;
    }
  }
  const threshold = f => new RegExp(`^${escapeRegExp(f.token)}:(${(f.op_tokens || '>= > =').split(' ').map(escapeRegExp).join('|')})?(-?\\d+)$`, 'i');
  for (const token of tokensOf(query)) {
    let matched = false;
    for (const f of active) {
      if (f.kind === 'bool' && f.token && token === f.token) { vals[f.key] = true; matched = true; }
      else if (f.kind === 'bool' && f.false_token && token === f.false_token) { vals[f.key] = false; matched = true; }
      else if (f.kind === 'threshold' && f.token) {
        const m = token.match(threshold(f));
        if (m) { vals[f.key] = { ...vals[f.key], enabled: true, op: m[1] || '=', n: Number(m[2]) }; matched = true; }
      } else if (f.kind === 'choice') {
        const raw = f.token ? (token.toLowerCase().startsWith(f.token.toLowerCase() + ':') ? token.slice(f.token.length + 1) : null) : token;
        const option = raw == null ? null : (f.options || []).find(o => o.value !== '' && (f.value_map?.[o.value] ?? o.value) === raw);
        if (option) { vals[f.key] = option.value; matched = true; }
      } else if (f.kind === 'text' && f.token && token.toLowerCase().startsWith(f.token.toLowerCase() + ':')) {
        vals[f.key] = [vals[f.key], token.slice(f.token.length + 1)].filter(Boolean).join(' '); matched = true;
      } else if (f.kind === 'authors') {
        if (authorPrefix && token.startsWith(authorPrefix)) { vals[f.key].push(token.slice(authorPrefix.length)); matched = true; }
        else if (!authorPrefix && token.length > 1 && token.startsWith('~')) { vals[f.key].push(token.slice(1)); matched = true; }
        else if (!authorPrefix && knownAuthors.has(token.toLowerCase())) { vals[f.key].push(token); matched = true; }
      }
      if (matched) break;
    }
    if (matched) continue;
    // Grouping syntax ({ }, ( ), bare ~) has no sidebar representation.
    if (token === '~' || /[{}()]/.test(token)) continue;
    const exclude = byKind('exclude'), include = byKind('tags');
    if (exclude && token.length > 1 && token.startsWith('-')) vals[exclude.key].push(token.slice(1));
    else if (include) vals[include.key].push(token);
  }
  if (discoverySite(site)) normalizeDiscoveryFilters(site, vals);
  const want = tokensOf(discoverySite(site) ? normalizeDiscoveryQuery(query) : query);
  const got = tokensOf(buildQuery(site, vals));
  const count = new Map();
  for (const t of got) count.set(t, (count.get(t) || 0) + 1);
  const unmatched = [];
  for (const t of want) { const n = count.get(t) || 0; if (n) count.set(t, n - 1); else unmatched.push(t); }
  const extra = [...count.values()].some(n => n > 0);
  return unmatched.length || extra ? { values: null, unmatched } : { values: vals, unmatched: [] };
}
