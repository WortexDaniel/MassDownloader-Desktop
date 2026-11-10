import { postKey } from './contracts.js';

export const locationLabel = 'Показать расположение';
export const queuedLabel = 'В очереди';
const submissions = new Set();
const attachment = post => !!(post.extra?.expanded_page || post.extra?.standalone_attachment);
const submissionKey = post => `${postKey(post)}\0${attachment(post) ? post.page_index || 0 : 'all'}`;
function submitting(post) {
  const prefix = `${postKey(post)}\0`;
  return submissions.has(prefix + 'all') || (attachment(post) ? submissions.has(submissionKey(post)) : [...submissions].some(key=>key.startsWith(prefix)));
}
export function isPostSubmitting(post) { return submitting(post); }

export function queueClock() {
  const namespace = 'http://www.w3.org/2000/svg';
  const icon = document.createElementNS(namespace, 'svg');
  for (const [name,value] of Object.entries({class:'queue-clock',viewBox:'0 0 24 24',width:'16',height:'16',fill:'none',stroke:'currentColor','stroke-width':'1.8','stroke-linecap':'round','stroke-linejoin':'round','aria-hidden':'true',focusable:'false'})) icon.setAttribute(name,value);
  const circle = document.createElementNS(namespace,'circle');
  for (const [name,value] of Object.entries({cx:'12',cy:'12',r:'8.5'})) circle.setAttribute(name,value);
  const hands = document.createElementNS(namespace,'path'); hands.setAttribute('d','M12 7v5l3 2');
  icon.append(circle,hands); return icon;
}

export async function isPostQueued(post, api) {
  return submitting(post) || !!(await api.PostQueued?.(post));
}

// Keep a reopened menu blocked while album resolution is still creating jobs.
export async function submitPostDownload(post, download) {
  if (submitting(post)) return 0;
  const key = submissionKey(post);
  submissions.add(key); window.dispatchEvent(new Event('download-submissions-changed'));
  try { return await download([post]); }
  finally { submissions.delete(key); window.dispatchEvent(new Event('download-submissions-changed')); }
}

export function isDownloaded(post) {
  return post.download_progress ? post.download_progress.state === 'complete' : !!post.downloaded;
}

export async function refreshDownloadState(post, api) {
  // Expanded files retain the parent album's page count and may inherit its flags.
  // Their action must follow the exact file, independently of the other pages.
  if (post.extra?.expanded_page || post.extra?.standalone_attachment) return !!(await api.LocalMedia?.(post));
  if (!api.DownloadStates) return isDownloaded(post);
  const states = await api.DownloadStates([post]);
  return states[postKey(post)]?.state === 'complete';
}
