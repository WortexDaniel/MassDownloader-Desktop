import { icon } from './icons.js';

// One frameless-window title bar for the main, feed and library windows.
const controls = [
  ['win-min', 'Свернуть', 'winMin', () => window.runtime?.WindowMinimise()],
  ['win-max', 'Развернуть', 'winMax', () => window.runtime?.WindowToggleMaximise()],
  ['win-close', 'Закрыть', 'winClose', () => window.runtime?.Quit()],
];
export function titlebar({ title = 'MassDownloader', mark = 'download', closeTitle = 'Закрыть' } = {}) {
  const bar = document.createElement('div');
  bar.className = 'titlebar';
  const badge = document.createElement('span'); badge.className = 'tb-mark'; badge.append(icon(mark, 14));
  const name = document.createElement('span'); name.className = 'tb-name'; name.textContent = title;
  const spacer = document.createElement('span'); spacer.className = 'tb-spacer';
  const buttons = document.createElement('div'); buttons.className = 'win-btns';
  for (const [id, label, glyph, action] of controls) {
    const button = document.createElement('button');
    button.type = 'button'; button.id = id;
    button.className = id === 'win-close' ? 'win-btn close' : 'win-btn';
    button.title = id === 'win-close' ? closeTitle : label;
    button.setAttribute('aria-label', button.title);
    button.append(icon(glyph, 10));
    button.addEventListener('click', action);
    buttons.append(button);
  }
  bar.append(badge, name, spacer, buttons);
  // Windows convention: double-click on the caption toggles maximise.
  bar.addEventListener('dblclick', e => { if (!e.target.closest?.('.win-btns')) window.runtime?.WindowToggleMaximise(); });
  return bar;
}
