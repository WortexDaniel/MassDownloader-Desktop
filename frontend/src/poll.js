import { el } from './main.js';

function voteCount(value) {
  if (value == null || value === '' || typeof value === 'boolean') return null;
  const number = Number(value);
  return Number.isFinite(number) && number >= 0 ? Math.floor(number) : null;
}

function pollDate(value) {
  if (value == null || value === '') return '';
  if (typeof value === 'string' && /^\d{4}-\d{2}-\d{2}(?:$|[T ])/u.test(value)) {
    const date = value.slice(0, 10), parsed = new Date(date + 'T00:00:00Z');
    return !Number.isNaN(parsed.getTime()) && parsed.toISOString().slice(0, 10) === date ? date : '';
  }
  const timestamp = Number(value);
  if (!Number.isFinite(timestamp) || timestamp <= 0) return '';
  const date = new Date(timestamp < 1e12 ? timestamp * 1000 : timestamp);
  return Number.isNaN(date.getTime()) ? '' : date.toISOString().slice(0, 10);
}

export function renderPoll(poll) {
  if (!poll || typeof poll !== 'object' || !Array.isArray(poll.choices)) return null;
  const choices = poll.choices.filter(c => c && typeof c === 'object' && !Array.isArray(c)).map(c => ({
    text: String(c.text ?? ''), votes: voteCount(c.votes) ?? 0,
  }));
  if (!choices.length) return null;
  const sum = choices.reduce((total, c) => total + c.votes, 0);
  const total = voteCount(poll.total_votes) ?? sum;
  const card = el('section', { class: 'post-poll', 'aria-label': 'Результаты опроса' });
  card.append(el('h4', { class: 'poll-title', text: String(poll.title || 'Опрос') }));
  const rows = el('div', { class: 'poll-choices' });
  for (const choice of choices) {
    const percent = total > 0 ? Math.min(100, 100 * choice.votes / total) : 0;
    const bar = el('span', { class: 'poll-bar', 'aria-hidden': true }); bar.style.width = `${percent}%`;
    rows.append(el('div', { class: 'poll-choice', title: `${choice.votes} / ${total} (${percent.toFixed(1)}%)` }, bar,
      el('span', { class: 'poll-choice-text', text: choice.text }), el('span', { class: 'poll-choice-votes', text: String(choice.votes) })));
  }
  card.append(rows);
  const start = pollDate(poll.created_at ?? poll.starts_at ?? poll.start_date);
  const end = pollDate(poll.closes_at ?? poll.closed_at ?? poll.ends_at ?? poll.end_date);
  const dates = [start, end].filter(Boolean).join('—');
  const word = { one: 'голос', few: 'голоса', many: 'голосов', other: 'голоса' }[new Intl.PluralRules('ru').select(total)];
  card.append(el('div', { class: 'poll-footer', text: `${dates ? `${dates} | ` : ''}${total} ${word}${poll.allow_multiple === true ? ' · несколько вариантов' : ''}` }));
  return card;
}
