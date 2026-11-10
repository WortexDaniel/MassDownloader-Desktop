import { OperationScope } from './operations.js';
import { applyMotion, animateIn } from './motion.js';
import { buildQuery, initialFilters, normalizeFilterValue, parseQuery } from './contracts.js';
import { icon, iconHTML } from './icons.js';
import { titlebar } from './titlebar.js';
import { applyViewPrefs } from './view-prefs.js';
import { discoverySite, normalizeDiscoveryFilters, normalizeDiscoveryQuery, syncDiscoveryOrder, changeDiscoveryFilter, changeManualDiscovery } from './discovery-filters.js';
// MassDownloader frontend — main window shell: sidebar filters, query line,
// run summary and the tabbed content area.
export const state = {
  sites: [],
  currentSite: null,
  filtersState: {},   // per-site {filter_key: value}
  query: '',
  manualQuery: null,
};

// -- tiny DOM helper ---------------------------------------------------------
export function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === null || v === undefined || v === false) continue;
    if (k === 'class') node.className = v;
    else if (k === 'innerHTML') node.innerHTML = v;
    else if (k === 'text') node.textContent = v;
    else if (k === 'dataset') Object.assign(node.dataset, v);
    else if (k.startsWith('on')) node.addEventListener(k.slice(2).toLowerCase(), v);
    else node.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children) {
    if (c == null) continue;
    node.append(c.nodeType ? c : document.createTextNode(c));
  }
  return node;
}

// -- shared controls (mockup design system) -----------------------------------
export function checkEl(on, onchange, attrs = {}) {
  const c = el('span', { role: 'checkbox', tabindex: '0', 'aria-checked': String(!!on), ...attrs, class: 'check' + (on ? ' on' : '') + (attrs.class ? ' ' + attrs.class : '') }, on ? '✓' : '');
  c.addEventListener('click', e => {
    e.preventDefault();
    if (c.getAttribute('aria-disabled') === 'true') return;
    const next = !c.classList.contains('on');
    setCheck(c, next);
    onchange?.(next);
  });
  c.addEventListener('keydown', e => {
    if (e.key === ' ' || e.key === 'Enter') { e.preventDefault(); e.stopPropagation(); c.click(); }
  });
  return c;
}
export function setCheck(c, on) {
  c.classList.toggle('on', !!on);
  c.textContent = on ? '✓' : '';
  c.setAttribute('aria-checked', String(!!on));
}

export function stepperEl(value, onchange, { min = -Infinity, max = Infinity, wide = true, placeholder = '' } = {}) {
  const inp = el('input', { class: 'mono', value: String(value), placeholder });
  const commit = dv => {
    let n = parseInt(inp.value, 10);
    if (Number.isNaN(n)) n = 0;
    n = Math.max(min, Math.min(max, n + dv));
    inp.value = String(n);
    onchange?.(n);
  };
  const wrap = el('div', { class: 'stepper' + (wide ? ' wide' : '') },
    el('button', { type: 'button', onclick: () => commit(-1), text: '−' }),
    inp,
    el('button', { type: 'button', onclick: () => commit(1), text: '+' }));
  inp.addEventListener('change', () => {
    let n = parseInt(inp.value, 10);
    if (Number.isNaN(n)) n = 0;
    n = Math.max(min, Math.min(max, n));
    inp.value = String(n);
    onchange?.(n);
  });
  return wrap;
}

export function selectEl(options, value, onchange, attrs = {}) {
  const sel = el('select', { ...attrs, class: `select-control ${attrs.class || ''}` });
  for (const o of options) sel.append(el('option', { value: o.value, text: o.label }));
  sel.value = value;
  sel.addEventListener('change', () => onchange?.(sel.value));
  return sel;
}

export function formatCount(n) {
  n = Number(n) || 0;
  if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(n >= 1e4 ? 0 : 1) + 'k';
  return String(n);
}

// -- sidebar: site picker -------------------------------------------------------
// One compact dropdown instead of a wall of chips: grouped by engine family,
// filterable, with health and authorization state per site.
const SITE_GROUPS = [
  ['Booru', ['e621', 'danbooru', 'aibooru', 'moebooru', 'gelbooru', 'gelbooru_legacy', 'gelbooru_com', 'sankaku']],
  ['Архивы подписок', ['kemono', 'onlyhaven']],
  ['Арт-платформы', ['pixiv', 'deviantart', 'furaffinity']],
  ['Соцсети', ['twitter', 'misskey']],
];
export function siteGroups(sites) {
  const groups = SITE_GROUPS.map(([label, engines]) => ({ label, sites: sites.filter(s => engines.includes(s.engine)) }));
  const known = new Set(SITE_GROUPS.flatMap(g => g[1]));
  groups.push({ label: 'Другие', sites: sites.filter(s => !known.has(s.engine)) });
  return groups.filter(g => g.sites.length);
}
const siteHealth = new Map();   // key -> 'checking' | 'ok' | 'down' | 'error'
let siteCredentials = null;
const HEALTH_TEXT = { checking: 'Проверка доступности…', ok: 'Доступен', down: 'Не отвечает', error: 'Ошибка проверки' };
async function loadSiteCredentials(force = false) {
  if (siteCredentials && !force) return siteCredentials;
  try { siteCredentials = await window.go.main.App.GetCredentials?.() || {}; } catch { siteCredentials = {}; }
  return siteCredentials;
}
export function siteNeedsLogin(site, creds = siteCredentials) {
  if (!site?.requires_auth || !creds) return false;
  const saved = creds[site.key] || {};
  return !(site.auth_fields || []).every(f => saved[f.key]);
}
function siteBadges(site) {
  const out = [];
  if (siteNeedsLogin(site)) out.push(el('span', { class: 'site-badge warn', text: 'нужен вход', title: 'Сайт требует авторизации — данные не заданы' }));
  else if (site.requires_auth) out.push(el('span', { class: 'site-badge', text: 'вход', title: 'Требует авторизации — данные сохранены' }));
  if (site.cf_protected) out.push(el('span', { class: 'site-badge', text: 'CF', title: 'Защищён Cloudflare: при 403 задайте cf_clearance в авторизации' }));
  return out;
}
function renderSites() {
  const wrap = document.getElementById('site-picker');
  if (!wrap) return;
  const site = state.currentSite;
  const health = site ? siteHealth.get(site.key) || 'checking' : 'checking';
  let button = wrap.querySelector('.site-current');
  if (!button) {
    button = el('button', { class: 'site-current', type: 'button', 'aria-haspopup': 'listbox', 'aria-expanded': 'false', 'aria-label': 'Сайт',
      onclick: () => (document.getElementById('site-pop') ? closeSitePicker() : openSitePicker()),
      onkeydown: e => { if (e.key === 'ArrowDown') { e.preventDefault(); openSitePicker(); } } });
    wrap.replaceChildren(button, el('div', { id: 'site-health', class: 'site-health', role: 'status', 'aria-live': 'polite' }));
  }
  button.replaceChildren(
    el('span', { class: 'site-dot', dataset: { health } }),
    el('span', { class: 'site-name', text: site?.nick || '—' }),
    el('span', { class: 'site-domain', text: site?.domain || '' }),
    ...(site ? siteBadges(site) : []),
    el('span', { class: 'caret', 'aria-hidden': 'true' }));
  button.title = site?.label || '';
  const label = wrap.querySelector('#site-health');
  label.dataset.health = health;
  label.textContent = HEALTH_TEXT[health];
}
function closeSitePicker(focus = true) {
  const pop = document.getElementById('site-pop');
  if (!pop) return;
  pop.remove();
  document.removeEventListener('mousedown', outsideSitePicker, true);
  const button = document.querySelector('#site-picker .site-current');
  button?.setAttribute('aria-expanded', 'false');
  if (focus) button?.focus();
}
function outsideSitePicker(e) {
  if (!e.target.closest?.('#site-pop, #site-picker')) closeSitePicker(false);
}
async function openSitePicker() {
  const wrap = document.getElementById('site-picker');
  if (!wrap || document.getElementById('site-pop')) return;
  const button = wrap.querySelector('.site-current');
  button.setAttribute('aria-expanded', 'true');
  const search = el('input', { class: 'field site-search', type: 'search', placeholder: 'Найти сайт…', spellcheck: 'false', autocomplete: 'off', 'aria-label': 'Поиск сайта' });
  const list = el('div', { class: 'site-list', role: 'listbox', 'aria-label': 'Сайты' });
  const pop = el('div', { id: 'site-pop', class: 'site-pop' }, search, list);
  wrap.append(pop);
  document.addEventListener('mousedown', outsideSitePicker, true);
  let options = [], index = -1;
  const mark = () => options.forEach((o, i) => { o.classList.toggle('active', i === index); if (i === index) o.scrollIntoView?.({ block: 'nearest' }); });
  const render = () => {
    const q = search.value.trim().toLowerCase();
    list.replaceChildren(); options = [];
    for (const group of siteGroups(state.sites)) {
      const items = group.sites.filter(s => !q || [s.nick, s.label, s.domain, s.key].some(v => String(v || '').toLowerCase().includes(q)));
      if (!items.length) continue;
      list.append(el('div', { class: 'site-group', text: group.label }));
      for (const s of items) {
        const option = el('button', { type: 'button', role: 'option', class: 'site-chip site-option' + (s.key === state.currentSite?.key ? ' current' : ''),
          'aria-selected': String(s.key === state.currentSite?.key), title: s.label, dataset: { site: s.key },
          onclick: () => { closeSitePicker(); if (s.key !== state.currentSite?.key) selectSite(s.key); } },
          el('span', { class: 'site-dot', dataset: { health: siteHealth.get(s.key) || 'unknown' } }),
          el('span', { class: 'site-name', text: s.nick }),
          el('span', { class: 'site-domain', text: s.domain || '' }),
          ...siteBadges(s));
        list.append(option); options.push(option);
      }
    }
    if (!options.length) list.append(el('div', { class: 'site-empty muted', text: 'Ничего не найдено' }));
    index = q ? 0 : options.findIndex(o => o.dataset.site === state.currentSite?.key);
    mark();
  };
  search.addEventListener('input', render);
  search.addEventListener('keydown', e => {
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      if (!options.length) return;
      index = (index + (e.key === 'ArrowDown' ? 1 : -1) + options.length) % options.length; mark();
    } else if (e.key === 'Enter') { e.preventDefault(); options[index]?.click(); }
    else if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); closeSitePicker(); }
  });
  pop.addEventListener('keydown', e => { if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); closeSitePicker(); } });
  render();
  search.focus();
  await loadSiteCredentials();
  if (pop.isConnected) render();
}

export async function selectSite(key, { carry = false } = {}) {
	state.manualQuery=null; state.manualSynced=false;
	const previous = state.currentSite;
  state.currentSite = state.sites.find(s => s.key === key);
  if (!state.filtersState[key]) {
    try {
      state.filtersState[key] = await window.go.main.App.FilterDefaults(key) ?? {};
    } catch { state.filtersState[key] = {}; }
  }
  if (carry && previous && previous.key !== key) {
    const source = state.filtersState[previous.key] || {};
    for (const f of state.currentSite.filters || []) {
      const other = previous.filters?.find(x => x.key === f.key && x.kind === f.kind);
      if (other && ['include', 'exclude', 'artists', 'rating', 'score', 'faves', 'images_only'].includes(f.key) && source[f.key] !== undefined)
        state.filtersState[key][f.key] = structuredClone(source[f.key]);
    }
  }
  renderSites();
  renderFilters();
  window.dispatchEvent(new CustomEvent('site-changed', { detail: key }));
  checkHealth(key);
  saveSettingsSoon();
}

// -- sidebar: dynamic filter panel ---------------------------------------------
function defaultValue(f) {
  switch (f.kind) {
    case 'tags': case 'exclude': case 'authors': return [];
    case 'threshold': return { enabled: false, op: '>=', n: f.default_value };
    case 'choice': return f.options?.[0]?.value ?? '';
    case 'text': return '';
    case 'bool': return !!f.default_bool;
  }
  return '';
}

export function renderFilters({ animate = true } = {}) {
  const panel = document.getElementById('filter-panel');
  panel.innerHTML = '';
  const site = state.currentSite;
  if (!site) return;
  const vals = state.filtersState[site.key] ||= {};
  normalizeDiscoveryFilters(site, vals);
  for (const f of site.filters) {
    if (!vals.hasOwnProperty(f.key)) vals[f.key] = defaultValue(f);
    panel.append(renderFilter(f, vals));
  }
  renderQuery();
  if (animate) animateIn(panel);
}

function renderFilter(f, vals) {
  const box = el('div', { class: 'group', dataset: { filter: f.key } });
  if (f.tooltip) box.title = f.tooltip;

  switch (f.kind) {
    case 'tags': case 'exclude': case 'authors': {
      const cls = f.kind === 'exclude' ? 'exc' : f.kind === 'authors' ? 'author' : 'req';
      const ph = f.placeholder || (f.kind === 'exclude' ? 'исключить тег…'
        : f.kind === 'authors' ? 'имя автора…' : 'добавить тег…');
      box.append(
        el('div', { class: 'label', text: f.label }),
        tagWell(f, vals, cls, ph));
      break;
    }
    case 'threshold': {
      const v = vals[f.key] = { enabled: false, op: (f.op_tokens || '>=').split(' ')[0], n: f.default_value || 0, ...(normalizeFilterValue(f,vals[f.key]) || {}) };
      // display glyphs ('≥') map 1:1 onto query tokens ('>=')
      const ops = (f.ops || '≥ > =').split(' ');
      const tokens = (f.op_tokens || '>= > =').split(' ');
      // Touching the value or operator turns the condition on.
      const enable = () => { if (!v.enabled) { v.enabled = true; setCheck(chk, true); row.classList.remove('off'); } };
      const numWrap = stepperEl(v.n, n => { v.n = n; enable(); filtersEdited(); }, { min: f.minimum ?? -1000, max: f.maximum || 1000000 });
      numWrap.querySelector('input')?.setAttribute('aria-label', `${f.label}: значение`);
      const opSel = selectEl(
        ops.map((o, i) => ({ value: tokens[i] ?? o, label: o })),
        v.op, op => { v.op = op; enable(); filtersEdited(); },
        { class: 'op', 'aria-label': `${f.label}: условие` });
      const chk = checkEl(v.enabled, on => {
        v.enabled = on;
        row.classList.toggle('off', !on);
        filtersEdited();
      }, { 'aria-label': f.label });
      const row = el('div', { class: 'frow' + (v.enabled ? '' : ' off') },
        chk,
        el('span', { class: 'fname', text: f.label }),
        opSel,
        numWrap);
      box.append(row);
      break;
    }
    case 'choice': {
      box.append(el('div', { class: 'prow' },
        el('span', { class: 'pl', text: f.label }),
        el('div', { class: 'select choice' },
          selectEl((f.options || []).map(o => ({ value: o.value, label: o.label })),
            vals[f.key] ?? '', v => {
              vals[f.key] = v;
              if (changeDiscoveryFilter(state.currentSite, vals, f.key)) {
                if (state.manualQuery !== null) state.manualQuery = changeManualDiscovery(state.manualQuery, f.key, v);
                renderFilters();
                saveSettingsSoon();
              } else filtersEdited();
            }))));
      break;
    }
    case 'text': {
      box.append(
        el('div', { class: 'label', text: f.label }),
        el('input', {
          class: 'field', type: 'text', placeholder: f.placeholder || '',
          value: vals[f.key],
          onchange: e => { vals[f.key] = e.target.value; filtersEdited(); },
        }));
      break;
    }
    case 'bool': {
      const t = el('label', { class: 'toggle' },
        checkEl(vals[f.key], on => {
          vals[f.key] = on;
          const changed = changeDiscoveryFilter(state.currentSite, vals, f.key);
          const discovery = discoverySite(state.currentSite) && ['hot', 'popular'].includes(f.key);
          if (discovery && state.manualQuery !== null) state.manualQuery = changeManualDiscovery(state.manualQuery, f.key, on);
          if (changed) renderFilters(); else if (discovery) renderQuery(); else return filtersEdited();
          saveSettingsSoon();
        }, { 'aria-label': f.label }),
        el('span', { class: 'ttxt', text: f.label }));
      box.append(t);
      break;
    }
  }
  return box;
}

// -- tag wells (chips + inline input + autocomplete) -----------------------------
let acAbortHide = null;
function tagWell(f, vals, chipCls, placeholder) {
  const well = el('div', { class: 'tagwell' });
  const chips = el('span', { style: 'display:contents' });
  const input = el('input', { class: 'tag-input', type: 'text', placeholder: '' });
  const ph = el('span', { class: 'ph', text: placeholder });

  const syncPh = () =>
    ph.style.display = ((vals[f.key] || []).length || input.value) ? 'none' : '';

  const chipNodes=new Map();
  const renderChips = () => {
    const keep=new Set(vals[f.key] || []);
    for(const [value,node] of chipNodes)if(!keep.has(value)){node.remove();chipNodes.delete(value);}
    for (const v of vals[f.key] || []) {
      if(chipNodes.has(v))continue;
      const chip=el('span', { class: `tag ${chipCls}` },
        el('span', { class: 'dot' }),
        el('span', { class: 'tname', text: v }),
        el('button', {
          class: 'x', text: '×', type: 'button', 'aria-label': `Убрать ${v}`,
          onclick: () => {
            vals[f.key] = vals[f.key].filter(x => x !== v);
            renderChips(); filtersEdited();
          },
        }));
      chips.append(chip);chipNodes.set(v,chip);
    }
    syncPh();
  };

  const commit = raw => {
    const phrase = state.currentSite?.tag_phrase && f.kind !== 'authors';
    const v = phrase ? raw.trim() : raw.trim().replace(/^[@~]+/, '');
    if (!v) return;
    const cur = vals[f.key];
    if (!Array.isArray(cur)) vals[f.key] = cur ? [cur] : [];
    if (vals[f.key].includes(v)) { input.value = ''; syncPh(); return; }
    vals[f.key].push(v);
    input.value = '';
    if (changeDiscoveryFilter(state.currentSite, vals, f.key, f.kind === 'exclude' ? '-' + v : v)) {
      state.manualQuery = null;
      renderFilters(); saveSettingsSoon();
      document.querySelector(`[data-filter="${f.key}"] .tag-input`)?.focus();
      return;
    }
    renderChips(); filtersEdited();
  };
  const removeLast = () => {
    const arr = vals[f.key];
    if (Array.isArray(arr) && arr.length) {
      arr.pop();
      renderChips(); filtersEdited();
    }
  };

  well.addEventListener('click', e => {
    if (e.target === well || e.target === ph) input.focus();
  });
  input.addEventListener('focus', () => well.classList.add('focus'));
  input.addEventListener('blur', () => { well.classList.remove('focus'); setTimeout(hideAC, 120); });

  input.addEventListener('keydown', e => {
    if (e.key === 'Enter' || e.key === ',' || (e.key === ' ' && !state.currentSite?.tag_phrase)) {
      if (acPickActive()) return;   // Enter picks the highlighted suggestion
      e.preventDefault();
      commit(input.value);
      hideAC();
    } else if (e.key === 'Backspace' && !input.value) {
      removeLast();
    } else if (e.key === 'Escape') {
      hideAC();
    }
  });

  // live autocomplete (backend enforces minimum lengths)
  let acTimer = null, acScope;
  const cancelSuggestions = () => { clearTimeout(acTimer); acScope?.close(); if(!input.isConnected)window.removeEventListener('site-changed',cancelSuggestions); };
  window.addEventListener('site-changed',cancelSuggestions);
  input.addEventListener('input', () => {
    clearTimeout(acTimer); acScope?.close();
    const scope = acScope = new OperationScope(window.go.main.App,input);
    syncPh();
    const q = input.value.trim();
    if (q.length < (f.autocomplete === 'artists' ? 2 : 3)) { hideAC(); return; }
    acTimer = setTimeout(async () => {
      try {
        const siteKey = state.currentSite.key;
        const mode = f.autocomplete === 'artists'
          ? await scope.call('SearchArtists', siteKey, q)
          : await scope.call('Autocomplete', siteKey, q);
        if (scope.closed || state.currentSite.key !== siteKey || input.value.trim() !== q || !input.isConnected) return;
        showAC(input, mode || [], item => {
          commit(item.value || item.name);
          hideAC();
          input.focus();
        });
      } catch (e) {
        if (f.autocomplete === 'artists' && input.isConnected && input.value.trim() === q) setStatus(String(e), true);
      }
    }, 250);
  });

  // -- dropdown --
  let acRows = [], acIdx = -1;
  const acPickActive = () => acIdx >= 0 && acRows[acIdx];

  function hideAC() {
    document.getElementById('ac-pop')?.remove();
    acRows = []; acIdx = -1;
  }
  function markActive() {
    acRows.forEach((r, i) => r.classList.toggle('active', i === acIdx));
  }
  function showAC(anchor, items, pick) {
    hideAC();
    if (!items.length) return;
    const pop = el('div', { id: 'ac-pop', class: 'ac-pop' });
    for (const it of items.slice(0, 12)) {
      const row = el('button', { class: 'ac-row', type: 'button' },
        el('span', { class: `tag-cat-${it.category}`, text: it.name }),
        it.post_count ? el('span', { class: 'ac-count', text: formatCount(it.post_count) }) : null);
      row.addEventListener('mousedown', e => { e.preventDefault(); pick(it); });
      pop.appendChild(row);
      acRows.push(row);
    }
    const r = anchor.getBoundingClientRect();
    pop.style.left = r.left + 'px';
    pop.style.top = r.bottom + 4 + 'px';
    document.body.appendChild(pop);
  }
  // keyboard navigation for the dropdown
  input.addEventListener('keydown', e => {
    if (!acRows.length) return;
    if (e.key === 'ArrowDown') { e.preventDefault(); acIdx = (acIdx + 1) % acRows.length; markActive(); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); acIdx = (acIdx - 1 + acRows.length) % acRows.length; markActive(); }
    else if (e.key === 'Enter' && acPickActive()) {
      e.preventDefault();
      acRows[acIdx].dispatchEvent(new MouseEvent('mousedown'));
    }
  });

  well.append(chips, input, ph);
  renderChips();
  return well;
}

// -- query assembly ---------------------------------------------------------------
// The query line and the sidebar are kept in sync both ways: filter edits
// rebuild the query; typing a query is parsed back into filters whenever the
// sidebar can represent it. Only unrepresentable queries stay "manual".
let goldPolicySignature = '';
let textFilterSignature = '';
state.manualSynced = false;
state.manualUnmatched = [];
export function filtersEdited() {
  if (state.manualQuery !== null) {
    if (!state.manualSynced) setStatus('Ручной запрос заменён фильтрами');
    state.manualQuery = null; state.manualSynced = false; state.manualUnmatched = [];
  }
  renderQuery(); saveSettingsSoon();
}
export function renderQuery() {
  const site = state.currentSite;
  if (!site) return;
  if (discoverySite(site) && state.manualQuery !== null) {
    state.manualQuery = normalizeDiscoveryQuery(state.manualQuery);
    syncDiscoveryOrder(state.filtersState[site.key] ||= {}, state.manualQuery);
  }
  const gold = state.filtersState.danbooru || {};
  const textSignature = JSON.stringify(Object.entries(state.filtersState).map(([site, values]) => [site, !!values.hide_text]).sort());
  if (textSignature !== textFilterSignature) {
    textFilterSignature = textSignature;
    window.dispatchEvent(new Event('text-filters-changed'));
  }
  const signature = JSON.stringify([!!gold.use_gold, gold.gold_tags || []]);
  if (signature !== goldPolicySignature) {
    goldPolicySignature = signature;
    window.dispatchEvent(new Event('media-policy-changed'));
  }
  state.query = state.manualQuery ?? buildQuery(site, state.filtersState[site.key] || {});
  if (discoverySite(site)) syncDiscoveryControls();
  for (const f of site.filters || []) if (f.enabled_if) {
    const box = document.querySelector(`[data-filter="${f.key}"]`);
    if (box) {
      const on = !!state.filtersState[site.key][f.enabled_if];
      box.style.opacity = on ? '1' : '.4'; box.style.pointerEvents = on ? '' : 'none';
      box.toggleAttribute('aria-disabled', !on);
      const dep = site.filters.find(x => x.key === f.enabled_if);
      if (dep) box.title = on ? (f.tooltip || '') : `Доступно при включённом «${dep.label}»`;
    }
  }
  renderManualState();
  const box = document.getElementById('query-line');
  if (!box) return;
  // Never rewrite the line under the user's caret while they type.
  if (state.manualQuery !== null && document.activeElement === box) return;
  if (state.query) {
    box.classList.remove('q-empty');
    box.value = state.query;
  } else {
    box.classList.add('q-empty');
    box.value = '';
  }
}

function renderManualState() {
  const manual = state.manualQuery !== null && !state.manualSynced;
  const banner = document.getElementById('manual-banner');
  if (banner) {
    banner.hidden = !manual;
    const detail = banner.querySelector('.mb-detail');
    if (detail) detail.textContent = state.manualUnmatched?.length
      ? `Не переносится в фильтры: ${state.manualUnmatched.slice(0, 6).join(' ')}${state.manualUnmatched.length > 6 ? ' …' : ''}`
      : '';
  }
  document.getElementById('filter-panel')?.classList.toggle('manual', manual);
  const badge = document.getElementById('q-mode');
  if (badge) badge.hidden = !manual;
  const reset = document.getElementById('query-reset');
  if (reset) reset.hidden = !manual;
  document.querySelector('.q-box')?.classList.toggle('manual', manual);
}

function syncDiscoveryControls() {
  const vals = state.filtersState[state.currentSite?.key] || {};
  for (const key of ['hot', 'popular']) {
    const check = document.querySelector(`[data-filter="${key}"] .check`);
    if (check) setCheck(check, !!vals[key]);
  }
  const sort = document.querySelector('[data-filter="sort"] select');
  if (sort) {
    sort.querySelector('option[data-discovery]')?.remove();
    const order = (state.manualQuery ?? buildQuery(state.currentSite, vals)).split(/\s+/).find(t => /^-?order:/i.test(t));
    const label = vals.popular ? 'Популярное' : vals.hot ? 'Горячее'
      : order && !/^order:id(?:_asc|_desc)?$/i.test(order) ? order : '';
    if (label) {
      sort.prepend(el('option', { value: '__discovery', text: label, disabled: true, 'data-discovery': true }));
      sort.value = '__discovery';
    } else sort.value = vals.sort || '';
  }
}

// Parse the manual line into sidebar values. On success the filters update
// in place while the typed text stays untouched until the line loses focus.
export function syncManualToFilters() {
  const site = state.currentSite;
  if (!site || state.manualQuery === null) return false;
  const parsed = parseQuery(site, state.manualQuery, state.filtersState[site.key] || {});
  state.manualUnmatched = parsed.unmatched;
  if (parsed.values) {
    state.filtersState[site.key] = parsed.values;
    state.manualSynced = true;
    const focused = document.activeElement;
    renderFilters({ animate: false });
    if (focused?.isConnected && focused !== document.activeElement) focused.focus?.();
  } else {
    state.manualSynced = false;
    renderManualState();
  }
  saveSettingsSoon();
  return state.manualSynced;
}

let manualParseTimer;
export function setManualQuery(query) {
  const box = document.getElementById('query-line');
  const caret = box?.selectionStart;
  state.manualQuery = discoverySite(state.currentSite) ? normalizeDiscoveryQuery(query) : query.trim();
  state.manualSynced = false;
  renderQuery();
  // Preserve the separator being typed; trimming the input prevents a next tag.
  if (box) {
    box.value = discoverySite(state.currentSite) ? state.manualQuery + (/\s$/.test(query) ? ' ' : '') : query;
    if (caret != null) box.setSelectionRange?.(Math.min(caret, box.value.length), Math.min(caret, box.value.length));
  }
  clearTimeout(manualParseTimer);
  manualParseTimer = setTimeout(syncManualToFilters, 300);
  saveSettingsSoon();
}
// Leaving the line: a fully parsed query is handed back to the filters.
function commitManualQuery() {
  clearTimeout(manualParseTimer);
  if (state.manualQuery === null) return;
  syncManualToFilters();
  if (state.manualSynced) { state.manualQuery = null; state.manualSynced = false; renderQuery(); }
}
export function resetManualQuery() {
  clearTimeout(manualParseTimer);
  state.manualQuery = null; state.manualSynced = false; state.manualUnmatched = [];
  renderQuery(); saveSettingsSoon();
}

// -- settings persistence -----------------------------------------------------------
let saveTimer = null;
export async function flushFilterSettings() {
	if (window.isFeedWindow) { state.filtersState = await window.go.main.App.GetSetting('filters_state') || {}; return; }
  clearTimeout(saveTimer);
  await window.go.main.App.SetSetting('filters_state', state.filtersState);
}
function saveSettingsSoon() {
  clearTimeout(saveTimer);
  saveTimer = setTimeout(async () => {
    try {
      await flushFilterSettings();
      await window.go.main.App.SetSetting('site', state.currentSite?.key ?? 'e621');
    } catch { /* ignore */ }
  }, 600);
}

// -- shell ------------------------------------------------------------------------------
function shellHTML() {
  return `
    <div class="window">
      <div id="titlebar-slot"></div>
      <div class="body">
        <aside class="sidebar">
          <div class="side-scroll">
            <div class="group">
              <div class="label">Сайт</div>
              <div id="site-picker" class="site-picker"></div>
            </div>
            <div id="manual-banner" class="manual-banner" role="status" hidden>
              <div class="mb-title">Запрос изменён вручную</div>
              <div class="mb-text">Фильтры ниже сейчас не применяются. Любое изменение фильтра заменит ручной запрос.</div>
              <div class="mb-detail mono"></div>
              <button type="button" class="linkbtn" id="manual-reset">Вернуть запрос из фильтров</button>
            </div>
            <div id="filter-panel" class="filter-panel"></div>
          </div>
          <div id="side-foot" class="side-foot">
            <details id="run-opts" class="run-opts">
              <summary>${iconHTML('sliders', 14)}<span class="ro-title">Параметры загрузки</span><span id="run-summary" class="ro-summary"></span></summary>
              <div class="ro-body">
                <div id="params-panel" class="group"></div>
                <div class="group">
                  <div class="label">Папка загрузки</div>
                  <div class="dirrow">
                    <input id="dl-root" class="field" type="text" spellcheck="false" aria-label="Папка загрузки" />
                    <button id="dl-root-pick" class="iconbtn" title="Выбрать папку" aria-label="Выбрать папку">…</button>
                    <button id="dl-root-open" class="iconbtn" title="Открыть в проводнике" aria-label="Открыть в проводнике">${iconHTML('external', 15)}</button>
                  </div>
                </div>
              </div>
            </details>
            <div id="run-buttons"></div>
          </div>
        </aside>
        <main class="main">
          <div class="topbar">
            <div class="q-row">
              <span class="q-label">Запрос</span>
              <div class="q-box">
                <input id="query-line" class="q-text mono" placeholder="пусто — все посты" spellcheck="false" aria-label="Запрос" title="Ctrl+L — перейти к запросу">
                <span id="q-mode" class="q-mode" hidden title="Запрос не совпадает с фильтрами слева">вручную</span>
                <button type="button" class="q-reset" id="query-reset" hidden title="Пересобрать запрос из фильтров">${iconHTML('reset', 13)}<span>из фильтров</span></button>
              </div>
              <div id="presets-row" class="presets"></div>
              <span id="top-actions" class="top-actions"></span>
            </div>
            <div id="run-strip" class="run-strip idle">
              <div class="scoreboard">
                <div class="score-primary">
                  <div class="ring">${iconHTML('download', 17)}</div>
                  <div>
                    <div class="big"><span id="score-done">0</span><span class="sep"> / </span><span id="score-total">0</span></div>
                    <div class="cap">скачано из очереди</div>
                  </div>
                </div>
                <div class="stats">
                  <div class="stat zero" id="statw-scanned"><span class="sv" id="stat-scanned">0</span><span class="sl">просканир.</span></div>
                  <div class="stat queued zero" id="statw-pending"><span class="sv" id="stat-pending">0</span><span class="sl">в очереди</span></div>
                  <div class="stat zero" id="statw-dupes"><span class="sv" id="stat-dupes">0</span><span class="sl">дубли</span></div>
                  <div class="stat done zero" id="statw-skipped"><span class="sv" id="stat-skipped">0</span><span class="sl">пропущено</span></div>
                  <div class="stat err zero" id="statw-failed"><span class="sv" id="stat-failed">0</span><span class="sl">ошибки</span></div>
                </div>
                <button id="btn-stop" class="stop-btn" type="button" disabled title="Остановить сканирование и загрузку">${iconHTML('stop', 14)}<span>Стоп</span></button>
              </div>
              <div class="progress"><i id="progress-bar"></i></div>
            </div>
          </div>
          <div id="tabs-bar" class="tabs"></div>
          <div class="preview">
            <div id="controls-bar" class="pv-bar"></div>
            <div class="grid-wrap"><div id="content"></div></div>
          </div>
          <div class="statusbar">
            <span class="sdot" id="status-dot"></span>
            <span class="st-text" id="status-line" role="status" aria-live="polite">Готов к работе</span>
            <span class="st-spacer"></span>
            <span class="st-speed" id="status-speed"></span>
            <span class="st-hist" title="Записей в истории скачиваний. Сброс — в Настройки → Кэш">история: <b id="hist-count">0</b></span>
          </div>
        </main>
      </div>
    </div>`;
}

function wireShell() {
  const query = document.getElementById('query-line');
  query.addEventListener('input', () => { setManualQuery(query.value); });
  query.addEventListener('keydown', e => {
    if (e.key === 'Enter') { commitManualQuery(); document.getElementById('btn-search')?.click(); }
    else if (e.key === 'Escape' && state.manualQuery !== null) { e.preventDefault(); resetManualQuery(); }
  });
  query.addEventListener('blur', commitManualQuery);
  document.getElementById('query-reset').addEventListener('click', resetManualQuery);
  document.getElementById('manual-reset').addEventListener('click', resetManualQuery);
  document.getElementById('titlebar-slot').replaceWith(titlebar());

  const rootInput = document.getElementById('dl-root');
  rootInput?.addEventListener('change', async () => {
    const v = rootInput.value.trim();
    await window.go.main.App.SetSetting('download_root', v).catch(() => {});
    window.dispatchEvent(new Event('download-root-changed'));
    setStatus('папка загрузки сохранена');
  });
  document.getElementById('dl-root-pick')?.addEventListener('click', async () => {
    try {
      const dir = await window.go.main.App.PickDirectory();
      if (dir) {
        rootInput.value = dir;
        await window.go.main.App.SetSetting('download_root', dir);
        window.dispatchEvent(new Event('download-root-changed'));
      }
    } catch (e) { setStatus(String(e), true); }
  });
  document.getElementById('dl-root-open')?.addEventListener('click',
    () => window.go.main.App.RevealDownloadRoot().catch(() => {}));
  const opts = document.getElementById('run-opts');
  try { opts.open = localStorage.getItem('run-opts-open') === '1'; } catch {}
  opts.addEventListener('toggle', () => { try { localStorage.setItem('run-opts-open', opts.open ? '1' : '0'); } catch {} });
  document.addEventListener('keydown', globalKeys);
}

const typing = target => target?.closest?.('input, textarea, select, [contenteditable="true"]');
function globalKeys(e) {
  if (e.defaultPrevented || document.querySelector('.viewer-overlay')) return;   // dialogs own the keyboard
  const key = e.key.toLowerCase();
  if ((e.ctrlKey || e.metaKey) && (key === 'l' || key === 'k')) {
    e.preventDefault(); const q = document.getElementById('query-line'); q?.focus(); q?.select?.(); return;
  }
  if ((e.ctrlKey || e.metaKey) && key === 'enter') { e.preventDefault(); document.getElementById('btn-dl-all')?.click(); return; }
  if (e.key === 'F5') { e.preventDefault(); document.getElementById('btn-search')?.click(); return; }
  if (typing(e.target)) return;
  window.dispatchEvent(new CustomEvent('grid-key', { detail: e }));
}

export async function refreshHistCount() {
  try {
    const n = await window.go.main.App.HistoryCount();
    const node = document.getElementById('hist-count');
    if (node) node.textContent = Number(n).toLocaleString('ru-RU');
  } catch { /* ignore */ }
}

export function setStatus(text, isError = false) {
  const line = document.getElementById('status-line');
  const dot = document.getElementById('status-dot');
  if (line) line.textContent = text;
  if (dot) dot.classList.toggle('err', !!isError);
}
export { setStatus as showStatus };

// -- boot -------------------------------------------------------------------------------
import { initMainArea, refreshScoreboard, renderGridOrEmpty } from './grid.js';
import { renderRunPanel, renderParams, renderTabs, switchTab, bindGlobalEvents } from './panels.js';
import { renderPresets, renderTopActions } from './dialogs.js';

async function init() {
  const app = document.getElementById('app');
  if (window.go?.main?.FeedWindow) {
    const bridge = window.go.main.FeedWindow;
    const mode = await bridge.Mode?.();
    window.isLibraryWindow = mode === 'library';
    window.isFeedWindow = !window.isLibraryWindow;
    window.go.main.App = new Proxy({}, { get: (_, method) => (...args) => bridge.Call(method, args) });
    try {
      state.settings = await window.go.main.App.GetAllSettings();
      initMotion();
      state.sites = await window.go.main.App.Sites() || [];
      state.filtersState = initialFilters(state.sites, state.settings.filters_state, state.settings);
      if (window.isLibraryWindow) {
        await import('./library.css');
        const { mountLibrary } = await import('./library.js');
        await mountLibrary(app);
        return;
      }
      await import('./feed.css');
      const { mountFeed } = await import('./feed.js');
      await mountFeed(app);
    } catch (e) { app.textContent = `Ошибка открытия ${window.isLibraryWindow ? 'библиотеки' : 'ленты'}: ${e}`; }
    return;
  }
  app.innerHTML = shellHTML();
  wireShell();

  try {
    state.settings = await window.go.main.App.GetAllSettings();
    initMotion();
    state.sites = await window.go.main.App.Sites() ?? [];
    const savedSite = await window.go.main.App.GetSetting('site');
    state.filtersState = initialFilters(state.sites, state.settings.filters_state, state.settings);
    const initial = state.sites.find(s => s.key === savedSite) ?? state.sites[0];

    const root = await window.go.main.App.GetSetting('download_root');
    if (typeof root === 'string' && root) {
      document.getElementById('dl-root').value = root;
    }

    renderTabs(document.getElementById('tabs-bar'));
    renderRunPanel(document.getElementById('run-buttons'));
    renderParams(document.getElementById('params-panel'));
    renderTopActions(document.getElementById('top-actions'));
    renderPresets(document.getElementById('presets-row'));

    if (initial) await selectSite(initial.key);
    initMainArea();
    bindGlobalEvents();
    window.runtime?.EventsOn('credentials:changed', () => { siteCredentials = null; loadSiteCredentials(true).then(renderSites); });
    loadSiteCredentials().then(renderSites);
    window.runtime?.EventsOn('proxy:changed', () => {
      if (state.currentSite) checkHealth(state.currentSite.key);
    });
    refreshScoreboard();
    refreshHistCount();
    switchTab('preview');
  } catch (e) {
    document.getElementById('site-picker')?.append(
      el('div', { class: 'muted', text: 'bindings unavailable: ' + e }));
  }
}

function initMotion() {
  applyMotion(state.settings?.ui_motion);
  applyViewPrefs(state.settings);
  window.runtime?.EventsOn('ui:motion', value => {
    state.settings.ui_motion = value;
    applyMotion(value);
  });
  window.matchMedia?.('(prefers-reduced-motion: reduce)').addEventListener?.('change', () => applyMotion(state.settings?.ui_motion));
}

let healthVersion = 0, healthScope;
async function checkHealth(key) {
  healthScope?.close(); healthScope = new OperationScope(window.go.main.App);
  const version = ++healthVersion;
  siteHealth.set(key, 'checking'); renderSites();
  let result;
  try { result = await healthScope.call('Health', key) ? 'ok' : 'down'; }
  catch { result = 'error'; }
  if (version !== healthVersion) return;
  siteHealth.set(key, result);
  if (state.currentSite?.key === key) renderSites();
}
window.addEventListener('DOMContentLoaded', init);
