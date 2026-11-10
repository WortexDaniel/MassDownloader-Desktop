// e621/e6ai discovery modes share one order slot with normal sorting.
export const discoverySite = site => ['e621', 'e6ai'].includes(site?.key);
const orderToken = token => /^-?order:/i.test(token);
const dateToken = token => /^[-~]?date:/i.test(token);

export function normalizeDiscoveryQuery(query) {
  let tokens = [];
  for (const token of String(query || '').trim().split(/\s+/).filter(Boolean)) {
    if (orderToken(token)) tokens = tokens.filter(old => !orderToken(old));
    if (token.toLowerCase() === 'order:popular') tokens = tokens.filter(old => !dateToken(old));
    if (dateToken(token)) tokens = tokens.filter(old => old.toLowerCase() !== 'order:popular');
    tokens.push(token);
  }
  return tokens.join(' ');
}

function removeTags(values, predicate) {
  for (const key of ['include', 'exclude', 'artists']) {
    if (Array.isArray(values[key])) values[key] = values[key].filter(tag => !predicate(key === 'exclude' ? '-' + tag : tag));
  }
}

export function syncDiscoveryOrder(values, query) {
  const order = String(query).split(/\s+/).find(orderToken)?.toLowerCase();
  values.hot = order === 'order:hot' || order === 'order:rank';
  values.popular = order === 'order:popular';
  values.sort = ['order:id', 'order:id_asc', '-order:id_desc'].includes(order) ? 'id' : '';
}

// Deterministic recovery for saved settings lacking an action timestamp.
export function normalizeDiscoveryFilters(site, values) {
  if (!discoverySite(site)) return values;
  if (values.popular) {
    values.hot = false; values.sort = '';
    removeTags(values, token => orderToken(token) || dateToken(token));
  } else if (values.hot) {
    values.popular = false; values.sort = '';
    removeTags(values, orderToken);
  } else if (values.sort) {
    removeTags(values, orderToken);
  } else {
    const raw = [...(values.artists || []), ...(values.include || []), ...(values.exclude || []).map(t => '-' + t)].join(' ');
    const normalized = normalizeDiscoveryQuery(raw);
    const keep = new Set(normalized.split(/\s+/));
    removeTags(values, token => (orderToken(token) || dateToken(token)) && !keep.has(token));
    const order = normalized.split(/\s+/).find(orderToken);
    if (order && ['order:hot', 'order:rank', 'order:popular', 'order:id', 'order:id_asc', 'order:id_desc'].includes(order.toLowerCase())) {
      syncDiscoveryOrder(values, normalized);
      removeTags(values, orderToken);
    }
  }
  return values;
}

// Called after assigning a value. Returns whether controls/chips need refreshing.
export function changeDiscoveryFilter(site, values, key, token = '') {
  if (!discoverySite(site)) return false;
  if (key === 'sort' || (['hot', 'popular'].includes(key) && values[key])) {
    if (key === 'sort') { values.hot = false; values.popular = false; }
    else { values[key === 'hot' ? 'popular' : 'hot'] = false; values.sort = ''; }
    removeTags(values, t => orderToken(t) || key === 'popular' && dateToken(t));
    return true;
  }
  if (orderToken(token)) {
    removeTags(values, orderToken);
    syncDiscoveryOrder(values, token);
    if (!values.hot && !values.popular && !values.sort && token.toLowerCase() !== 'order:id_desc') {
      values.include = [...(values.include || []), token];
    }
    if (values.popular) removeTags(values, dateToken);
    return true;
  }
  if (dateToken(token) && values.popular) {
    values.popular = false;
    removeTags(values, t => t.toLowerCase() === 'order:popular');
    return true;
  }
  return false;
}

export function changeManualDiscovery(query, key, value) {
  let tokens = String(query).split(/\s+/).filter(Boolean);
  if (['hot', 'popular', 'sort'].includes(key)) {
    tokens = tokens.filter(t => !orderToken(t) && !(key === 'popular' && value && dateToken(t)));
    if (key === 'sort' && value) tokens.push('order:' + value);
    else if (key !== 'sort' && value) tokens.push('order:' + key);
  }
  return normalizeDiscoveryQuery(tokens.join(' '));
}
