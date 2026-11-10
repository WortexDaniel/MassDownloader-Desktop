// Preview layout preferences. They are plain attributes on <html>, so every
// .grid (search, favourites, author dialog) follows them through CSS alone.
export const gridModes = ['tiles', 'mosaic'];
export const tileSizes = ['s', 'm', 'l'];
const pick = (allowed, value, fallback) => (allowed.includes(value) ? value : fallback);

export function applyViewPrefs(settings = {}) {
  const root = document.documentElement;
  root.dataset.grid = pick(gridModes, settings.ui_grid_mode, 'tiles');
  root.dataset.tile = pick(tileSizes, settings.ui_tile_size, 'm');
  return { mode: root.dataset.grid, size: root.dataset.tile };
}
export const viewPrefs = () => ({
  mode: pick(gridModes, document.documentElement.dataset.grid, 'tiles'),
  size: pick(tileSizes, document.documentElement.dataset.tile, 'm'),
});
export async function setViewPref(key, value, settings, api) {
  const setting = key === 'mode' ? 'ui_grid_mode' : 'ui_tile_size';
  if (settings) settings[setting] = value;
  const applied = applyViewPrefs({ ui_grid_mode: viewPrefs().mode, ui_tile_size: viewPrefs().size, [setting]: value });
  // The layout switches at once; a failed save only loses the choice on restart.
  try { await api?.SetSetting?.(setting, applied[key]); } catch { /* keep the applied layout */ }
  return applied;
}

// Width / height of a post's media, clamped so one panorama or one tall strip
// cannot take a whole mosaic row. Unknown sizes use the 3:4 tile.
export function postAspect(post) {
  const width = Number(post?.file?.width) || 0, height = Number(post?.file?.height) || 0;
  if (!width || !height) return 0.75;
  return Math.round(Math.min(2.2, Math.max(0.56, width / height)) * 1000) / 1000;
}
