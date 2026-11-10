// MassDownloader frontend — dialogs: auth, cache, reverse search, presets.
// Redesigned to the graphite/orange theme (custom buttons, no Material).
import { state, el, checkEl, selectEl, selectSite, renderQuery, refreshHistCount, setCheck, syncManualToFilters } from './main.js';
import { icon } from './icons.js';
import { dismissOverlay, animateIn, applyMotion, revealOnce, forgetReveal } from './motion.js';
import { encodeBytes, initialFilters, mediaURL } from './contracts.js';
import { reversePreview } from './reverse-preview.js';
import { setStatus as showStatus } from './main.js';

// -- generic modal -----------------------------------------------------------------
function modal(title, bodyBuilder, onClose = () => {}) {
  let resolve;const result=new Promise(done=>{resolve=done;});
  const overlay = el('div', { class: 'viewer-overlay dialog-overlay' });
  const box = el('div', { class: 'dialog-box' });
  const close = el('button', { class: 'viewer-close', text: '✕', type: 'button' });
  const previousFocus = document.activeElement;
  let closed = false;
  const finish=(value=false)=>{if(closed)return;closed=true;onClose();dismissOverlay(overlay);if(previousFocus?.isConnected)previousFocus.focus();resolve(value);};
  close.addEventListener('click', () => finish());
  overlay.addEventListener('click', e => {
    if (e.target === overlay) finish();
  });
  overlay.addEventListener('keydown', e => { if (e.key === 'Escape') finish(); });
  box.append(close, el('h2', { text: title }));
  Promise.resolve(bodyBuilder(box, finish)).catch(e => showStatus(String(e), true));
  overlay.tabIndex = -1;
  overlay.append(box);
  document.body.appendChild(overlay);
  overlay.focus();
  return result;
}

// shared button styles
const bPrimary = (text, onclick) => {
  const b = el('button', { class: 'pv-btn', text, type: 'button' });
  b.addEventListener('click', onclick);
  return b;
};
const bGhost = (text, onclick) => {
  const b = el('button', { class: 'pv-btn ghost', text, type: 'button' });
  b.addEventListener('click', onclick);
  return b;
};

export function confirmDownload(details, signal) {
  if (signal?.aborted) return Promise.resolve(false);
  let abort;
  return modal('Начать скачивание?', (box, close) => {
    box.classList.add('download-confirmation');
    box.setAttribute('role', 'dialog'); box.setAttribute('aria-modal', 'true');
    box.setAttribute('aria-label', 'Подтверждение скачивания');
    abort = () => close(false);
    signal?.addEventListener('abort', abort, { once: true });
    box.append(el('p', { text: details }), el('div', { class: 'viewer-actions' },
      bGhost('Отмена', () => close(false)), bPrimary('Скачать', () => close(true))));
  }, () => signal?.removeEventListener('abort', abort));
}

// Themed replacements for window.confirm / window.prompt (WebView2 renders
// the native ones as browser chrome).
export function confirmAction(title, text, okLabel = 'OK', { danger = false } = {}) {
  return modal(title, (box, close) => {
    box.setAttribute('role', 'alertdialog'); box.setAttribute('aria-modal', 'true'); box.setAttribute('aria-label', title);
    box.classList.add('confirm-box');
    const ok = bPrimary(okLabel, () => close(true));
    if (danger) ok.classList.add('danger');
    box.append(el('p', { text }), el('div', { class: 'viewer-actions dialog-actions' }, bGhost('Отмена', () => close(false)), ok));
    queueMicrotask(() => ok.focus());
  });
}
export function promptText(title, label, value = '', okLabel = 'Сохранить') {
  return modal(title, (box, close) => {
    box.setAttribute('role', 'dialog'); box.setAttribute('aria-modal', 'true'); box.setAttribute('aria-label', title);
    box.classList.add('confirm-box');
    const input = el('input', { class: 'field', type: 'text', value, spellcheck: 'false' });
    const submit = () => { const v = input.value.trim(); if (v) close(v); };
    input.addEventListener('keydown', e => { if (e.key === 'Enter') { e.preventDefault(); submit(); } });
    box.append(el('label', { class: 'dlg-field' }, el('span', { text: label }), input),
      el('div', { class: 'viewer-actions dialog-actions' }, bGhost('Отмена', () => close(null)), bPrimary(okLabel, submit)));
    queueMicrotask(() => { input.focus(); input.select?.(); });
  }).then(v => (typeof v === 'string' ? v : null));
}

const fieldInput = (labelText, value = '', type = 'text') => {
  const inp = el('input', { class: 'field', type, value, spellcheck: 'false' });
  return [el('label', { class: 'dlg-field' }, el('span', { text: labelText }), inp), inp];
};

// -- auth dialog ---------------------------------------------------------------------
export function openAuthDialog(requestedSite) {
  const site = requestedSite?.key ? requestedSite : state.currentSite;
  if (!site) return Promise.resolve(false);
  return modal(`Авторизация — ${site.label}`, async (box, close) => {
    const authAddress = String(site.auth_url || '').trim();
    if (authAddress) {
      let isURL = false;
      try { isURL = /^https?:$/.test(new URL(authAddress).protocol) && !/\s/.test(authAddress); } catch {}
      box.append(isURL
        ? bGhost('Открыть страницу входа / API',()=>window.runtime?.BrowserOpenURL(authAddress))
        : el('p', { class:'auth-instructions muted', text:authAddress }));
    }

    let creds = {};
    try { creds = (await window.go.main.App.GetCredentials()) ?? {}; } catch {}
    const mine = creds[site.key] ?? {};

    const fields = {};
    for (const fld of site.auth_fields ?? []) {
      const [lbl, input] = fieldInput(fld.label, mine[fld.key] ?? '', /key|token|cookie|pass|sess|ct0/i.test(fld.key)?'password':'text');
      fields[fld.key] = input;
      box.append(lbl);
    }
    if (site.cf_protected) {
      box.append(el('div', { class: 'muted', style: 'font-size:12px',
        text: 'Cloudflare: при 403 вставьте cf_clearance и User-Agent из браузера.' }));
      for (const fld of [
        { key: 'cf_clearance', label: 'cf_clearance (cookie)' },
        { key: 'user_agent', label: 'User-Agent браузера' }]) {
        const [lbl, input] = fieldInput(fld.label, mine[fld.key] ?? '', fld.key === 'cf_clearance' ? 'password' : 'text');
        fields[fld.key] = input;
        box.append(lbl);
      }
      for (const host of site.cf_extra_domains ?? []) {
        box.append(el('div', { class: 'muted', style: 'font-size:12px',
          text: `${host} — отдельные данные CDN, если браузер использует их; пустые поля наследуют значения сайта.` }));
        for (const fld of [
          { key: `cf_clearance@${host}`, label: `cf_clearance — ${host}` },
          { key: `user_agent@${host}`, label: `User-Agent — ${host}` }]) {
          const [lbl, input] = fieldInput(fld.label, mine[fld.key] ?? '', fld.key.startsWith('cf_clearance') ? 'password' : 'text');
          fields[fld.key] = input;
          box.append(lbl);
        }
      }
    }
    const save = bPrimary('Сохранить', async () => {
      const vals = {};
      for (const [k, inp] of Object.entries(fields)) vals[k] = inp.value.trim();
      if(site.requires_auth&&(site.auth_fields||[]).some(f=>!vals[f.key])) {showStatus('Заполните обязательные поля авторизации',true);return;}
      creds[site.key] = { ...mine, ...vals };
      try {
        await window.go.main.App.SetCredentials(creds);
        showStatus('авторизация сохранена');
        close(true);
      } catch (e) { showStatus(String(e), true); }
    });
    box.append(el('div', { class: 'viewer-actions' }, save,
      bGhost('Отмена', () => close())));
  });
}
export async function ensureAuth(siteKey) {
 const site=state.sites.find(s=>s.key===siteKey);if(!site?.requires_auth)return true;
 const creds=(await window.go.main.App.GetCredentials())?.[siteKey]||{};
 if((site.auth_fields||[]).every(f=>creds[f.key]))return true;
 return await openAuthDialog(site);
}

// -- cache management dialog -----------------------------------------------------------
export function openCacheDialog() { return openSettingsDialog('cache'); }

async function mountCacheSettings(box, isCurrent) {
  const api = window.go.main.App;
  let stats, cacheVideos;
  try { [stats, cacheVideos] = await Promise.all([api.CacheStats(), api.GetSetting('cache_videos')]); stats ??= {}; cacheVideos = cacheVideos === true; }
  catch (err) { showStatus(String(err), true); return; }
  if (!isCurrent()) return;
  const mb = b => ((b || 0) / 1048576).toFixed(1) + ' МБ';
  let timer, closed = false, refreshing = false;
  {
    const thumbInfo = el('div', { class: 'mono muted' });
    const videoInfo = el('div', { class: 'mono muted' });
    const cacheToggle = checkEl(cacheVideos, async on => {
      cacheToggle.style.pointerEvents = 'none';
      try {
        await api.SetSetting('cache_videos', on);
        cacheVideos = on;
      } catch (err) {
        setCheck(cacheToggle, cacheVideos);
        showStatus(String(err), true);
      } finally { cacheToggle.style.pointerEvents = ''; }
    });
    const buttons = [];
    const run = async (fn, message) => {
      buttons.forEach(b => { b.disabled = true; });
      try { await fn(); stats = await api.CacheStats(); render(); if (message) showStatus(message); }
      catch (err) { showStatus(String(err), true); }
      finally { buttons.forEach(b => { b.disabled = false; }); }
    };
    const thumbClear = bGhost('Очистить миниатюры', () => run(() => api.ClearThumbs(), 'Миниатюры очищены'));
    const videoClear = bGhost('Очистить видео', () => run(() => api.ClearVideoTmp(), 'Очистка видео завершена'));
    const render = () => {
      thumbInfo.textContent = `${mb(stats.thumb_bytes)} · ${stats.thumb_count ?? 0} файлов`;
      videoInfo.textContent = `${mb(stats.video_tmp_bytes)} · ${stats.video_tmp_count ?? 0} файлов` +
        (stats.video_loading ? ` · загружается: ${stats.video_loading}` : '');
    };
    render();
    const row = (title, info, button) => el('div', { class: 'cache-row' },
      el('div', {}, el('strong', { text: title }), info), button);
    box.append(row('Миниатюры и превью', thumbInfo, thumbClear), row('Видео', videoInfo, videoClear),
      el('label', { class: 'toggle' }, cacheToggle, 'Кэшировать видео'),
      el('p', { class: 'muted', text: 'При включении видео загружается целиком перед воспроизведением и сохраняется в кеш. При выключении воспроизводится напрямую по HTTP. Самые старые файлы кеша удаляются при превышении лимита.' }));
    const [thumbLabel, thumbLimit] = fieldInput('Лимит кеша миниатюр (МБ)', String(stats.limit_mb || 512), 'number');
    const [videoLabel, videoLimit] = fieldInput('Лимит кеша видео (МБ)', String(stats.video_limit_mb || 2048), 'number');
    for (const input of [thumbLimit, videoLimit]) { input.min = '64'; input.max = '65536'; input.step = '1'; }
    box.append(thumbLabel, videoLabel);
    const normalize = input => {
      const value = Number(input.value);
      if (!Number.isFinite(value) || value < 64 || value > 65536 || !Number.isInteger(value)) throw new Error('Лимит должен быть целым числом от 64 до 65536 МБ');
      return value;
    };
    const all = bGhost('Очистить всё', () => run(async () => { await api.ClearThumbs(); await api.ClearVideoTmp(); }, 'Очистка кеша завершена'));
    const apply = bPrimary('Применить лимиты', () => run(async () => {
      const thumb = normalize(thumbLimit), video = normalize(videoLimit);
      await api.SetSetting('thumb_cache_mb', thumb);
      await api.SetSetting('video_cache_mb', video);
    }, 'Лимиты кеша применены'));
    buttons.push(thumbClear, videoClear, all, apply);
    box.append(el('div', { class: 'viewer-actions wrap' }, all, apply));
    const histInfo = el('div', { class: 'mono muted' });
    const renderHistory = async () => { try { histInfo.textContent = `${Number(await api.HistoryCount()).toLocaleString('ru-RU')} записей`; } catch {} };
    const histReset = bGhost('Сбросить историю', async () => {
      const ok = await confirmAction('Сбросить историю?', 'Приложение забудет, какие файлы уже скачаны: «Пропускать скачанное» и сверка дублей с базой начнут заново. Сами файлы на диске не удаляются.', 'Сбросить', { danger: true });
      if (!ok) return;
      try { await api.ResetHistory(); await refreshHistCount(); await renderHistory(); showStatus('история очищена'); }
      catch (err) { showStatus(String(err), true); }
    });
    histReset.classList.add('danger-ghost');
    box.append(el('hr', { class: 'divider' }), el('div', { class: 'history-row' },
      el('div', {}, el('strong', { text: 'История скачиваний' }), histInfo), histReset));
    renderHistory();
    timer = setInterval(async () => {
      if (closed || refreshing) return;
      refreshing = true;
      try { const next = await api.CacheStats(); if (!closed) { stats = next; render(); } }
      catch {} finally { refreshing = false; }
    }, 1500);
  }
  return () => { closed = true; clearInterval(timer); };
}

// -- reverse image search dialog ---------------------------------------------------------
export function openReverseDialog() {
  let closed = false, pasteHandler, dispose = () => {};
  return modal('Поиск по изображению', async (box, close) => {
    const api = window.go.main.App;
    const [savedKey, savedSimilarity] = await Promise.all([api.GetSetting('saucenao_api_key'), api.GetSetting('reverse_min_similarity')]);
    if (closed) return;
    box.classList.add('reverse-search-box');
    box.setAttribute('role', 'dialog'); box.setAttribute('aria-modal', 'true'); box.setAttribute('aria-label', 'Поиск по изображению');
    const [keyLabel, key] = fieldInput('SauceNAO API key', savedKey || '', 'password');
    const [simLabel, similarity] = fieldInput('Минимальное сходство, %', String(savedSimilarity ?? 55), 'number');
    similarity.min = '0'; similarity.max = '100';
    const fileInput = el('input', { type: 'file', accept: 'image/*', style: 'display:none' });
    const pickBtn = bGhost('Выбрать файл…', () => fileInput.click());
    const fileName = el('span', { class: 'muted rev-file-name', text: 'файл не выбран' });
    const imageInfo = el('span', { class: 'muted rev-image-info' });
    const sourcePreview = el('div', { class: 'rev-source-preview' }, reversePreview('', { emptyText: 'Выберите файл или вставьте изображение через Ctrl+V' }));
    let sourceURL = '';
    const resultCards = new Set();
    function clearResults() {
      for (const card of resultCards) { forgetReveal(card); card.querySelector('.rev-image-frame')?.disposePreview(); }
      resultCards.clear(); results.replaceChildren();
    }
    dispose = () => {
      clearResults(); sourcePreview.firstElementChild?.disposePreview();
      if (sourceURL) URL.revokeObjectURL(sourceURL);
    };
    const providersBox = el('div', { class: 'rev-providers', role: 'group', 'aria-label': 'Движки поиска' });
    const results = el('div', { class: 'rev-results' });
    const status = el('p', { class: 'muted rev-search-status', role: 'status', 'aria-live': 'polite' });
    let file = null, busy = false, providersBusy = false, providers = [], providerRequest = 0;
    const selection = new Map();
    const searchBtn = bPrimary('Поиск', run);
    searchBtn.classList.add('rev-search-button');
    const saveBtn = bGhost('Сохранить настройки поиска', async () => {
      if (busy || providersBusy) return;
      providersBusy = true; syncControls();
      try { await saveSettings(); await loadProviders(); if (!closed) report('Настройки поиска сохранены'); }
      catch (e) { if (!closed) report(String(e), true); }
      finally { if (!closed) { providersBusy = false; syncControls(); } }
    });
    function report(text, error = false) { status.textContent = text; status.classList.toggle('error', error); }
    function syncControls() {
      const locked = busy || providersBusy;
      searchBtn.disabled = locked; saveBtn.disabled = locked; pickBtn.disabled = locked; fileInput.disabled = locked;
      key.disabled = locked; similarity.disabled = locked;
      for (const checkbox of providersBox.querySelectorAll('input')) checkbox.disabled = locked || checkbox.dataset.available !== 'true';
      searchBtn.replaceChildren(...(busy ? [el('span', { class: 'viewer-spinner', 'aria-hidden': 'true' })] : []), document.createTextNode(busy ? 'Поиск…' : 'Поиск'));
      searchBtn.setAttribute('aria-busy', String(busy));
    }
    async function saveSettings() {
      const value = Number(similarity.value);
      if (!similarity.value.trim() || !Number.isFinite(value) || value < 0 || value > 100) throw new Error('Сходство должно быть числом от 0 до 100');
      await api.SetSetting('saucenao_api_key', key.value.trim());
      await api.SetSetting('reverse_min_similarity', value);
    }
    function chooseImage(image, pasted = false) {
      if (closed || busy || providersBusy || !image) return;
      if (!image.type?.startsWith('image/') && !/\.(png|jpe?g|gif|webp|bmp|avif)$/i.test(image.name || '')) { report('Выберите файл изображения', true); return; }
      file = image;
      fileName.textContent = pasted ? `Из буфера обмена: ${image.name || 'image.png'}` : image.name;
      sourcePreview.firstElementChild?.disposePreview();
      if (sourceURL) URL.revokeObjectURL(sourceURL);
      sourceURL = URL.createObjectURL(image);
      imageInfo.textContent = image.size ? `${(image.size / 1024).toLocaleString('ru-RU', { maximumFractionDigits: 1 })} КБ` : '';
      sourcePreview.replaceChildren(reversePreview(sourceURL, { alt: 'Изображение для поиска', onReady: img => {
        imageInfo.textContent = `${img.naturalWidth} × ${img.naturalHeight}${image.size ? ` · ${(image.size / 1024).toLocaleString('ru-RU', { maximumFractionDigits: 1 })} КБ` : ''}`;
      } }));
      clearResults(); report(pasted ? 'Изображение вставлено. Выберите движки и нажмите «Поиск».' : 'Выберите движки и нажмите «Поиск».');
    }
    fileInput.addEventListener('change', () => chooseImage(fileInput.files?.[0]));
    pasteHandler = event => {
      const overlays = document.querySelectorAll('.dialog-overlay');
      if (closed || overlays[overlays.length - 1] !== box.parentElement) return;
      const data = event.clipboardData;
      const item = [...(data?.items || [])].find(item => item.kind === 'file' && item.type.startsWith('image/'));
      const image = item?.getAsFile() || [...(data?.files || [])].find(file => file.type.startsWith('image/'));
      if (!image) return; // Keep ordinary text paste in the API-key field.
      event.preventDefault(); chooseImage(image, true);
    };
    document.addEventListener('paste', pasteHandler);
    async function loadProviders() {
      const request = ++providerRequest;
      providersBusy = true; syncControls();
      try {
        const available = await api.ReverseProviders() ?? [];
        if (closed || request !== providerRequest) return;
        providers = available; providersBox.replaceChildren();
        for (const p of providers) {
          if (p.ok && !selection.has(p.key)) selection.set(p.key, true);
          const checkbox = el('input', { type: 'checkbox', 'data-provider': p.key, 'data-available': String(!!p.ok) });
          checkbox.checked = !!p.ok && !!selection.get(p.key);
          checkbox.addEventListener('change', () => selection.set(p.key, checkbox.checked));
          providersBox.append(el('label', { class: 'rev-provider-choice', title: p.blurb || '' }, checkbox,
            el('span', {}, el('span', { text: p.label + (p.ok ? '' : ' — недоступен') }),
              !p.ok && p.why ? el('span', { class: 'muted rev-provider-reason', text: p.why }) : null)));
        }
      } finally {
        if (!closed && request === providerRequest) { providersBusy = false; syncControls(); }
      }
    }
    function appendMatches(target, matches) {
        if (!matches?.length) { target.append(el('p', { class: 'muted rev-empty-result', text: 'Совпадений нет.' })); return; }
        for (const m of matches) {
          const thumb = reversePreview(m.thumb_url ? mediaURL(m.thumb_url, 'jpg') : '', { alt: `Превью результата — ${m.source}`, lazy: true });
          const details = el('div', { class: 'rev-match-details' },
            el('div', { class: 'rev-match-heading' }, el('span', { class: 'rev-sim', text: `${m.similarity}%` }), el('strong', { text: m.source })),
            m.author ? el('p', { class: 'rev-match-author muted', text: m.author }) : null,
            m.note ? el('p', { class: 'rev-note muted', text: m.note }) : null);
          const row = el('div', { class: 'rev-row' },
            thumb, details);
          const actions = el('div', { class: 'rev-actions' });
          if (m.in_app) {
            actions.append(bGhost('открыть', async () => {
              const post = await api.PostByID(m.site_key, m.post_id);
              if (closed) return;
              if (post) {
                close();
                window.dispatchEvent(new CustomEvent('open-post', { detail: post }));
              } else showStatus('пост не найден на сайте', true);
            }));
          }
          actions.append(bGhost('в браузере', () =>
            m.url && window.runtime?.BrowserOpenURL(m.url)));
          details.append(actions); target.append(row); resultCards.add(row); revealOnce(row);
        }
    }
    async function run() {
      if (busy || providersBusy || closed) return;
      if (!file) { report('Выберите файл изображения или вставьте его через Ctrl+V', true); return; }
      const selected = providers.filter(p => p.ok && selection.get(p.key));
      if (!selected.length) { report('Выберите хотя бы один движок поиска', true); return; }
      const image = file;
      busy = true; syncControls(); clearResults(); results.setAttribute('aria-busy', 'true'); report('Подготовка изображения…');
      try {
        await saveSettings();
        const arr = encodeBytes(new Uint8Array(await image.arrayBuffer()));
        if (closed) return;
        let completed = 0, found = 0, failures = 0;
        const tasks = selected.map(provider => {
          const matchesBox = el('div', { class: 'rev-provider-matches' }, el('div', { class: 'rev-search-placeholder', role: 'status' },
            el('div', { class: 'rev-skeleton-thumb', 'aria-hidden': 'true' }),
            el('div', { class: 'rev-skeleton-details' }, el('div', { class: 'rev-provider-loading' }, el('span', { class: 'viewer-spinner', 'aria-hidden': 'true' }), 'Поиск…'),
              el('span', { class: 'rev-skeleton-line', 'aria-hidden': 'true' }), el('span', { class: 'rev-skeleton-line short', 'aria-hidden': 'true' }))));
          const section = el('section', { class: 'rev-provider-result' }, el('h3', { text: provider.label }), matchesBox);
          results.append(section); animateIn(section, { duration: 160 });
          return { provider, matchesBox, section };
        });
        report(`Поиск: 0/${selected.length} движков`);
        await Promise.allSettled(tasks.map(async ({ provider, matchesBox, section }) => {
          try {
            const matches = await api.ReverseSearch(provider.key, image.name || 'image.png', arr);
            if (closed) return;
            section.dataset.state = matches?.length ? 'matches' : 'empty';
            found += matches?.length || 0; matchesBox.replaceChildren(); appendMatches(matchesBox, matches); animateIn(matchesBox, { duration: 160 });
          } catch (e) {
            if (closed) return;
            failures++; matchesBox.replaceChildren(el('p', { class: 'error', text: String(e) }));
          } finally { if (!closed) { completed++; report(`Поиск: ${completed}/${selected.length} движков · совпадений: ${found}${failures ? ` · ошибок: ${failures}` : ''}`); } }
        }));
        if (!closed) report(`Поиск завершён · совпадений: ${found}${failures ? ` · ошибок: ${failures}` : ''}`, failures === selected.length);
      } catch (e) { if (!closed) report(String(e), true); }
      finally { if (!closed) { busy = false; results.setAttribute('aria-busy', 'false'); syncControls(); } }
    }
    const source = el('div', { class: 'rev-source' }, el('h3', { class: 'rev-section-title', text: 'Изображение для поиска' }), sourcePreview,
      fileName, imageInfo, el('div', { class: 'dirrow rev-file-picker' }, pickBtn), fileInput,
      el('p', { class: 'muted rev-paste-hint', text: 'Ctrl+V — вставить изображение из буфера обмена.' }));
    const controls = el('div', { class: 'rev-controls' }, keyLabel, simLabel, saveBtn,
      el('h3', { class: 'rev-section-title', text: 'Движки поиска' }), providersBox);
    const body = el('div', { class: 'rev-dialog-body' }, el('div', { class: 'rev-setup' }, source, controls), results);
    box.append(body, el('div', { class: 'rev-search-bar' }, searchBtn, status));
    try { await loadProviders(); } catch (e) { if (!closed) report(String(e), true); }
  }, () => { closed = true; dispose(); document.removeEventListener('paste', pasteHandler); });
}

// -- search presets ------------------------------------------------------------------------
export async function renderPresets(mount) {
  let presets = [];
  try { presets = (await window.go.main.App.GetSetting('search_presets')) ?? []; } catch {}
  const button = el('button', { id: 'preset-button', class: 'pv-btn ghost preset-button', type: 'button', 'aria-haspopup': 'menu', 'aria-expanded': 'false',
    title: 'Сохранённые поиски' }, icon('bookmark', 14), el('span', { text: 'Пресеты' }), presets.length ? el('span', { class: 'tnum', text: String(presets.length) }) : null);
  mount.replaceChildren(button);
  const close = () => {
    mount.querySelector('.preset-menu')?.remove(); button.setAttribute('aria-expanded', 'false');
    document.removeEventListener('mousedown', outside, true);
  };
  const outside = e => { if (!mount.contains(e.target)) close(); };
  const save = async () => {
    close();
    const site = state.sites.find(x => x.key === state.currentSite?.key);
    const name = await promptText('Сохранить поиск', `Название (${site?.nick || ''}: ${state.query || 'все посты'})`);
    if (!name) return;
    const next = [...presets.filter(p => p.name !== name), { name, site: state.currentSite?.key, query: state.query, filters: structuredClone(state.filtersState) }];
    await window.go.main.App.SetSetting('search_presets', next);
    showStatus(`пресет «${name}» сохранён`);
    renderPresets(mount);
  };
  const remove = async idx => {
    const p = presets[idx];
    if (!p || !await confirmAction('Удалить пресет?', `«${p.name}» будет удалён.`, 'Удалить', { danger: true })) return;
    await window.go.main.App.SetSetting('search_presets', presets.filter((_, j) => j !== idx));
    renderPresets(mount);
  };
  button.addEventListener('click', () => {
    if (mount.querySelector('.preset-menu')) return close();
    const menu = el('div', { class: 'preset-menu', role: 'menu' });
    const items = [];
    presets.forEach((p, i) => {
      const site = state.sites.find(x => x.key === p.site);
      const apply = el('button', { type: 'button', role: 'menuitem', class: 'preset-item', title: p.query || '',
        onclick: async () => { close(); await applyPreset(presets, String(i)); } },
        el('span', { class: 'preset-name', text: p.name || p.query || '—' }),
        el('span', { class: 'preset-site', text: site?.nick || p.site || '' }),
        el('span', { class: 'preset-query mono', text: p.query || 'все посты' }));
      const del = el('button', { type: 'button', class: 'preset-del', title: 'Удалить пресет', 'aria-label': `Удалить пресет ${p.name}`,
        onclick: e => { e.stopPropagation(); close(); remove(i); } }, icon('x', 13));
      menu.append(el('div', { class: 'preset-row' }, apply, del)); items.push(apply);
    });
    if (!presets.length) menu.append(el('div', { class: 'preset-empty muted', text: 'Сохранённых поисков нет' }));
    const add = el('button', { type: 'button', role: 'menuitem', class: 'preset-add', onclick: save }, icon('plus', 14), el('span', { text: 'Сохранить текущий поиск…' }));
    items.push(add);
    menu.append(add);
    menu.addEventListener('keydown', e => {
      const at = items.indexOf(document.activeElement);
      if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); close(); button.focus(); }
      else if (e.key === 'ArrowDown' || e.key === 'ArrowUp') { e.preventDefault(); items[(at + (e.key === 'ArrowDown' ? 1 : -1) + items.length) % items.length]?.focus(); }
    });
    mount.append(menu); button.setAttribute('aria-expanded', 'true');
    document.addEventListener('mousedown', outside, true);
    items[0]?.focus();
  });
}

async function applyPreset(presets, idx) {
  if (idx === '') return;
  const p = presets[+idx];
  if (!p) return;
  if (p.site && state.sites.some(s => s.key === p.site)) {
    if (p.filters) Object.assign(state.filtersState, initialFilters(state.sites,p.filters));
    await selectSite(p.site, { carry: false });
    if (p.query !== undefined) { state.manualQuery = p.query; renderQuery(); syncManualToFilters(); if (state.manualSynced) { state.manualQuery = null; state.manualSynced = false; renderQuery(); } }
  }
  showStatus(`пресет «${p.name}» загружен`);
}

// -- top toolbar ------------------------------------------------------------------------------
export function renderTopActions(mount) {
  mount.innerHTML = '';
  const iconBtn = (name, title, onclick) => {
    const b = el('button', { class: 'iconbtn sm', title, 'aria-label': title, type: 'button' }, icon(name, 15));
    b.addEventListener('click', () => onclick());
    return b;
  };
  mount.append(
    iconBtn('key', 'Авторизация сайта', () => openAuthDialog()),
    iconBtn('imageSearch', 'Поиск по изображению', openReverseDialog),
    iconBtn('library', 'Библиотека изображений', async () => { try { await window.go.main.App.OpenLibrary(); } catch (e) { showStatus(String(e),true); } }),
    iconBtn('settings', 'Настройки', () => openSettingsDialog()));
}

async function mountRateLimits(box, isCurrent) {
    const defaults = await window.go.main.App.DefaultRateLimits(), saved = await window.go.main.App.GetSetting('rate_limits') || {};
    if (!isCurrent()) return;
    const fields = {};
    for (const site of state.sites) {
      const [label, input] = fieldInput(`${site.label} — секунд между запросами`, String(saved[site.key] ?? defaults[site.key]), 'number');
      input.min = '0.01'; input.step = '0.05'; fields[site.key] = input; box.append(label);
    }
    const save = bPrimary('Сохранить лимиты', async () => {
      save.disabled = true;
      try {
        const values = {};
        for (const [key, field] of Object.entries(fields)) {
          const n = Number(field.value);
          if (!Number.isFinite(n) || n <= 0) throw new Error('Интервал запросов должен быть больше нуля');
          values[key] = n;
        }
        await window.go.main.App.SetRateLimits(values);
        showStatus('Лимиты запросов применены');
      } catch (error) { showStatus(String(error), true); }
      finally { save.disabled = false; }
    });
    box.append(el('div', { class:'viewer-actions' }, save,
      bGhost('Сбросить', async () => {
        try { await window.go.main.App.SetRateLimits({}); for (const [key, field] of Object.entries(fields)) field.value = String(defaults[key]); showStatus('Лимиты запросов сброшены'); }
        catch (error) { showStatus(String(error), true); }
      })));
}

export function openSettingsDialog(initialSection = 'cache') {
  let cleanup, generation = 0, closed = false;
  return modal('Настройки', (box) => {
    box.classList.add('settings-box');
    box.setAttribute('role','dialog'); box.setAttribute('aria-modal','true'); box.setAttribute('aria-label','Настройки');
    const navigation = el('nav', { class:'settings-nav', role:'tablist', 'aria-label':'Разделы настроек' });
    const content = el('div', { class:'settings-content', role:'tabpanel' });
    const sections = [ ['cache','Кэш',mountCacheSettings], ['downloader','Загрузчик',mountDownloaderSettings], ['appearance','Внешний вид',mountAppearanceSettings], ['limits','Лимиты',mountRateLimits], ['proxy','Прокси',mountProxySettings], ['logs','Логи',mountLogSettings] ];
    const tabs = new Map();
    async function selectSection(key) {
      cleanup?.(); cleanup = null;
      const current = ++generation;
      for (const [id, tab] of tabs) { tab.classList.toggle('active',id === key); tab.setAttribute('aria-selected',String(id === key)); }
      content.setAttribute('aria-label', sections.find(s => s[0] === key)[1]);
      content.replaceChildren(el('p',{class:'muted',text:'Загрузка…'}));
      const isCurrent = () => !closed && current === generation;
      try {
        const panel = el('div', { class:'settings-panel' });
        content.replaceChildren(panel);
        const dispose = await sections.find(s => s[0] === key)[2](panel,isCurrent);
        if (isCurrent()) {
          cleanup = dispose;
          animateIn(panel, { duration: 160 });
        } else dispose?.();
      } catch (error) { if (isCurrent()) content.replaceChildren(el('p',{class:'settings-error',text:String(error)})); }
    }
    for (const [key, title] of sections) {
      const tab = el('button',{type:'button',role:'tab',text:title,onclick:()=>selectSection(key)});
      tabs.set(key,tab); navigation.append(tab);
    }
    box.append(el('div',{class:'settings-layout'},navigation,content));
    selectSection(sections.some(s=>s[0] === initialSection) ? initialSection : 'cache');
  }, () => { closed = true; generation++; cleanup?.(); });
}

export const LOG_LEVELS = [
  { value: 'debug', label: 'Отладка (DEBUG)', hint: 'Всё, включая каждый сетевой запрос, ожидание лимитов и кэш. Для поиска проблем; журнал растёт быстро.' },
  { value: 'info', label: 'Обычный (INFO)', hint: 'Действия, загрузки, паузы из-за лимитов сайтов и ошибки. Рекомендуется.' },
  { value: 'warn', label: 'Предупреждения (WARN)', hint: 'Только предупреждения и ошибки: 429, обрывы, неудачные запросы.' },
  { value: 'error', label: 'Ошибки (ERROR)', hint: 'Только ошибки.' },
];
async function mountLogSettings(box, isCurrent) {
  const api = window.go.main.App, saved = await api.GetAllSettings();
  if (!isCurrent()) return;
  let current = LOG_LEVELS.some(l => l.value === saved.log_level) ? saved.log_level : 'info';
  const error = el('div', { class: 'settings-error', role: 'status', 'aria-live': 'polite' });
  const hint = el('p', { class: 'muted' });
  const showHint = value => { hint.textContent = LOG_LEVELS.find(l => l.value === value)?.hint || ''; };
  const levelSelect = selectEl(LOG_LEVELS.map(({ value, label }) => ({ value, label })), current, async value => {
    levelSelect.disabled = true; error.textContent = ''; error.classList.remove('settings-success');
    try {
      await api.SetSetting('log_level', value);
      (state.settings ??= {}).log_level = value; current = value; showHint(value);
      error.classList.add('settings-success'); error.textContent = 'Уровень логирования применён';
    } catch (failure) {
      levelSelect.value = current; showHint(current); error.textContent = String(failure);
      showStatus(String(failure), true);
    } finally { levelSelect.disabled = false; }
  }, { class: 'field', 'aria-label': 'Уровень логирования' });
  showHint(current);
  box.append(el('label', { class: 'dlg-field' }, el('span', { text: 'Уровень логирования' }), levelSelect), hint,
    el('p', { class: 'muted', text: 'Записи ниже выбранного уровня не попадают в журнал и файл app.log. Применяется сразу, без перезапуска. Фильтр по уровню на странице логов работает отдельно и только скрывает уже записанное.' }),
    error);
}

async function mountAppearanceSettings(box, isCurrent) {
  const api = window.go.main.App, saved = await api.GetAllSettings();
  if (!isCurrent()) return;
  let current = saved.ui_motion || 'full';
  const error = el('div', { class: 'settings-error', role: 'status', 'aria-live': 'polite' });
  const motion = selectEl([
    { value: 'full', label: 'Полные' },
    { value: 'minimal', label: 'Минимальные' },
    { value: 'off', label: 'Выключены' },
  ], current, async value => {
    motion.disabled = true; error.textContent = '';
    try {
      await api.SetSetting('ui_motion', value);
      (state.settings ??= {}).ui_motion = value;
      applyMotion(value); current = value;
    } catch (failure) {
      motion.value = current; error.textContent = String(failure);
      showStatus(String(failure), true);
    } finally { motion.disabled = false; }
  }, { class: 'field', 'aria-label': 'Анимации' });
  box.append(el('label', { class: 'dlg-field' }, el('span', { text: 'Анимации' }), motion),
    el('p', { class: 'muted', text: 'Плавность появления окон, вкладок и карточек. Изменения сохраняются и применяются сразу ко всем окнам.' }), error);
}

async function mountDownloaderSettings(box, isCurrent) {
  const api = window.go.main.App, saved = await api.GetAllSettings();
  if (!isCurrent()) return;
  const [threadsLabel, threads] = fieldInput('Потоки загрузки', String(saved.threads ?? 4), 'number');
  threads.min = '1'; threads.max = '12'; threads.step = '1';
  const values = {
    confirm_download_main: saved.confirm_download_main !== false,
    confirm_download_author: saved.confirm_download_author !== false,
  };
  const error = el('div', { class: 'settings-error', role: 'status', 'aria-live': 'polite' });
  box.append(threadsLabel);
  for (const [key, label] of [
    ['confirm_download_main', 'Подтверждать «Скачать всё» в главном окне'],
    ['confirm_download_author', 'Подтверждать «Скачать всё» в окне автора'],
  ]) box.append(el('label', { class: 'toggle' }, checkEl(values[key], on => { values[key] = on; }), label));
  const save = bPrimary('Сохранить', async () => {
    save.disabled = true; error.textContent = ''; error.classList.remove('settings-success');
    try {
      const count = Number(threads.value);
      if (!Number.isInteger(count) || count < 1 || count > 12) throw new Error('Число потоков должно быть целым от 1 до 12');
      for (const [key, value] of Object.entries({ threads: count, ...values })) {
        await api.SetSetting(key, value);
        (state.settings ??= {})[key] = value;
      }
      window.dispatchEvent(new Event('download-settings-changed'));
      error.classList.add('settings-success'); error.textContent = 'Настройки загрузчика сохранены';
    } catch (failure) { error.textContent = String(failure); }
    finally { save.disabled = false; }
  });
  box.append(el('p', { class: 'muted', text: '«Скачать всё» сначала сканирует посты. Число потоков применяется к следующим загрузкам, включая выбранные посты и работы автора.' }),
    error, el('div', { class: 'viewer-actions' }, save));
}

async function mountProxySettings(box, isCurrent) {
  const api = window.go.main.App;
  const saved = await api.GetProxySettings();
  if (!isCurrent()) return;
  let mode = saved.mode || 'system';
  const error = el('div',{class:'settings-error',role:'status','aria-live':'polite'});
  const modes = el('div',{class:'proxy-modes'});
  const own = el('fieldset',{class:'proxy-fields'},el('legend',{text:'Собственный прокси'}));
  const type = selectEl([{value:'socks5',label:'SOCKS5'},{value:'http',label:'HTTP'}],saved.type || 'socks5',null,{'aria-label':'Тип прокси'});
  const [hostLabel, host] = fieldInput('Адрес',saved.host || ''); host.placeholder = '127.0.0.1';
  const [portLabel, port] = fieldInput('Порт',String(saved.port || 10808),'number'); port.min = '1'; port.max = '65535'; port.step = '1';
  const [usernameLabel, username] = fieldInput('Логин (необязательно)',saved.username || '');
  const [passwordLabel, password] = fieldInput('Пароль (необязательно)',saved.password || '','password');
  own.append(el('label',{class:'dlg-field'},el('span',{text:'Тип'},type)),
    el('div',{class:'proxy-address-fields'},hostLabel,portLabel),
    el('div',{class:'proxy-auth-fields'},usernameLabel,passwordLabel));
  const updateMode = () => {
    const wasHidden = own.hidden;
    own.hidden = mode !== 'custom'; own.disabled = mode !== 'custom'; error.textContent = '';
    if (wasHidden && !own.hidden) animateIn(own, { duration: 160 });
  };
  const radios = [];
  for (const [value, title] of [ ['disabled','Отключить прокси'], ['system','Использовать системные настройки прокси'], ['custom','Использовать собственный прокси'] ]) {
    const radio = el('input',{type:'radio',name:'proxy-mode',value}); radio.checked = mode === value;
    radio.addEventListener('change',()=> { if (radio.checked) { mode = value; for (const other of radios) other.checked = other === radio; updateMode(); } });
    radios.push(radio); modes.append(el('label',{class:'proxy-mode'},radio,el('span',{text:title})));
  }
  const save = bPrimary('Сохранить',async()=> {
    save.disabled = true; error.textContent = '';
    try {
      const number = Number(port.value);
      if (mode === 'custom' && (!host.value.trim() || !Number.isInteger(number) || number < 1 || number > 65535)) throw new Error('Укажите адрес и порт от 1 до 65535');
      await api.SetProxySettings({mode,type:type.value,host:host.value.trim(),port:Number.isInteger(number) ? number : (saved.port || 10808),username:username.value.trim(),password:password.value});
      error.classList.add('settings-success'); error.textContent = 'Настройки прокси применены';
      showStatus('Настройки прокси применены');
    } catch (failure) { error.classList.remove('settings-success'); error.textContent = String(failure); }
    finally { save.disabled = false; }
  });
  box.append(modes,own,error,el('div',{class:'viewer-actions'},save));
  updateMode();
}
