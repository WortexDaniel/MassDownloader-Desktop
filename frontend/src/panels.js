import { state, el, setStatus, checkEl, stepperEl, selectEl, refreshHistCount, flushFilterSettings } from './main.js';
import { refreshScoreboard, postCard, openViewer, openAuthor, renderGridOrEmpty, renderControls, updateDownloadStates } from './grid.js';
import { postId } from './contracts.js';
import { renderLogs, bindLogEvents } from './logs.js';
import { ensureAuth, confirmDownload } from './dialogs.js';
import { renderDownloadJobs, updateDownloadJobs, disposeDownloadJobs, downloadFailureCount, downloadJobCount } from './download-jobs.js';
import { runAuthorChecks } from './author-checks.js';
import { OperationScope } from './operations.js';
import { animateIn, forgetReveal } from './motion.js';
import { icon } from './icons.js';

const api = () => window.go.main.App;
export const run = { cfg: { max_posts: 200, skip_seen: true, dedup_mode: 'db', by_artist: true } };
export function renderRunPanel(mount) {
  const s = state.settings || {}; Object.assign(run.cfg, { max_posts: s.max_posts ?? 200, skip_seen: s.skip_seen !== false, dedup_mode: s.dedup_mode || 'db', by_artist: s.subfolder_by_artist !== false });
  const stop = async () => { await api().Cancel(); setStatus('Остановка…'); };
  const all = el('button', { id: 'btn-dl-all', class: 'btn btn-primary', type: 'button', onclick: startRun,
    title: 'Просканировать все посты по текущему запросу и скачать (Ctrl+Enter)' }, icon('download', 16), el('span', { text: 'Скачать всё по запросу' }));
  const buttons = [all];
  // The stop control normally lives next to the progress strip; keep a local
  // one only when the shell has none (feed/tests mount the panel alone).
  const shellStop = document.getElementById('btn-stop');
  if (shellStop) shellStop.onclick = stop;
  else buttons.push(el('button', { id: 'btn-stop', class: 'btn btn-danger', text: 'Остановить', disabled: true, onclick: stop }));
  mount.replaceChildren(el('div', { class: 'btns' }, ...buttons));
  updateRunSummary();
}
export function updateRunSummary() {
  const node = document.getElementById('run-summary');
  if (!node) return;
  const parts = [run.cfg.max_posts ? `до ${run.cfg.max_posts} постов` : 'все посты'];
  if (run.cfg.skip_seen) parts.push('без повторов');
  if (run.cfg.by_artist) parts.push('по авторам');
  node.textContent = parts.join(' · ');
  const root = document.getElementById('dl-root')?.value;
  node.title = `${parts.join(', ')}${root ? `\nПапка: ${root}` : ''}`;
}
window.addEventListener('download-root-changed', () => updateRunSummary());
async function saveCfg() {
  try { for (const [key, value] of Object.entries({ max_posts: run.cfg.max_posts, skip_seen: run.cfg.skip_seen, dedup_mode: run.cfg.dedup_mode, subfolder_by_artist: run.cfg.by_artist })) {state.settings[key]=value;await api().SetSetting(key, value);} window.dispatchEvent(new Event('download-settings-changed')); updateRunSummary(); }
  catch (e) { setStatus(String(e), true); }
}
async function startRun() {
  if (!state.currentSite) return;
  if(!await ensureAuth(state.currentSite.key))return;
  try { await flushFilterSettings(); await api().StartRun({ ...run.cfg, site_key: state.currentSite.key, query: state.query, root: await api().GetSetting('download_root'), artist_tags: state.filtersState[state.currentSite.key]?.artists || [] }); switchTab('downloads'); refreshScoreboard(); }
  catch (e) { setStatus(String(e), true); }
}
export function renderParams(mount) {
  const row = (label, control) => el('div', { class: 'prow' }, el('span', { class: 'pl', text: label }), control);
  mount.replaceChildren(
    row('Сверять дубли', selectEl([{ value: 'db', label: 'с базой данных' }, { value: 'folder', label: 'с папкой загрузки' }, { value: 'both', label: 'база + папка' }], run.cfg.dedup_mode, v => { run.cfg.dedup_mode = v; saveCfg(); })),
    el('label', { class: 'toggle' }, checkEl(run.cfg.skip_seen, on => { run.cfg.skip_seen = on; saveCfg(); }), 'Пропускать скачанное'),
    el('label', { class: 'toggle' }, checkEl(run.cfg.by_artist, on => { run.cfg.by_artist = on; saveCfg(); }), 'Раскладывать по папкам авторов'),
    row('Лимит постов (0 — все)', stepperEl(run.cfg.max_posts, n => { run.cfg.max_posts = n; saveCfg(); }, { min: 0 })));
}
const tabs = [['preview', 'Предпросмотр'], ['favorites', 'Избранное'], ['downloads', 'Загрузки'], ['logs', 'Логи']];
let active = 'preview', generation = 0; const counts = {};
function positionTabIndicator(mount) {
  const button = mount.querySelector('.tab.active'), indicator = mount.querySelector('.tab-indicator');
  if (!button || !indicator) return;
  indicator.style.width = `${button.offsetWidth || 0}px`;
  indicator.style.transform = `translateX(${button.offsetLeft || 0}px)`;
}
export function renderTabs(mount) {
  if (!mount) return;
  if (!mount.querySelector('[data-tab-key]')) {
    mount.replaceChildren(...tabs.map(([key,label]) => el('button', {class:'tab',dataset:{tabKey:key},onclick:()=>switchTab(key)},
      el('span',{class:'tab-label',text:label}),el('span',{class:'tnum',hidden:true}))),
      el('button', {class:'tab feed-open',title:'Открыть ленту в отдельном окне',onclick:async()=>{try{await api().OpenFeed();}catch(e){setStatus(String(e),true);}}},el('span',{class:'tab-label',text:'Лента'}),icon('external',12)),
      el('span',{class:'tab-indicator','aria-hidden':'true'}));
    if (window.ResizeObserver) new window.ResizeObserver(()=>positionTabIndicator(mount)).observe(mount);
  }
  for (const [key] of tabs) {
    const button=mount.querySelector(`[data-tab-key="${key}"]`), counter=button.querySelector('.tnum');
    button.classList.toggle('active',active===key);
    button.setAttribute('aria-current',active===key?'page':'false');
    const value=String(counts[key] || '');
    if(counter.textContent!==value)counter.textContent=value;
    counter.hidden=!counts[key];
  }
  positionTabIndicator(mount);
}
export function setTabCount(key, n) { counts[key] = n; renderTabs(document.getElementById('tabs-bar')); }
window.setJobTabCount = n => setTabCount('downloads', n);
export function switchTab(key) {
  active = key; generation++; renderTabs(document.getElementById('tabs-bar'));
  const bar = document.getElementById('controls-bar');
  if (bar) {
    bar.style.display = ['preview', 'downloads'].includes(key) ? '' : 'none';
    if (key === 'preview') renderControls();
    if (key === 'downloads') renderDownloadControls([], true);
  }
  const mount = document.getElementById('content'); mount.dataset.tab = key; mount.removeAttribute('aria-busy');
  if(key!=='downloads')disposeDownloadJobs(mount);
  for (const card of mount.querySelectorAll('.card')) forgetReveal(card);
  mount.classList.toggle('favorites-view',key==='favorites');
  if (key === 'preview') renderGridOrEmpty();
  if (key === 'favorites') renderFavorites(mount);
  if (key === 'downloads') renderDownloads(mount);
  if (key === 'logs') renderLogs(mount);
  animateIn(mount, {duration:160});
}
let authorCheckRun = null;
let authorCheckScope;
window.addEventListener('beforeunload',()=>authorCheckScope?.close());
// Hosts that answered 429 in this run: their remaining authors are skipped
// (the Go client pauses the whole zone) instead of each retrying into it.
const rateLimited = error => /HTTP 429/.test(String(error));
// Batch phase: one OR request settles many unchanged authors of one site
// (e621-family). Everyone else, and any doubt, goes to the per-author scan.
async function settleAuthorBatches(scope, authors, hostFor, blocked) {
  const app = api();
  if (typeof app.CheckAuthorsBatch !== 'function' && typeof app.RunOperation !== 'function') return { rest: authors, settled: 0 };
  const bySite = new Map();
  for (const author of authors) (bySite.get(author.site) || bySite.set(author.site, []).get(author.site)).push(author);
  const rest = []; let settled = 0;
  for (const [site, group] of bySite) {
    if (group.length < 2) { rest.push(...group); continue; }
    try {
      setStatus(`Проверка подписок: пакетная проверка ${site} (${group.length})`);
      const result = await scope.call('CheckAuthorsBatch', site, group.map(a => a.query));
      const pending = new Set(result?.pending || group.map(a => a.query));
      for (const author of group) if (pending.has(author.query) || !(author.query in (result?.unread || {}))) rest.push(author); else settled++;
    } catch (error) {
      if (rateLimited(error)) blocked.set(hostFor(group[0]), String(error));
      rest.push(...group);
    }
  }
  return { rest, settled };
}
export function checkAuthors() {
  if (authorCheckRun) return authorCheckRun;
  authorCheckRun = (async () => {
	const scope = authorCheckScope = new OperationScope(api());
    const authors = await api().Authors() || [];
    const hostFor = author => state.sites.find(site => site.key === author.site)?.domain || author.site;
    const blocked = new Map();
    const { rest, settled } = await settleAuthorBatches(scope, authors, hostFor, blocked);
    let skipped = 0;
    const result = await runAuthorChecks(rest,
      author => {
        if (blocked.has(hostFor(author))) { skipped++; return 0; }
        return scope.call('CheckNewPosts',author.site, author.query, 2000);
      }, {
        hostFor,
        onProgress: ({ completed, total, active }) => setStatus(`Проверка подписок: ${completed + settled}/${total + settled} · одновременно ${active}`),
        onError: (author, error) => {
          if (rateLimited(error)) {
            const host = hostFor(author);
            if (!blocked.has(host)) { blocked.set(host, String(error)); setStatus(`${host}: ${error}`, true); }
            return;
          }
          setStatus(`${author.name}: ${error}`, true);
        },
      });
    const done = result.completed + settled, total = result.total + settled;
    const notes = [];
    if (result.failed) notes.push(`ошибок: ${result.failed}`);
    if (skipped) notes.push(`отложено из-за лимита сайта: ${skipped} (${[...blocked.keys()].join(', ')})`);
    setStatus(`Проверено подписок: ${done}/${total}${notes.length ? ` · ${notes.join(' · ')}` : ''}`, result.failed > 0 || skipped > 0);
    return { completed: done, total, failed: result.failed };
  })().finally(() => {
	authorCheckScope?.close(); authorCheckScope=null;
    authorCheckRun = null;
    window.dispatchEvent(new Event('favorites-changed'));
  });
  return authorCheckRun;
}
let favoritesRequest = 0;
let favoritesAuthorQuery = '';
async function renderFavorites(mount) {
  const version = generation, request = ++favoritesRequest;
  const preservePosition = !!mount.querySelector('.favorites-actions');
  const current = () => generation === version && active === 'favorites' && request === favoritesRequest;
  // Refreshing unread counts must not collapse the scroll container while
  // an author dialog is open. Keep the existing view until its replacement is ready.
  if (!preservePosition) mount.textContent = 'Загрузка…';
  mount.setAttribute('aria-busy', 'true');
  try {
    const [posts, authors] = await Promise.all([api().FavPosts(), api().Authors()]);
    if (!current()) return;
    await updateDownloadStates(posts || []); if (!current()) return;
    const content = document.createDocumentFragment();
    const check = el('button', { class: 'pv-btn ghost', text: 'Проверить подписки', disabled: !!authorCheckRun, onclick: async () => { check.disabled = true; try { await checkAuthors(); } catch (e) { setStatus(String(e), true); } finally { check.disabled = false; } } });
    const startup = el('label', { class: 'toggle' }, checkEl(state.settings?.check_authors_on_startup, async on => { state.settings.check_authors_on_startup = on; await api().SetSetting('check_authors_on_startup', on); }), 'Проверять при запуске');
    content.append(el('div', { class: 'viewer-actions favorites-actions' }, check, startup), el('h3', { class: 'content-title', text: 'Авторы' }));
    const search = el('input', { class: 'field fav-author-search', type: 'search', value: favoritesAuthorQuery,
      placeholder: 'Поиск по имени автора…', 'aria-label': 'Поиск по избранным авторам', autocomplete: 'off', spellcheck: 'false' });
    const reset = el('button', { class: 'pv-btn ghost', type: 'button', text: 'Сбросить', onclick: () => {
      search.value = ''; applyAuthorSearch(); search.focus();
    } });
    const authorCount = el('span', { class: 'muted fav-author-count', role: 'status', 'aria-live': 'polite' });
    content.append(el('div', { class: 'fav-author-search-tools' }, search, reset), authorCount);
    const authorList=el('div',{class:'fav-authors'});content.append(authorList);
    const authorRows = (authors || []).map(author => ({ name: String(author.name || '').toLocaleLowerCase(), row: el('div', { class: 'fav-author' },
      el('button', { class: 'linkbtn', text: author.name, onclick: () => openAuthor(author.site, author.query, author.name) }),
      el('span', { class: 'muted', text: author.site }), author.unread > 0 ? el('span', { class: 'unread-badge', text: `+${author.unread}${author.overflow ? '+' : ''}` }) : null,
      el('button', { class: 'fa-read', text: '✓', title: 'Отметить все новые просмотренными', 'aria-label': `Отметить все новые посты ${author.name} просмотренными`, disabled: !(author.unread > 0), onclick: async e => {
        const button = e.currentTarget;
        if (button.disabled) return;
        button.disabled = true;
        try { await api().MarkAllAuthorViewed(author.site, author.query); window.dispatchEvent(new Event('favorites-changed')); }
        catch (error) { button.disabled = false; setStatus(String(error), true); }
      } }),
      el('button', { class: 'fa-del', text: '✕', title: 'Удалить подписку', onclick: async () => { await api().AuthorRemove(author.site, author.query); renderFavorites(mount); } })) }));
    function applyAuthorSearch() {
      favoritesAuthorQuery = search.value;
      const query = favoritesAuthorQuery.trim().toLocaleLowerCase();
      const matches = authorRows.filter(author => author.name.includes(query));
      authorList.replaceChildren(...matches.map(author => author.row));
      if (!matches.length) authorList.append(el('p', { class: 'muted fav-author-empty', text: authorRows.length ? 'Авторы не найдены' : 'Нет избранных авторов' }));
      authorCount.textContent = `Показано ${matches.length} из ${authorRows.length}`;
      reset.disabled = !favoritesAuthorQuery;
    }
    search.addEventListener('input', applyAuthorSearch);
    applyAuthorSearch();
    content.append(el('h3', { class: 'content-title', text: `Посты — ${posts?.length || 0}` })); const g = el('div', { class: 'grid' }); content.append(g);
    for (const p of posts || []) { const card = postCard(p, selected => openViewer(selected, posts), {animate:!preservePosition}); const remove = el('button', { class: 'fav-remove', text: '✕', title: 'Убрать из избранного', onclick: async e => { e.stopPropagation(); await api().FavRemovePost(p.site, postId(p)); renderFavorites(mount); } }); card.append(remove); g.append(card); }
    // Capture at commit time: the user may scroll while the API is pending.
    const scrollTop = preservePosition ? mount.scrollTop : 0;
    const oldSearch = mount.querySelector('.fav-author-search');
    const restoreSearchFocus = oldSearch && document.activeElement === oldSearch;
    const caret = restoreSearchFocus ? [oldSearch.selectionStart, oldSearch.selectionEnd] : null;
    for (const card of mount.querySelectorAll('.card')) forgetReveal(card);
    mount.replaceChildren(content);
    if (restoreSearchFocus) { search.focus({ preventScroll: true }); if (caret?.[0] != null) search.setSelectionRange?.(...caret); }
    mount.scrollTop = scrollTop;
    if (!preservePosition) animateIn(mount, {duration:160});
    setTabCount('favorites', posts?.length || 0);
  } catch (e) { if (current()) { if (!preservePosition) mount.textContent = String(e); setStatus(String(e), true); } }
  finally { if (current()) mount.setAttribute('aria-busy', 'false'); }
}
let retrying = false, clearing = false;
function renderDownloadControls(jobs, running, count = Array.isArray(jobs) ? jobs.length : 0) {
  const bar = document.getElementById('controls-bar'); if (!bar) return;
  const failed = Array.isArray(jobs) ? jobs.filter(job => job.status === 'failed').length : jobs || 0;
  let retry = bar.querySelector('#btn-retry-failed');
  if (!retry) {
    retry = el('button', { id: 'btn-retry-failed', class: 'pv-btn ghost', text: retrying ? 'Повторная загрузка…' : 'Повторить ошибки',
    disabled: retrying || running || !failed, 'aria-busy': String(retrying), onclick: async () => {
      if (retry.disabled || retrying || clearing) return;
      retrying = true; retry.disabled = true; retry.textContent = 'Повторная загрузка…'; retry.setAttribute('aria-busy', 'true');
      bar.querySelector('#btn-clear-downloads').disabled=true;
      setStatus('Повторная загрузка файлов с ошибками…');
      try {
        const n = await api().RetryFailed();
        setStatus(n ? `Повторная загрузка: ${n} файлов` : 'Нет файлов с ошибками для повторной загрузки');
        await refreshScoreboard();
      } catch (e) { setStatus(String(e), true); }
      finally {
        retrying = false;
        if (active === 'downloads') await renderDownloads(document.getElementById('content'));
      }
    } });
    const clear=el('button',{id:'btn-clear-downloads',class:'pv-btn ghost',text:'Очистить список',
      title:'Очистить список загрузок. Скачанные файлы и история сохранятся.',onclick:async()=> {
        if(clear.disabled || clearing || retrying)return;
        clearing=true;clear.disabled=true;retry.disabled=true;clear.textContent='Очистка…';clear.setAttribute('aria-busy','true');
        // Invalidate snapshots already in flight before clearing the backend.
        downloadRequest++;
        try {
          const removed=await api().ClearJobs();
          downloadRequest++;
          if(active==='downloads') {
            const mount=document.getElementById('content');
            disposeDownloadJobs(mount);renderDownloadJobs(mount,[],api);setTabCount('downloads',0);
          }
          setStatus(`Список загрузок очищен: ${removed}`);
          await refreshScoreboard();
        } catch(error) {setStatus(String(error),true);}
        finally {
          clearing=false;
          if(active==='downloads')await renderDownloads(document.getElementById('content'));
        }
      }});
    bar.replaceChildren(retry, el('span', { class: 'muted download-error-count' }),el('span',{class:'pv-spacer'}),clear);
  }
  retry.disabled=retrying || clearing || running || !failed;
  retry.setAttribute('aria-busy',String(retrying));
  retry.textContent=retrying?'Повторная загрузка…':'Повторить ошибки';
  bar.querySelector('.download-error-count').textContent=`Ошибок: ${failed}`;
  const clear=bar.querySelector('#btn-clear-downloads');
  clear.disabled=clearing || retrying || running || !count;
  clear.textContent=clearing?'Очистка…':'Очистить список';clear.setAttribute('aria-busy',String(clearing));
}
let downloadRequest = 0;
const downloadReaders = new Set();
function recordDownloadEvent(change) {
  for(const reader of downloadReaders) reader.set(change.id,{...reader.get(change.id),...change});
}
async function renderDownloads(mount) {
  const version = generation, request = ++downloadRequest;
  const reader=new Map();downloadReaders.add(reader);
  try {
    const [jobs, running] = await Promise.all([api().Jobs(), api().IsRunning()]); if (generation !== version || active !== 'downloads' || request !== downloadRequest) return;
    renderDownloadJobs(mount, jobs, api);
    updateDownloadJobs(mount,[...reader.values()]);
    renderDownloadControls(downloadFailureCount(mount), running, downloadJobCount(mount));
  } catch (e) { setStatus(String(e), true); }
  finally {downloadReaders.delete(reader);}
}
async function refreshDownloadChanges(ids) {
  const mount=document.getElementById('content');
  if(!api().JobsByID || !updateDownloadJobs(mount,[]))return renderDownloads(mount);
  const version=generation,request=downloadRequest,reader=new Map();downloadReaders.add(reader);
  try {
    const [jobs,running]=await Promise.all([api().JobsByID(ids),api().IsRunning()]);
    if(generation!==version || active!=='downloads' || request!==downloadRequest)return;
    updateDownloadJobs(mount,jobs);updateDownloadJobs(mount,[...reader.values()]);
    renderDownloadControls(downloadFailureCount(mount),running,downloadJobCount(mount));
  } catch(e) {setStatus(String(e),true);}
  finally {downloadReaders.delete(reader);}
}
let jobTimer, favoritesTimer;
export function bindGlobalEvents() {
  bindLogEvents();
  let refreshing=false, dirty=false, full=false, scoreboard=false;
  const changed=new Set(),progress=new Map();
  const refreshJobs = () => {
    dirty=true;
    if (jobTimer != null || refreshing) return;
    jobTimer=setTimeout(async()=> {
      jobTimer=null;refreshing=true;dirty=false;
      const ids=[...changed],updates=[...progress.values()],readAll=full,refreshCounts=scoreboard;
      changed.clear();progress.clear();full=false;scoreboard=false;
      try {
        if(active==='downloads')updateDownloadJobs(document.getElementById('content'),updates);
        await Promise.all([active==='downloads'?(readAll?renderDownloads(document.getElementById('content')):ids.length?refreshDownloadChanges(ids):null):null,refreshCounts?refreshScoreboard():null]);
      }
      finally { refreshing=false;if(dirty)refreshJobs(); }
    },100);
  };
  window.runtime?.EventsOn('job:progress',(id,received,total)=> {
    if(Number.isInteger(id) && Number.isFinite(received) && Number.isFinite(total)) {
      const change={id,received,total};progress.set(id,change);recordDownloadEvent(change);
    } else {full=true;scoreboard=true;}
    refreshJobs();
  });
  for(const event of ['job:queued','job:started','job:finished']) window.runtime?.EventsOn(event,(id,status,error)=> {
    if(Number.isInteger(id)) {
      changed.add(id);
      recordDownloadEvent({id,status:event==='job:queued'?'queued':event==='job:started'?'running':status,error:event==='job:finished'?error || '':''});
    } else full=true;
    scoreboard=true;refreshJobs();
  });
  window.runtime?.EventsOn('wave:finished',()=> {full=true;scoreboard=true;refreshJobs();});
  window.runtime?.EventsOn('library:changed',()=> {full=true;refreshJobs();refreshFavorites();refreshHistCount();});
  window.runtime?.EventsOn('scan:update', (scanned, queued, skipped) => {
    setStatus(`Просканировано ${scanned}, в очереди ${queued}, пропущено ${skipped}`);
    // Live counters while scanning; the full summary refresh follows on job events.
    const set = (id, value) => { const node = document.getElementById(id); if (node) { node.textContent = String(value || 0); node.parentElement?.classList.toggle('zero', !value); } };
    set('stat-scanned', scanned);
    document.getElementById('run-strip')?.classList.remove('idle');
  });
  let confirmation;
  window.runtime?.EventsOn('scan:finished', async err => {
    setStatus(err || 'Сканирование завершено', !!err);
    if (err) return;
    try {
      const summary = await api().Summary(), id = summary.awaiting_confirmation;
      if (!id) return;
      confirmation?.abort();
      const controller = confirmation = new AbortController();
      setStatus('Сканирование завершено. Ожидание подтверждения скачивания.');
      const accepted = await confirmDownload(`Просканировано: ${summary.scanned}. Пропущено: ${summary.duplicates}. В очереди: ${summary.total} файлов. Начать скачивание?`, controller.signal);
      if (await api().ConfirmRun(id, accepted)) setStatus(accepted ? 'Скачивание началось' : 'Скачивание отменено');
      if (confirmation === controller) confirmation = null;
      refreshScoreboard();
    } catch (e) { setStatus(String(e), true); }
  });
  window.runtime?.EventsOn('wave:finished', () => confirmation?.abort());
  window.runtime?.EventsOn('wave:finished', () => { refreshHistCount(); setStatus('Загрузка завершена'); });
  const refreshFavorites = () => {
    clearTimeout(favoritesTimer);
    favoritesTimer = setTimeout(() => { if (active === 'favorites') renderFavorites(document.getElementById('content')); }, 120);
  };
  window.runtime?.EventsOn('authors:changed', refreshFavorites);
  window.runtime?.EventsOn('favorites:changed', () => window.dispatchEvent(new Event('favorites-changed')));
  window.addEventListener('favorites-changed', refreshFavorites);
  window.addEventListener('open-post', e => { if (e.detail) openViewer(e.detail); });
  if (state.settings?.check_authors_on_startup) checkAuthors().catch(e => setStatus(String(e), true));
}
