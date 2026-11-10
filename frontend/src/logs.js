import { el, selectEl, setStatus } from './main.js';
let entries = [], redraw = () => {}, mount = null, logTimer;
export function bindLogEvents() { window.runtime?.EventsOn('log:entry', entry => {
  entries.push(entry); if (entries.length > 10000) entries.shift();
  if (mount?.isConnected && document.getElementById('content')?.dataset.tab === 'logs' && logTimer == null) {
    logTimer=setTimeout(()=>{logTimer=null;if(mount?.isConnected && mount.dataset.tab==='logs')redraw();},100);
  }
}); }
export async function renderLogs(target) {
  mount = target;
  try { entries = await window.go.main.App.Logs(); } catch (e) { setStatus(String(e), true); }
  if (target.dataset.tab !== 'logs') return;
  const search = el('input', { class: 'field log-search', placeholder: 'Поиск в логах…', 'aria-label': 'Поиск в логах' });
  const level = selectEl(['', 'DEBUG', 'INFO', 'WARN', 'ERROR'].map(value => ({ value, label: value || 'Все уровни' })), '', () => redraw(), { 'aria-label': 'Уровень логов' });
  const output = el('pre', { class: 'log-output' });
  let painted=0, previousQuery=null;
  const filtered = () => entries.filter(e => (!level.value || e.level === level.value) && `${e.time} ${e.level} ${e.message}`.toLowerCase().includes(search.value.toLowerCase()));
  let previousFirst;
  redraw = () => {
    const bottom = output.scrollTop + output.clientHeight >= output.scrollHeight - 30;
    const query=`${level.value}\0${search.value.toLowerCase()}`, format=e=>`${e.time} ${e.level} ${e.message}`;
    if(query===previousQuery && painted<=entries.length && entries[0]===previousFirst) {
      const added=entries.slice(painted).filter(e=>(!level.value || e.level===level.value) && format(e).toLowerCase().includes(search.value.toLowerCase()));
      if(added.length)output.append(document.createTextNode((output.textContent?'\n':'')+added.map(format).join('\n')));
    } else output.textContent=filtered().map(format).join('\n');
    painted=entries.length;previousQuery=query;previousFirst=entries[0];
    if(bottom)output.scrollTop=output.scrollHeight;
  };
  search.oninput = redraw;
  let copyTimer;
  const copy = el('button', { class: 'pv-btn ghost', text: 'Копировать', onclick: async () => {
    if (copy.disabled) return;
    copy.disabled = true;
    try {
      if (!window.runtime?.ClipboardSetText || await window.runtime.ClipboardSetText(output.textContent) === false) throw new Error('Не удалось скопировать в буфер обмена');
      clearTimeout(copyTimer); copy.textContent = 'Скопировано ✓'; copy.classList.add('copy-confirmed');
      copyTimer = setTimeout(() => { copy.textContent = 'Копировать'; copy.classList.remove('copy-confirmed'); }, 1400);
    } catch (e) { setStatus(String(e), true); } finally { copy.disabled = false; }
  } });
  // The recording level lives in Settings → Логи; this filter only hides.
  const recording = el('button', { class: 'pv-btn ghost log-recording', title: 'Какие записи попадают в журнал — меняется в настройках', onclick: async () => {
    try { (await import('./dialogs.js')).openSettingsDialog('logs'); } catch (e) { setStatus(String(e), true); }
  } });
  const showRecording = value => { recording.textContent = `Запись: ${String(value || 'info').toUpperCase()}`; };
  showRecording(); Promise.resolve(window.go.main.App.LogLevel?.()).then(v => { if (v) showRecording(v); }).catch(() => {});
  target.replaceChildren(el('div', { class: 'log-toolbar' }, search,
    el('div', { class: 'viewer-actions' }, recording, level, copy,
      el('button', { class: 'pv-btn ghost', text: 'Очистить', onclick: async () => {
        try { await window.go.main.App.ClearLogs(); entries = []; redraw(); } catch (e) { setStatus(String(e), true); }
      } }))), output);
  redraw(); output.scrollTop = output.scrollHeight;
}
