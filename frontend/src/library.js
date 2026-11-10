import { el, selectEl, state, setStatus } from './main.js';
import { bindLogEvents, renderLogs } from './logs.js';
import { dismissOverlay } from './motion.js';
import { titlebar as windowTitlebar } from './titlebar.js';
import { mediaPlaceholder, mediaSkeleton, fileKind } from './media-placeholder.js';

const api = () => window.go.main.App;
const FILE_PAGE = 120, GROUP_PAGE = 20;
const basename = path => String(path || '').split(/[\\/]/).at(-1);
const plural = (n, one, few, many) => { const a = Math.abs(n) % 100, b = a % 10; return a > 10 && a < 20 ? many : b === 1 ? one : b >= 2 && b <= 4 ? few : many; };
const count = (n, one, few, many) => `${Number(n || 0).toLocaleString('ru-RU')} ${plural(Number(n || 0), one, few, many)}`;
export function bytes(n) {
  n = Number(n) || 0;
  for (const [size, unit] of [[1 << 30, 'ГБ'], [1 << 20, 'МБ'], [1 << 10, 'КБ']]) if (n >= size) return new Intl.NumberFormat('ru-RU', { maximumFractionDigits: 1 }).format(n / size) + ' ' + unit;
  return n + ' Б';
}
const siteLabel = key => state.sites.find(s => s.key === key)?.label || key || '?';
const who = m => m.group_name ? `Группа «${m.group_name}»` : `${siteLabel(m.site)} / ${m.author}`;

export const LAYOUTS = [
  { value: 'flat', label: 'Одна папка', tree: ['_authors/', '  Foo/', '    123_e621.png', '    456_p0.png', '    album/…'], hint: 'Все работы группы лежат вместе. Источник каждого файла сохраняется в каталоге.' },
  { value: 'by_site', label: 'По сайтам внутри группы', tree: ['_authors/', '  Foo/', '    e621/123.png', '    pixiv/456_p0.png'], hint: 'Общая папка автора с подпапками источников.' },
  { value: 'primary', label: 'В папке основного источника', tree: ['e621/', '  Foo/', '    123.png', '    456_p0.png'], hint: 'Группа лежит рядом с обычными авторами сайта, выбранного основным.' },
];
const KINDS = { delete: 'Удаление в карантин', dedupe: 'Удаление дублей', move: 'Перенос папок', convert: 'Конвертация', restore: 'Восстановление', authors: 'Группа авторов', layout: 'Структура папок' };
const STATUSES = { prepared: 'Подготовлено', applying: 'Выполняется', done: 'Завершено', failed: 'Ошибка / отменено', recovery_required: 'Требуется восстановление', restored: 'Отменено', purging: 'Очистка карантина прервана', purged: 'Карантин очищен' };
const MODES = [{ value: 'bytes', label: 'Точные копии файлов' }, { value: 'pixels', label: 'Одинаковые пиксели' }, { value: 'similar', label: 'Похожие изображения' }, { value: 'name', label: 'Одинаковое имя' }];
const CANDIDATE_MODES = [{ value: 'similar', label: 'Похожие (pHash)' }, { value: 'pixels', label: 'Одинаковые пиксели' }, { value: 'bytes', label: 'Точные копии' }];

const field = (label, input, attrs = {}) => el('label', { class: 'dlg-field', ...attrs }, el('span', { text: label }), input);
const button = (text, fn, attrs = {}) => el('button', { type: 'button', class: 'pv-btn ghost', text, ...attrs, onclick: async e => { try { await fn(e); } catch (err) { setStatus(String(err?.message || err), true); } } });
function check(label, value, change, attrs = {}) {
  const input = el('input', { type: 'checkbox', 'aria-label': label });
  input.checked = !!value; input.addEventListener('change', () => change(input.checked));
  return el('label', { class: 'library-check', ...attrs }, input, el('span', { text: label }));
}
function input(value, label, change, type = 'text', attrs = {}) {
  return el('input', { class: 'field', type, value: String(value ?? ''), 'aria-label': label, ...attrs, oninput: e => change(e.target.value) });
}

export function chooseKeeper(files, preferred = '', policy = 'quality') {
  const source = f => preferred && (f.site === preferred || (f.origins || []).some(o => o.site === preferred)) ? 1 : 0;
  return [...files].sort((a, b) => {
    const quality = (b.width || 0) * (b.height || 0) - (a.width || 0) * (a.height || 0), origin = source(b) - source(a);
    return (policy === 'source' ? origin || quality : quality || origin) || (b.size || 0) - (a.size || 0) || (a.path || '').localeCompare(b.path || '');
  })[0]?.id;
}

export function operationTitle(plan) {
  if (plan.kind === 'restore') return `Отмена: ${KINDS[plan.original_kind] || plan.original_kind || 'операция'}`;
  if (plan.kind === 'layout') {
    const name = v => LAYOUTS.find(l => l.value === v)?.label || v;
    return `Структура папок: ${name(plan.snapshot?.layout)} → ${name(plan.layout)}`;
  }
  if (plan.kind === 'authors') {
    const after = plan.groups?.[0], before = plan.snapshot?.groups || [];
    if (!after) return `Группа «${before[0]?.name || '?'}» распущена`;
    if (!before.length) return `Объединение: «${after.name}»`;
    return `Группа «${after.name}» изменена`;
  }
  return KINDS[plan.kind] || plan.kind;
}

function planCounts(plan) {
  let moved = 0, removed = 0, created = 0;
  for (const c of plan.changes || []) {
    if (c.after?.state === 'quarantined') removed++;
    else if (c.after?.id && c.before?.id !== c.after.id) created++;
    else moved++;
  }
  return { moved, removed, created };
}

function modal(title, build, { wide = false } = {}) {
  const previous = document.activeElement;
  const overlay = el('div', { class: 'viewer-overlay library-overlay', role: 'dialog', 'aria-modal': 'true', 'aria-label': title, tabindex: '-1' });
  let closed = false;
  const close = () => { if (closed) return; closed = true; dismissOverlay(overlay); previous?.isConnected && previous.focus?.(); };
  const body = el('div', { class: 'library-dialog-body' });
  const box = el('div', { class: 'viewer-box library-dialog' + (wide ? ' wide' : '') },
    el('div', { class: 'viewer-header' }, el('strong', { text: title }), button('×', close, { class: 'viewer-close', 'aria-label': 'Закрыть' })), body);
  overlay.append(box);
  overlay.addEventListener('click', e => { if (e.target === overlay) close(); });
  overlay.addEventListener('keydown', e => { if (e.key === 'Escape') { e.preventDefault(); close(); } });
  document.body.append(overlay); overlay.focus?.();
  build(body, close, () => !closed);
  return close;
}

export async function mountLibrary(mount) {
  document.title = 'Библиотека — MassDownloader';
  const prefs = { policy: 'quality', preferred: '', candidate_mode: 'similar', candidate_threshold: 6, min_shared: 2, min_ratio: 0.1, sort: '', ...(state.settings?.library_prefs || {}) };
  const filters = { site: '', author: '', group: '', search: '', format: '', sort: prefs.sort || '' };
  let tab = 'files', authorsView = 'candidates', revision = 0, busy = false, currentTask = '', ended = false, loadingStatus = false;
  let mode = 'bytes', threshold = 8, fullScan = false, filterTimer, poll, observer;
  let files = [], filesTotal = 0, groups = [], groupsTotal = 0, loadingMore = false, lastClicked = -1;
  let overview = { layout: 'flat', groups: [] }, roots = [];
  const selected = new Set(), duplicateChoices = new Map(), unbind = [];

  // ----- shell -----
  const rootsList = el('div', { class: 'library-roots', role: 'list', 'aria-label': 'Папки библиотеки' });
  const status = el('span', { id: 'status-line', text: 'Выберите папки и запустите сканирование' });
  const statusDot = el('span', { id: 'status-dot', class: 'status-dot' });
  const progress = el('progress', { max: '1', value: '0', hidden: true, 'aria-label': 'Ход операции' });
  const stats = el('div', { class: 'library-stats' });
  const content = el('div', { id: 'content', class: 'library-content', dataset: { tab } });
  const tools = el('div', { class: 'library-tools' });
  const nav = el('nav', { class: 'library-tabs', 'aria-label': 'Разделы библиотеки' });
  const actionbar = el('div', { class: 'library-actionbar', hidden: true });
  const cancel = button('Отменить', () => api().LibraryCancel(), { hidden: true });
  const scan = button('Сканировать', async () => {
    if (!roots.length) throw new Error('Добавьте папку для сканирования');
    await begin(() => fullScan ? api().LibraryRescan(roots) : api().LibraryScan(roots));
  }, { class: 'pv-btn' });
  const groupChip = el('div', { class: 'library-filter-chip', hidden: true });

  function renderRoots() {
    rootsList.replaceChildren(...roots.map(path => el('div', { class: 'library-root', role: 'listitem', title: path },
      el('span', { text: path }), button('×', () => { roots = roots.filter(p => p !== path); renderRoots(); }, { class: 'library-root-remove', 'aria-label': `Убрать папку ${path}` }))));
    if (!roots.length) rootsList.append(el('p', { class: 'muted library-hint', text: 'Папок нет' }));
  }
  const addRoot = button('+ Папка', async () => {
    const bridge = window.go.main.FeedWindow;
    const path = bridge?.PickDirectory ? await bridge.PickDirectory() : await api().PickDirectory();
    if (path && !roots.includes(path)) { roots.push(path); renderRoots(); }
  });
  const filterChanged = () => {
    selected.clear(); duplicateChoices.clear(); lastClicked = -1;
    clearTimeout(filterTimer); filterTimer = setTimeout(() => load(), 200);
  };
  const siteSelect = selectEl([{ value: '', label: 'Все источники' }, ...state.sites.map(s => ({ value: s.key, label: s.label }))], '', v => { filters.site = v; filterChanged(); }, { 'aria-label': 'Источник' });
  const formatSelect = selectEl([{ value: '', label: 'Все форматы' }, ...['jpg', 'png', 'webp', 'gif', 'avif', 'bmp', 'tiff'].map(v => ({ value: v, label: v.toUpperCase() }))], '', v => { filters.format = v; filterChanged(); }, { 'aria-label': 'Формат' });
  const sortSelect = selectEl([{ value: '', label: 'По пути' }, { value: 'newest', label: 'Сначала новые' }, { value: 'largest', label: 'Сначала большие' }, { value: 'resolution', label: 'По разрешению' }], filters.sort, v => { filters.sort = v; savePrefs({ sort: v }); filterChanged(); }, { 'aria-label': 'Сортировка' });
  const authorInput = input('', 'Автор или группа', v => { filters.author = v; filterChanged(); }, 'search', { placeholder: 'ник или группа' });
  const searchInput = input('', 'Поиск по пути', v => { filters.search = v; filterChanged(); }, 'search', { placeholder: 'часть пути' });
  function setGroupFilter(group) {
    filters.group = group?.id || '';
    groupChip.hidden = !group;
    groupChip.replaceChildren(...(group ? [el('span', { text: `Группа: ${group.name}` }), button('×', () => { setGroupFilter(null); filterChanged(); }, { class: 'library-root-remove', 'aria-label': 'Снять фильтр группы' })] : []));
  }

  const sidebar = el('aside', { class: 'library-sidebar' },
    el('section', { class: 'library-side-section' }, el('h2', { text: 'Папки' }), rootsList,
      el('div', { class: 'library-sidebar-actions' }, addRoot, scan),
      check('Пересчитать хеши всех файлов', false, v => fullScan = v)),
    el('section', { class: 'library-side-section' }, el('h2', { text: 'Фильтр' }), groupChip,
      field('Источник', siteSelect), field('Автор / группа', authorInput), field('Путь', searchInput), field('Формат', formatSelect), field('Сортировка файлов', sortSelect)));

  const TABS = [['files', 'Файлы'], ['duplicates', 'Дубли'], ['authors', 'Авторы'], ['operations', 'Операции'], ['logs', 'Логи'], ['settings', 'Настройки']];
  for (const [id, label] of TABS) nav.append(button(label, () => switchTab(id), { class: 'tab' + (id === tab ? ' active' : ''), dataset: { tab: id }, 'aria-label': label }));
  function switchTab(id) {
    tab = id; lastClicked = -1;
    for (const b of nav.children) b.classList.toggle('active', b.dataset.tab === id);
    load();
  }

  const titlebar = windowTitlebar({ title: 'MassDownloader · Библиотека', mark: 'library', closeTitle: 'Закрыть библиотеку' });
  mount.replaceChildren(el('div', { class: 'window library-window' }, titlebar,
    el('header', { class: 'library-header' }, el('h1', { text: 'Библиотека изображений' }), stats, cancel),
    el('div', { class: 'library-body' }, sidebar, el('main', { class: 'library-main' }, nav, tools, content, actionbar)),
    el('div', { class: 'library-status' }, statusDot, status, progress)));
  bindLogEvents();

  // ----- shared state -----
  const query = extra => ({ ...filters, mode, threshold: Number(threshold), ...extra });
  async function savePrefs(patch) {
    Object.assign(prefs, patch);
    try { await api().LibrarySetPrefs?.(patch); } catch (e) { setStatus(String(e), true); }
  }
  async function begin(fn) {
    if (busy) throw new Error('Операция библиотеки уже выполняется');
    currentTask = await fn(); busy = true; updateBusy(); await readStatus();
  }
  function updateBusy() {
    scan.disabled = busy; cancel.hidden = !busy; progress.hidden = !busy;
    for (const control of document.querySelectorAll('.library-window [data-mutation], .library-overlay [data-mutation]')) control.disabled = busy;
    updateActionbar(); updateDuplicateSelection();
  }
  async function info() {
    const value = await api().LibraryInfo();
    if (ended) return;
    if (!roots.length) { roots = [...(value.roots || [])].filter(Boolean); renderRoots(); }
    // replaceChildren() would print a skipped part as the text «null».
    stats.replaceChildren(...[
      el('span', { text: count(value.count, 'файл', 'файла', 'файлов') }),
      value.size ? el('span', { text: bytes(value.size) }) : null,
      value.quarantine ? el('span', { class: 'warn', text: `карантин: ${count(value.quarantine, 'файл', 'файла', 'файлов')} · ${bytes(value.quarantine_bytes)}` }) : null,
      value.limited ? el('span', { class: 'warn', title: 'Для этих файлов доступны только имя и точные копии', text: `ограниченный анализ: ${value.limited}` }) : null].filter(Boolean));
  }
  function taskStatus(value) {
    if (ended || !value) return;
    if (currentTask && value.id !== currentTask && busy) return;
    const wasBusy = busy;
    busy = !!value.running;
    if (value.id) currentTask = value.id;
    progress.max = value.total || 1; progress.value = value.done || 0;
    if (value.stage) setStatus(`${value.stage}${value.total ? ` · ${value.done} / ${value.total}` : ''}${value.path ? ` · ${basename(value.path)}` : ''}${value.error ? ` · ${value.error}` : ''}`, !!value.error);
    updateBusy();
    if (wasBusy && !busy) { selected.clear(); duplicateChoices.clear(); info().catch(e => setStatus(String(e), true)); load(); }
  }
  async function readStatus() {
    if (ended || loadingStatus) return;
    loadingStatus = true;
    try { taskStatus(await api().LibraryStatus()); } catch (e) { setStatus(String(e), true); } finally { loadingStatus = false; }
  }

  // ----- thumbnails -----
  function resetImages() {
    observer?.disconnect();
    observer = typeof IntersectionObserver !== 'undefined' ? new IntersectionObserver(entries => { for (const e of entries) if (e.isIntersecting) hydrate(e.target); }, { root: content, rootMargin: '300px' }) : null;
  }
  async function hydrate(img) {
    observer?.unobserve(img);
    const id = img.dataset.fileId;
    try {
      const url = api().LibraryThumbnail ? await api().LibraryThumbnail(id) : await api().LibraryFileURL(id);
      if (!img.isConnected) return;
      if (!url) { img.dispatchEvent(new Event('thumb-missing')); return; }
      img.addEventListener('error', async () => {
        try { const original = await api().LibraryFileURL(id); if (img.isConnected && original) img.src = original; else img.dispatchEvent(new Event('thumb-missing')); } catch { img.dispatchEvent(new Event('thumb-missing')); }
      }, { once: true });
      img.src = url;
    } catch (e) { if (img.isConnected) { img.title = String(e); img.dispatchEvent(new Event('thumb-missing')); } }
  }
  function thumb(file, onOpen = () => compare([file]), cls = '') {
    const img = el('img', { alt: `${file.width || '?'}×${file.height || '?'} · ${basename(file.path)}`, loading: 'lazy', dataset: { fileId: file.id } });
    const box = button('', onOpen, { class: `library-thumbnail loading ${cls}`, 'aria-label': `Просмотреть ${basename(file.path)}`, title: file.path });
    let errors = 0;
    img.addEventListener('load', () => { box.classList.remove('loading', 'failed'); box.querySelector('.media-ph')?.remove(); });
    const missing = () => { box.classList.remove('loading'); box.classList.add('failed'); if (!box.querySelector('.media-ph')) box.append(mediaPlaceholder(fileKind(file.format), { label: 'Нет превью', ext: file.format || '' })); };
    img.addEventListener('error', () => { if (++errors >= (api().LibraryThumbnail ? 2 : 1)) missing(); });
    img.addEventListener('thumb-missing', missing);
    box.append(mediaSkeleton('image'), img);
    if (observer) observer.observe(img); else hydrate(img);
    return box;
  }
  const dims = f => `${f.width || '?'}×${f.height || '?'}`;
  const sourceText = f => {
    const origins = [...new Set((f.origins || []).map(o => `${siteLabel(o.site)} #${o.post_id}${o.page_index > 0 ? ` · ${o.page_index + 1}` : ''}`))];
    return origins.join(' / ') || [f.site && siteLabel(f.site), f.author].filter(Boolean).join(' / ') || 'Источник не определён';
  };

  // ----- preview / apply -----
  async function preview(request) {
    if (busy) return;
    const plan = await api().LibraryPreview(request);
    if (plan.kind === 'layout' && !(plan.changes || []).length) { await begin(() => api().LibraryApply(plan.id)); return; }
    showPlan(plan);
  }
  function showPlan(plan) {
    modal('Предпросмотр операции', (body, close) => {
      const { moved, removed, created } = planCounts(plan);
      const facts = [];
      if (moved) facts.push(`переместить: ${count(moved, 'файл', 'файла', 'файлов')}`);
      if (removed) facts.push(`в карантин: ${count(removed, 'файл', 'файла', 'файлов')}`);
      if (created) facts.push(`создать: ${count(created, 'копию', 'копии', 'копий')}`);
      body.append(el('p', { class: 'library-plan-title' }, el('strong', { text: operationTitle(plan) }), el('span', { class: 'muted', text: facts.length ? ' · ' + facts.join(' · ') : ' · файлы не перемещаются' })));
      const notes = [];
      if (plan.kind === 'authors' && plan.groups?.[0]) notes.push(`Новые загрузки участников будут сохраняться в папку группы «${plan.groups[0].folder}». Подписки и источники постов не меняются.`);
      if (plan.kind === 'authors' && !plan.groups?.length) notes.push('Файлы вернутся в папки своих источников. Новые загрузки снова пойдут в обычные папки авторов.');
      if (plan.kind === 'layout') notes.push('Файлы всех групп переедут в новую структуру. Новые загрузки сразу пойдут по новому пути.');
      if (removed && plan.kind !== 'delete') notes.push('Одинаковые файлы уходят в карантин. Посты будут открывать сохранённую версию.');
      if (plan.kind === 'delete') notes.push('Избранное и история скачивания сохранятся.');
      if (plan.kind === 'convert' && !plan.request?.conversion?.replace) notes.push('Будут созданы копии, оригиналы останутся основными файлами постов.');
      if (plan.skipped) notes.push(`Пропущено групп с отличающимися версиями: ${plan.skipped}.`);
      notes.push('Операцию можно отменить на вкладке «Операции», пока файлы не изменены.');
      for (const n of notes) body.append(el('p', { class: 'muted', text: n }));
      if ((plan.changes || []).length) {
        const list = el('div', { class: 'library-plan-list' });
        for (const c of plan.changes.slice(0, 150)) {
          const quarantined = c.after?.state === 'quarantined';
          const target = quarantined ? `Карантин${c.keep ? ` · оставлен ${basename(c.keep_path || c.keep.path)}` : ''}` : c.after?.path;
          list.append(el('div', { class: 'library-plan-item' + (quarantined ? ' removed' : '') }, el('span', { text: c.before.path }), el('span', { text: '→ ' + target })));
        }
        if (plan.changes.length > 150) list.append(el('p', { class: 'muted', text: `И ещё ${plan.changes.length - 150}.` }));
        body.append(list);
      }
      const apply = button('Применить', async () => {
        apply.disabled = true;
        try { await begin(() => api().LibraryApply(plan.id)); close(); } catch (e) { apply.disabled = false; throw e; }
      }, { class: 'pv-btn library-apply' });
      body.append(el('div', { class: 'viewer-actions' }, button('Отмена', close), apply));
    }, { wide: true });
  }

  // ----- files -----
  function updateActionbar() {
    const show = tab === 'files' && selected.size > 0;
    actionbar.hidden = !show;
    if (!show) return;
    actionbar.replaceChildren(
      el('strong', { text: `Выбрано: ${selected.size.toLocaleString('ru-RU')}` }),
      button('Снять выбор', () => { selected.clear(); syncChecks(); }),
      el('span', { class: 'tb-spacer' }),
      button('Конвертировать…', () => convertDialog(), { 'data-mutation': true, disabled: busy }),
      button('Удалить…', () => deleteDialog(), { 'data-mutation': true, disabled: busy, class: 'pv-btn ghost danger-ghost' }));
  }
  function syncChecks() {
    for (const node of content.querySelectorAll('.library-file[data-id]')) {
      const on = selected.has(node.dataset.id);
      node.classList.toggle('selected', on);
      const box = node.querySelector('input[type=checkbox]');
      if (box) box.checked = on;
    }
    updateActionbar();
  }
  function fileTile(file, index) {
    const box = el('input', { type: 'checkbox', 'aria-label': `Выбрать ${basename(file.path)}` });
    box.checked = selected.has(file.id);
    box.addEventListener('click', e => {
      if (e.shiftKey && lastClicked >= 0) {
        const [a, b] = [Math.min(lastClicked, index), Math.max(lastClicked, index)];
        for (const f of files.slice(a, b + 1)) box.checked ? selected.add(f.id) : selected.delete(f.id);
      } else box.checked ? selected.add(file.id) : selected.delete(file.id);
      lastClicked = index; syncChecks();
    });
    return el('article', { class: 'library-file' + (selected.has(file.id) ? ' selected' : ''), dataset: { id: file.id } },
      el('label', { class: 'library-file-select' }, box), thumb(file),
      el('div', { class: 'library-file-meta' },
        el('strong', { text: basename(file.path), title: file.path }),
        el('span', { text: `${dims(file)} · ${(file.format || '').toUpperCase()}${file.bit_depth === 16 ? ' · 16 бит' : ''} · ${bytes(file.size)}${file.frames > 1 ? ` · ${file.frames} кадр.` : ''}` }),
        el('span', { text: sourceText(file), title: file.path }),
        file.error ? el('span', { class: 'library-limited', text: file.error }) : null),
      button('Показать в папке', () => api().LibraryReveal(file.id), { class: 'library-reveal' }));
  }
  async function loadFiles(request, append) {
    const page = await api().LibraryFiles(query({ offset: append ? files.length : 0, limit: FILE_PAGE }));
    if (ended || request !== revision) return;
    const start = append ? files.length : 0;
    if (!append) files = [];
    files.push(...page.files); filesTotal = page.total;
    let grid = content.querySelector('.library-file-grid');
    if (!append) {
      content.replaceChildren();
      tools.replaceChildren(el('div', { class: 'library-selection-tools' },
        el('span', { class: 'library-selection', text: `${count(filesTotal, 'файл', 'файла', 'файлов')} по фильтру` }),
        button('Выбрать все по фильтру', async () => { for (const id of await api().LibrarySelect(query())) selected.add(id); syncChecks(); }),
        el('span', { class: 'muted library-hint', text: 'Shift + клик — выбрать диапазон' })));
      grid = el('div', { class: 'library-file-grid' }); content.append(grid);
      if (!filesTotal) content.append(el('div', { class: 'empty', text: roots.length ? 'По фильтру ничего не найдено. Если папки ещё не сканировались — нажмите «Сканировать».' : 'Добавьте папку и запустите сканирование.' }));
    }
    page.files.forEach((f, i) => grid.append(fileTile(f, start + i)));
    updateActionbar();
  }
  function deleteDialog() {
    let useCopies = true, allowRedownload = false;
    modal('Удалить выбранные', (body, close) => {
      body.append(el('p', { text: `Файлов: ${selected.size}. Они будут перемещены в карантин внутри своей папки библиотеки.` }),
        check('Если есть точные или конвертированные копии — посты будут открывать их', useCopies, v => useCopies = v),
        check('Разрешить автоматическую повторную загрузку этих постов', allowRedownload, v => allowRedownload = v),
        el('div', { class: 'viewer-actions' }, button('Отмена', close), button('Предпросмотр', async () => { close(); await preview({ kind: 'delete', ids: [...selected], use_copies: useCopies, allow_redownload: allowRedownload }); }, { class: 'pv-btn' })));
    });
  }
  function convertDialog() {
    const conversion = { format: 'webp', quality: 90, max_width: 0, max_height: 0, replace: false, metadata: true, background: 'white', allow_depth_loss: false };
    modal('Конвертация', (body, close) => {
      const options = el('div', { class: 'library-form' });
      const render = () => {
        options.replaceChildren(field('Формат', selectEl([{ value: 'webp', label: 'WebP без потерь' }, { value: 'png', label: 'PNG без потерь' }, { value: 'jpg', label: 'JPEG' }], conversion.format, v => { conversion.format = v; render(); }, { 'aria-label': 'Выходной формат' })));
        if (conversion.format === 'webp') options.append(check('Разрешить перевод 16 бит в 8 бит', conversion.allow_depth_loss, v => conversion.allow_depth_loss = v));
        if (conversion.format === 'jpg') {
          options.append(field('Качество JPEG (1–100)', input(conversion.quality, 'Качество JPEG', v => conversion.quality = Number(v), 'number', { min: '1', max: '100' })),
            field('Фон прозрачных областей', selectEl([{ value: 'white', label: 'Белый' }, { value: 'black', label: 'Чёрный' }], conversion.background, v => conversion.background = v, { 'aria-label': 'Фон прозрачных областей' })));
        }
        options.append(el('div', { class: 'library-form-row' },
          field('Макс. ширина (0 — нет)', input(conversion.max_width, 'Максимальная ширина', v => conversion.max_width = Number(v), 'number', { min: '0' })),
          field('Макс. высота (0 — нет)', input(conversion.max_height, 'Максимальная высота', v => conversion.max_height = Number(v), 'number', { min: '0' }))),
        check('Заменить оригиналы (оригиналы уйдут в карантин)', conversion.replace, v => conversion.replace = v),
        check('Сохранить EXIF и ICC-профиль', conversion.metadata, v => conversion.metadata = v));
      };
      render();
      body.append(el('p', { text: `Файлов: ${selected.size}. Анимация в один кадр не конвертируется; EXIF-ориентация применяется.` }), options,
        el('div', { class: 'viewer-actions' }, button('Отмена', close), button('Предпросмотр конвертации', async () => { close(); await preview({ kind: 'convert', ids: [...selected], conversion: { ...conversion } }); }, { class: 'pv-btn' })));
    });
  }

  // ----- duplicates -----
  function updateDuplicateSelection() {
    const summary = tools.querySelector('[data-role=dupe-summary]');
    if (!summary) return;
    let removing = 0, chosen = 0, unconfirmed = 0;
    for (const choice of duplicateChoices.values()) {
      if (!choice.remove.size) continue;
      chosen++; removing += choice.remove.size;
      const keeper = choice.files.find(f => f.id === choice.keep);
      if (!choice.different && choice.files.some(f => choice.remove.has(f.id) && f.sha256 !== keeper?.sha256)) unconfirmed++;
    }
    summary.textContent = `К удалению: ${removing} · групп: ${chosen}`;
    tools.querySelector('[data-role=dupe-preview]').disabled = busy || !removing || !!unconfirmed;
    tools.querySelector('[data-role=dupe-clear]').disabled = busy || !duplicateChoices.size;
    const hint = tools.querySelector('[data-role=dupe-hint]');
    hint.hidden = !unconfirmed; hint.textContent = unconfirmed ? `Подтвердите замену отличающихся версий в группах: ${unconfirmed}.` : '';
  }
  function renderDuplicateTools() {
    const method = selectEl(MODES, mode, v => { mode = v; duplicateChoices.clear(); load(); }, { 'aria-label': 'Режим поиска дублей' });
    const options = el('div', { class: 'library-options' }, field('Проверка', method));
    if (mode === 'similar') {
      const distance = input(threshold, 'Допустимое расстояние pHash', v => threshold = Number(v), 'number', { min: '0', max: '20' });
      distance.addEventListener('change', () => { threshold = Math.max(0, Math.min(20, Number(distance.value) || 0)); duplicateChoices.clear(); load(); });
      options.append(field('Расстояние pHash (0–20)', distance, { title: 'Меньше — строже' }));
    }
    options.append(
      field('Оставлять', selectEl([{ value: 'quality', label: 'Лучшее разрешение' }, { value: 'source', label: 'Предпочтительный источник' }], prefs.policy, v => { savePrefs({ policy: v }); load(); }, { 'aria-label': 'Приоритет сохраняемой версии' })),
      field('Предпочтительный источник', selectEl([{ value: '', label: 'Без предпочтения' }, ...state.sites.map(s => ({ value: s.key, label: s.label }))], prefs.preferred, v => { savePrefs({ preferred: v }); load(); }, { 'aria-label': 'Предпочтительный источник' })),
      button('Авторазбор всех групп…', () => autoDialog(), { 'data-mutation': true, class: 'pv-btn' }));
    tools.replaceChildren(options, el('div', { class: 'library-selection-tools' },
      el('span', { class: 'library-selection', 'aria-live': 'polite', dataset: { role: 'dupe-summary' } }),
      el('span', { class: 'library-limited', hidden: true, dataset: { role: 'dupe-hint' } }),
      button('Снять выбор', () => { duplicateChoices.clear(); load(); }, { dataset: { role: 'dupe-clear' } }),
      button('Предпросмотр удаления', () => preview({ kind: 'dedupe', dedupe_groups: [...duplicateChoices.values()].filter(c => c.remove.size).map(c => ({ ids: [...c.remove], keep_id: c.keep, allow_different: c.different })) }), { class: 'pv-btn', dataset: { role: 'dupe-preview' } })));
    updateDuplicateSelection();
  }
  function autoDialog() {
    let allowDifferent = false;
    modal('Авторазбор дублей', (body, close) => {
      const policy = prefs.policy === 'source' && prefs.preferred ? `файл из «${siteLabel(prefs.preferred)}», затем лучшее разрешение` : 'файл с наибольшим разрешением';
      body.append(el('p', { text: `Групп по текущему фильтру: ${groupsTotal}. В каждой будет оставлен ${policy}, остальные — в карантин.` }),
        el('p', { class: 'muted', text: 'Группы, где версии отличаются байтами (похожие, одинаковые пиксели с разными метаданными, одинаковые имена), пропускаются, если не разрешить замену.' }),
        check('Разрешить замену отличающихся версий', allowDifferent, v => allowDifferent = v),
        el('div', { class: 'viewer-actions' }, button('Отмена', close), button('Предпросмотр', async () => {
          close();
          await preview({ kind: 'dedupe', auto: { query: query({ offset: 0, limit: 0 }), policy: prefs.policy, preferred: prefs.preferred, allow_different: allowDifferent } });
        }, { class: 'pv-btn' })));
    });
  }
  function groupCard(group) {
    const choice = duplicateChoices.get(group.id) || { keep: chooseKeeper(group.files, prefs.preferred, prefs.policy), remove: new Set(), different: false };
    choice.files = group.files;
    const remove = choice.remove, controls = new Map();
    const best = Math.max(...group.files.map(f => (f.width || 0) * (f.height || 0)));
    const ordered = [...group.files].sort((a, b) => Number(b.id === choice.keep) - Number(a.id === choice.keep));
    const row = el('div', { class: 'library-dupe-row' });
    const refresh = (save = true) => {
      if (save) duplicateChoices.set(group.id, choice);
      for (const [id, c] of controls) {
        c.radio.checked = id === choice.keep; c.remove.checked = remove.has(id); c.remove.disabled = id === choice.keep;
        c.node.classList.toggle('keeper', id === choice.keep); c.node.classList.toggle('removing', remove.has(id));
      }
      updateDuplicateSelection();
    };
    let shown = 0;
    const addFile = f => {
      const radio = el('input', { type: 'radio', name: `keep-${group.id}`, 'aria-label': `Сохранить ${basename(f.path)}`, value: f.id });
      const checkRemove = el('input', { type: 'checkbox', 'aria-label': `Убрать ${basename(f.path)}` });
      radio.addEventListener('change', () => { choice.keep = f.id; remove.delete(f.id); refresh(); });
      checkRemove.addEventListener('change', () => { checkRemove.checked ? remove.add(f.id) : remove.delete(f.id); refresh(); });
      const node = el('article', { class: 'library-dupe', dataset: { id: f.id } }, thumb(f, () => compare(group.files)),
        el('div', { class: 'library-file-meta' },
          el('span', { class: (f.width || 0) * (f.height || 0) === best ? 'best' : '', text: `${dims(f)} · ${(f.format || '').toUpperCase()} · ${bytes(f.size)}` }),
          el('span', { text: sourceText(f), title: f.path }),
          group.distances?.[f.id] != null && group.mode === 'similar' ? el('span', { class: 'muted', text: `pHash: ${group.distances[f.id]}` }) : null),
        el('div', { class: 'library-file-choice' }, el('label', { class: 'library-check' }, radio, 'Оставить'), el('label', { class: 'library-check' }, checkRemove, 'Убрать')));
      controls.set(f.id, { radio, remove: checkRemove, node });
      row.append(node);
    };
    for (const f of ordered.slice(0, 24)) addFile(f);
    shown = Math.min(24, ordered.length);
    const more = button(`Ещё ${ordered.length - shown}`, () => {
      for (const f of ordered.slice(shown, shown + 24)) addFile(f);
      shown = Math.min(shown + 24, ordered.length); more.hidden = shown >= ordered.length; more.textContent = `Ещё ${ordered.length - shown}`; refresh(false);
    }, { hidden: shown >= ordered.length });
    const differing = new Set(group.files.map(f => f.sha256)).size > 1;
    const section = el('section', { class: 'library-group', dataset: { groupId: group.id } });
    const head = el('div', { class: 'library-group-head' }, el('strong', { text: count(group.files.length, 'файл', 'файла', 'файлов') }), el('span', { class: 'muted', text: group.reason }), el('span', { class: 'tb-spacer' }),
      button('Убрать остальные', () => { for (const f of group.files) if (f.id !== choice.keep) remove.add(f.id); refresh(); }),
      button('Сравнить', () => compare(group.files)),
      group.files.length <= 200 ? button('Это разные', async () => { await api().LibraryIgnore(group.files.map(f => f.id)); duplicateChoices.delete(group.id); section.remove(); groupsTotal--; updateDuplicateSelection(); }, { 'data-mutation': true, title: 'Больше не показывать эту группу' }) : null);
    section.append(head, row, more);
    if (differing) section.append(check('Разрешить замену отличающихся версий выбранным файлом', choice.different, v => { choice.different = v; refresh(); }, { class: 'library-check library-limited' }));
    refresh(false);
    return section;
  }
  async function loadDuplicates(request, append) {
    const page = await api().LibraryDuplicates(query({ offset: append ? groups.length : 0, limit: GROUP_PAGE }));
    if (ended || request !== revision) return;
    if (!append) { groups = []; content.replaceChildren(); renderDuplicateTools(); }
    groups.push(...page.groups); groupsTotal = page.total;
    if (!append) content.append(el('p', { class: 'muted library-hint', text: groupsTotal ? `Групп: ${groupsTotal}. Выбор сохраняется при прокрутке; смена режима или фильтров его сбрасывает.` : '' }));
    for (const g of page.groups) content.append(groupCard(g));
    if (!append && !groupsTotal) content.append(el('div', { class: 'empty', text: 'Совпадений по выбранному режиму и фильтрам нет' }));
  }

  // ----- authors -----
  function numberPref(key, minValue, maxValue, scale = 1, reload = true) {
    const node = input(Math.round(Number(prefs[key]) * scale), key, () => {}, 'number', { min: String(minValue), max: String(maxValue) });
    node.addEventListener('change', () => { const v = Math.max(minValue, Math.min(maxValue, Number(node.value) || 0)); node.value = String(v); savePrefs({ [key]: v / scale }); if (reload) load(); });
    return node;
  }
  async function loadAuthors(request) {
    overview = await api().LibraryAuthors();
    if (ended || request !== revision) return;
    const seg = el('div', { class: 'library-seg', role: 'tablist' },
      ...[['candidates', 'Кандидаты'], ['groups', `Группы · ${overview.groups.length}`]].map(([id, label]) =>
        button(label, () => { authorsView = id; load(); }, { class: 'library-seg-btn' + (authorsView === id ? ' active' : ''), role: 'tab', 'aria-selected': String(authorsView === id) })));
    tools.replaceChildren(el('div', { class: 'library-selection-tools' }, seg, el('span', { class: 'tb-spacer' }), button('+ Новая группа', () => groupEditor({}), { 'data-mutation': true, class: 'pv-btn' })));
    if (authorsView === 'groups') { renderGroups(); return; }
    tools.append(el('div', { class: 'library-options' },
      field('Совпадение работ', selectEl(CANDIDATE_MODES, prefs.candidate_mode, v => { savePrefs({ candidate_mode: v }); load(); }, { 'aria-label': 'Совпадение работ' })),
      prefs.candidate_mode === 'similar' ? field('Расстояние pHash', numberPref('candidate_threshold', 0, 20)) : null,
      field('Мин. общих работ', numberPref('min_shared', 1, 1000)),
      field('Мин. доля, %', numberPref('min_ratio', 0, 100, 100))));
    content.replaceChildren(el('div', { class: 'empty', text: 'Поиск авторов с общими работами…' }));
    const list = await api().LibraryAuthorCandidates({ mode: prefs.candidate_mode, threshold: Number(prefs.candidate_threshold), min_shared: Number(prefs.min_shared), min_ratio: Number(prefs.min_ratio) });
    if (ended || request !== revision) return;
    content.replaceChildren(el('p', { class: 'muted library-hint', text: 'Авторы с разных сайтов, у которых совпадают работы. Доля — сколько работ меньшего из двух авторов есть у другого. Объединение переносит файлы в одну папку и убирает одинаковые копии.' }));
    if (!list.length) content.append(el('div', { class: 'empty', text: 'Кандидатов нет. Ослабьте пороги или выберите «Похожие», если сайты пережимают изображения.' }));
    for (const c of list) content.append(candidateCard(c));
  }
  function memberChip(m, extra) {
    const amount = m.works ?? m.files;
    return el('span', { class: 'library-member' + (m.group ? ' group' : '') },
      el('span', { class: 'site', text: m.group ? 'группа' : siteLabel(m.site) }), el('span', { text: m.group ? m.group_name : m.author }),
      amount != null ? el('span', { class: 'muted', text: String(amount), title: m.works != null ? 'работ' : 'файлов' }) : null, extra || null);
  }
  function candidateCard(c) {
    const samples = el('div', { class: 'library-samples' });
    for (const sample of c.samples) samples.append(el('div', { class: 'library-sample' }, ...sample.map(f => thumb(f, () => compare(sample), 'small'))));
    const percent = Math.round(c.ratio * 100);
    return el('section', { class: 'library-candidate', dataset: { candidate: c.id } },
      el('div', { class: 'library-group-head' }, el('strong', { text: c.name || who(c.members[0]) }),
        el('span', { class: 'library-badge', text: count(c.shared, 'общая работа', 'общие работы', 'общих работ') }),
        el('span', { class: 'library-badge' + (percent >= 50 ? ' strong' : ''), text: `совпадение ${percent}%` })),
      el('div', { class: 'library-members' }, ...c.members.map(m => memberChip(m))),
      samples,
      el('div', { class: 'viewer-actions' },
        button('Объединить…', () => groupEditor({ candidate: c }), { class: 'pv-btn', 'data-mutation': true }),
        button('Не связаны', async () => { await api().LibraryIgnoreAuthors(c.members.map(m => m.key)); setStatus('Кандидат скрыт'); load(); }, { title: 'Больше не предлагать эту связь' })));
  }
  function renderGroups() {
    content.replaceChildren();
    if (!overview.groups.length) { content.append(el('div', { class: 'empty', text: 'Групп пока нет. Объедините кандидатов или создайте группу вручную.' })); return; }
    const plain = g => g.members.map(({ site, author }) => ({ site, author }));
    for (const g of overview.groups) {
      const detach = m => button('×', () => {
        const rest = plain(g).filter(x => !(x.site === m.site && x.author === m.author));
        if (rest.length < 2) return dissolve(g);
        return preview({ kind: 'authors', authors: { group_id: g.id, name: g.name, members: rest, dedupe: false } });
      }, { class: 'library-root-remove', 'aria-label': `Убрать ${m.site}/${m.author} из группы`, 'data-mutation': true });
      content.append(el('section', { class: 'library-candidate', dataset: { group: g.id } },
        el('div', { class: 'library-group-head' }, el('strong', { text: g.name }),
          el('span', { class: 'muted', text: `${count(g.files, 'файл', 'файла', 'файлов')} · ${bytes(g.size)}` }),
          el('span', { class: 'tb-spacer' }),
          button('Файлы', () => { setGroupFilter(g); switchTab('files'); }),
          button('Изменить…', () => groupEditor({ group: g }), { 'data-mutation': true }),
          button('Распустить…', () => dissolve(g), { 'data-mutation': true })),
        el('code', { class: 'library-path', text: g.path || `${overview.layout === 'primary' ? g.primary_site : '_authors'}/${g.folder}` }),
        el('div', { class: 'library-members' }, ...g.members.map(m => memberChip(m, detach(m)))),
        g.outside ? el('p', { class: 'library-limited' }, `${count(g.outside, 'файл', 'файла', 'файлов')} участников лежат вне папки группы. `,
          button('Собрать', () => preview({ kind: 'authors', authors: { group_id: g.id, name: g.name, members: plain(g), dedupe: false } }), { 'data-mutation': true })) : null,
        g.samples.length ? el('div', { class: 'library-samples flat' }, ...g.samples.map(f => thumb(f, () => compare(g.samples), 'small'))) : null));
    }
  }
  function dissolve(g) {
    modal('Распустить группу', (body, close) => {
      body.append(el('p', { text: `Файлы группы «${g.name}» вернутся в папки своих источников (<сайт>/<автор>). Дубли, убранные при объединении, остаются в карантине — их возвращает отмена той операции.` }),
        el('div', { class: 'viewer-actions' }, button('Отмена', close), button('Предпросмотр', async () => { close(); await preview({ kind: 'authors', authors: { group_id: g.id, dissolve: true } }); }, { class: 'pv-btn' })));
    });
  }
  async function groupEditor({ group, candidate }) {
    if (candidate?.members.some(m => m.group)) overview = await api().LibraryAuthors();
    const groupsByID = new Map(overview.groups.map(g => [g.id, g]));
    const members = [], absorb = [];
    let groupID = group?.id || '', name = group?.name || candidate?.name || '';
    const add = (m, fromGroup = '') => { if (!members.some(x => x.site === m.site && x.author === m.author)) members.push({ site: m.site, author: m.author, files: m.files ?? m.works, checked: true, fromGroup }); };
    for (const m of group?.members || []) add(m, group.id);
    for (const m of candidate?.members || []) {
      if (!m.group) { add(m); continue; }
      const g = groupsByID.get(m.group);
      if (!groupID) { groupID = m.group; name = g?.name || name; } else if (m.group !== groupID && !absorb.includes(m.group)) absorb.push(m.group);
      for (const x of g?.members || []) add(x, m.group);
    }
    let primary = group?.primary_site || '', dedupe = !group;
    modal(group ? `Группа «${group.name}»` : 'Объединение авторов', (body, close) => {
      const list = el('div', { class: 'library-member-list' });
      const primaryBox = el('div');
      const renderPrimary = () => {
        const sites = [...new Set(members.filter(m => m.checked).map(m => m.site))];
        if (primary && !sites.includes(primary)) primary = '';
        primaryBox.replaceChildren(field('Основной источник', selectEl([{ value: '', label: 'Автоматически (больше файлов)' }, ...sites.map(s => ({ value: s, label: siteLabel(s) }))], primary, v => primary = v, { 'aria-label': 'Основной источник' }), { title: 'Нужен для структуры «В папке основного источника»' }));
      };
      const renderMembers = () => {
        list.replaceChildren(...members.map(m => {
          const box = el('input', { type: 'checkbox', 'aria-label': `${m.site}/${m.author}` });
          box.checked = m.checked; box.addEventListener('change', () => { m.checked = box.checked; renderPrimary(); });
          return el('label', { class: 'library-member-row' }, box, el('span', { class: 'site', text: siteLabel(m.site) }), el('span', { text: m.author }),
            m.files != null ? el('span', { class: 'muted', text: String(m.files) }) : null,
            m.fromGroup && m.fromGroup !== groupID ? el('span', { class: 'library-limited', text: `из группы «${groupsByID.get(m.fromGroup)?.name || '?'}»` }) : null);
        }));
        renderPrimary();
      };
      const results = el('div', { class: 'library-suggest', hidden: true });
      let searchTimer;
      const search = input('', 'Добавить автора', v => {
        clearTimeout(searchTimer);
        searchTimer = setTimeout(async () => {
          if (!v.trim()) { results.hidden = true; return; }
          try {
            const found = (await api().LibraryIdentities(v)).filter(r => !members.some(m => m.site === r.site && m.author === r.author));
            results.replaceChildren(...found.slice(0, 12).map(r => {
              const foreign = r.group && r.group !== groupID && !absorb.includes(r.group);
              return button(`${siteLabel(r.site)} / ${r.author} · ${r.files}${r.group_name ? ` · группа «${r.group_name}»` : ''}`, () => {
                add(r, r.group || ''); results.hidden = true; search.value = ''; renderMembers();
              }, { class: 'library-suggest-item', disabled: !!foreign, title: foreign ? 'Автор уже в другой группе' : '' });
            }));
            if (!found.length) results.replaceChildren(el('span', { class: 'muted', text: 'Не найдено' }));
            results.hidden = false;
          } catch (e) { setStatus(String(e?.message || e), true); }
        }, 200);
      }, 'search', { placeholder: 'ник или сайт/ник' });
      renderMembers();
      body.append(field('Название и папка группы', input(name, 'Название группы', v => name = v)),
        el('div', { class: 'library-member-block' }, el('span', { class: 'library-label', text: 'Авторы' }), list),
        field('Добавить автора', search), results, primaryBox,
        check('Сразу убрать одинаковые работы (точные копии и совпадающие пиксели)', dedupe, v => dedupe = v),
        el('p', { class: 'muted library-hint', text: `Оставляется ${prefs.policy === 'source' && prefs.preferred ? `версия из «${siteLabel(prefs.preferred)}»` : 'версия с лучшим разрешением'} (вкладка «Настройки»). Похожие, но не одинаковые файлы не удаляются.` }),
        el('div', { class: 'viewer-actions' }, button('Отмена', close), button('Предпросмотр', async () => {
          const chosen = members.filter(m => m.checked).map(({ site, author }) => ({ site, author }));
          if (chosen.length < 2) throw new Error('Выберите минимум двух авторов');
          if (!name.trim()) throw new Error('Укажите название группы');
          close();
          await preview({ kind: 'authors', authors: { group_id: groupID, name: name.trim(), members: chosen, absorb, dedupe, policy: prefs.policy, preferred: prefs.preferred, primary_site: primary } });
        }, { class: 'pv-btn', 'data-mutation': true })));
    });
  }

  // ----- operations -----
  function operationCard(plan) {
    const { moved, removed, created } = planCounts(plan);
    const facts = [STATUSES[plan.state] || plan.state, new Date(plan.created * 1000).toLocaleString('ru-RU')];
    if (moved) facts.push(`перемещено ${moved}`);
    if (removed) facts.push(`в карантин ${removed}`);
    if (created) facts.push(`создано ${created}`);
    if (plan.restore_files?.length) facts.push(`файлов ${plan.restore_files.length}`);
    const node = el('article', { class: 'library-operation' }, el('strong', { text: operationTitle(plan) }), el('span', { text: facts.join(' · ') }),
      plan.error ? el('span', { class: 'library-limited', text: plan.error }) : null);
    if ((plan.changes || []).length) {
      const details = el('details', {}, el('summary', { text: 'Файлы' }));
      details.addEventListener('toggle', () => {
        if (!details.open || details.querySelector('.library-plan-list')) return;
        const list = el('div', { class: 'library-plan-list' });
        for (const c of plan.changes.slice(0, 200)) list.append(el('div', { class: 'library-plan-item' }, el('span', { text: c.before.path }), el('span', { text: '→ ' + (c.after.state === 'quarantined' ? 'Карантин' : c.after.path) })));
        if (plan.changes.length > 200) list.append(el('p', { class: 'muted', text: `И ещё ${plan.changes.length - 200}.` }));
        details.append(list);
      });
      node.append(details);
    }
    const actions = el('div', { class: 'viewer-actions' });
    if (plan.state === 'done' && plan.kind !== 'restore') actions.append(button('Отменить операцию', () => restore(plan), { 'data-mutation': true }));
    if (['done', 'purging'].includes(plan.state) && (plan.steps || []).some(s => s.to.replaceAll('\\', '/').includes('/.massdownloader-trash/'))) actions.append(button('Очистить карантин', () => purge(plan), { 'data-mutation': true }));
    if (actions.children.length) node.append(actions);
    return node;
  }
  function restore(plan) {
    modal('Отмена операции', (body, close) => {
      body.append(el('p', { text: `Отменить «${operationTitle(plan)}»? Файлы вернутся на прежние места вместе со связями с постами и группами. Если файлы уже изменены другими операциями, отмена не выполнится.` }),
        el('div', { class: 'viewer-actions' }, button('Нет', close), button('Отменить операцию', async () => { await begin(() => api().LibraryRestore(plan.id)); close(); }, { class: 'pv-btn' })));
    });
  }
  function purge(plan) {
    modal('Очистка карантина', (body, close) => {
      let accepted = false;
      const apply = button('Удалить окончательно', async () => { if (!accepted) return; await begin(() => plan ? api().LibraryPurge(plan.id) : api().LibraryPurgeAll()); close(); }, { class: 'pv-btn danger', disabled: true });
      body.append(el('p', { text: plan ? 'Резервные файлы этой операции будут удалены, отменить её станет невозможно. Активные изображения и избранное сохранятся.' : 'Будут окончательно удалены резервные файлы всех завершённых операций. Отменить эти операции станет невозможно.' }),
        check('Подтверждаю окончательное удаление', false, on => { accepted = on; apply.disabled = !on; }), el('div', { class: 'viewer-actions' }, button('Отмена', close), apply));
    });
  }
  async function loadOperations(request) {
    const [plans, value] = await Promise.all([api().LibraryOperations(), api().LibraryInfo()]);
    if (ended || request !== revision) return;
    tools.replaceChildren(el('div', { class: 'library-selection-tools' }, el('span', { class: 'library-selection', text: `Последние операции: ${plans.length}` }),
      value.quarantine ? button(`Очистить весь карантин · ${bytes(value.quarantine_bytes)}`, () => purge(null), { 'data-mutation': true }) : null));
    content.replaceChildren(...(plans.length ? plans.map(operationCard) : [el('div', { class: 'empty', text: 'Операций пока нет' })]));
  }

  // ----- settings -----
  async function loadSettings(request) {
    overview = await api().LibraryAuthors();
    if (ended || request !== revision) return;
    const layouts = el('div', { class: 'library-layouts', role: 'radiogroup', 'aria-label': 'Структура папок групп' });
    for (const l of LAYOUTS) {
      const active = overview.layout === l.value;
      layouts.append(el('button', {
        type: 'button', class: 'library-layout' + (active ? ' active' : ''), role: 'radio', 'aria-checked': String(active), 'data-mutation': true, dataset: { layout: l.value },
        onclick: async () => { if (active || busy) return; try { await preview({ kind: 'layout', authors: { layout: l.value } }); } catch (e) { setStatus(String(e?.message || e), true); } },
      }, el('strong', { text: l.label }), el('pre', { text: l.tree.join('\n') }), el('span', { class: 'muted', text: l.hint })));
    }
    content.replaceChildren(
      el('section', { class: 'library-settings' }, el('h3', { text: 'Структура папок объединённых авторов' }),
        el('p', { class: 'muted library-hint', text: `Групп: ${overview.groups.length}. При смене структуры файлы всех групп переносятся автоматически (с предпросмотром и возможностью отмены), новые загрузки сразу идут по новому пути.` }), layouts),
      el('section', { class: 'library-settings' }, el('h3', { text: 'Какую версию оставлять' }),
        el('div', { class: 'library-options' },
          field('Приоритет', selectEl([{ value: 'quality', label: 'Лучшее разрешение' }, { value: 'source', label: 'Предпочтительный источник' }], prefs.policy, v => savePrefs({ policy: v }), { 'aria-label': 'Приоритет версии' })),
          field('Предпочтительный источник', selectEl([{ value: '', label: 'Без предпочтения' }, ...state.sites.map(s => ({ value: s.key, label: s.label }))], prefs.preferred, v => savePrefs({ preferred: v }), { 'aria-label': 'Источник по умолчанию' }))),
        el('p', { class: 'muted library-hint', text: 'Используется при объединении авторов, авторазборе и как предложение в группах дублей. При равенстве — файл большего размера.' })),
      el('section', { class: 'library-settings' }, el('h3', { text: 'Поиск кандидатов в авторы' }),
        el('div', { class: 'library-options' },
          field('Совпадение работ', selectEl(CANDIDATE_MODES, prefs.candidate_mode, v => savePrefs({ candidate_mode: v }), { 'aria-label': 'Режим кандидатов' })),
          field('Расстояние pHash', numberPref('candidate_threshold', 0, 20, 1, false)), field('Мин. общих работ', numberPref('min_shared', 1, 1000, 1, false)), field('Мин. доля, %', numberPref('min_ratio', 0, 100, 100, false))),
        el('p', { class: 'muted library-hint', text: 'Сайты часто пережимают файлы, поэтому точные копии находят мало. Рабочий режим — «Похожие» с расстоянием 4–8.' })));
  }

  // ----- dispatcher -----
  async function load() {
    const request = ++revision;
    content.dataset.tab = tab; resetImages(); updateActionbar();
    if (tab !== 'files' && tab !== 'duplicates') tools.replaceChildren();
    try {
      if (tab === 'logs') { content.replaceChildren(); await renderLogs(content); return; }
      if (tab === 'files') await loadFiles(request, false);
      else if (tab === 'duplicates') await loadDuplicates(request, false);
      else if (tab === 'authors') await loadAuthors(request);
      else if (tab === 'operations') await loadOperations(request);
      else if (tab === 'settings') await loadSettings(request);
      if (request === revision) { content.scrollTop = 0; updateBusy(); }
    } catch (e) { if (request === revision && !ended) setStatus(String(e?.message || e), true); }
  }
  async function loadMore() {
    if (loadingMore) return;
    const more = tab === 'files' ? files.length < filesTotal : tab === 'duplicates' ? groups.length < groupsTotal : false;
    if (!more) return;
    loadingMore = true;
    try { tab === 'files' ? await loadFiles(revision, true) : await loadDuplicates(revision, true); updateBusy(); }
    catch (e) { setStatus(String(e?.message || e), true); }
    finally { loadingMore = false; }
  }
  content.addEventListener('scroll', () => { if (content.scrollTop + content.clientHeight > content.scrollHeight - 800) loadMore(); }, { passive: true });

  // ----- compare -----
  function compare(list) {
    modal('Сравнение изображений', (body, close, alive) => {
      let left = list[0].id, right = list[1]?.id || left, version = 0, zoom = 100, overlayMode = false;
      const view = el('div', { class: 'library-compare-view' });
      const choices = list.map(f => ({ value: f.id, label: `${siteLabel(f.site)} · ${dims(f)} · ${bytes(f.size)} · ${basename(f.path)}` }));
      let syncing = false;
      view.addEventListener('scroll', e => {
        if (syncing || !e.target.classList?.contains('library-compare-scroll')) return;
        syncing = true;
        for (const node of view.querySelectorAll('.library-compare-scroll')) if (node !== e.target) { node.scrollLeft = e.target.scrollLeft; node.scrollTop = e.target.scrollTop; }
        syncing = false;
      }, true);
      const opacity = input(50, 'Прозрачность наложения', v => view.style.setProperty('--compare-opacity', Number(v) / 100), 'range', { min: '0', max: '100', hidden: true });
      async function paint() {
        const request = ++version;
        view.classList.toggle('overlay', overlayMode);
        view.style.setProperty('--compare-opacity', opacity.value / 100); view.style.setProperty('--compare-zoom', zoom + '%');
        const ids = list.length > 1 ? [left, right] : [left];
        const nodes = ids.map(id => { const f = list.find(x => x.id === id), img = el('img', { alt: basename(f.path) }); return { id, img, node: el('figure', {}, el('figcaption', { text: `${dims(f)} · ${bytes(f.size)} · ${f.path}` }), el('div', { class: 'library-compare-scroll' }, img)) }; });
        view.replaceChildren(...nodes.map(n => n.node));
        for (const { id, img } of nodes) { try { const url = await api().LibraryFileURL(id); if (alive() && request === version && url) img.src = url; } catch (e) { if (alive()) setStatus(String(e), true); } }
      }
      const zoomInput = input(100, 'Общий масштаб', v => { zoom = Number(v); view.style.setProperty('--compare-zoom', zoom + '%'); }, 'range', { min: '100', max: '400' });
      body.append(el('div', { class: 'library-options' },
        field('Слева', selectEl(choices, left, v => { left = v; paint(); }, { 'aria-label': 'Левое изображение' })),
        list.length > 1 ? field('Справа', selectEl(choices, right, v => { right = v; paint(); }, { 'aria-label': 'Правое изображение' })) : null,
        list.length > 1 ? check('Наложение', false, v => { overlayMode = v; opacity.hidden = !v; paint(); }) : null, field('Масштаб', zoomInput), opacity), view);
      paint();
    }, { wide: true });
  }

  unbind.push(window.runtime?.EventsOn('library:progress', taskStatus));
  // Progress polling also covers reopening this window during a running task.
  poll = setInterval(readStatus, 1500); poll.unref?.();
  window.addEventListener('beforeunload', () => { ended = true; clearInterval(poll); clearTimeout(filterTimer); observer?.disconnect(); unbind.forEach(fn => fn?.()); });
  renderRoots();
  await Promise.all([info(), readStatus()]);
  await load();
}
