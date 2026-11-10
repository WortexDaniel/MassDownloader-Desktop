import { el } from './main.js';

import { OperationScope } from './operations.js';
export function quoteFeedTag(tag) {
  const value = String(tag);
  return /[\s()"\\]/u.test(value) || /^(AND|OR|NOT)$/i.test(value) ? JSON.stringify(value) : value;
}

// Tolerant token scanning also works while a group or quoted tag is unfinished.
export function feedTagAtCaret(value, caret) {
  const pattern = /"(?:\\.|[^"\\])*"?|[^\s()"]+/gu;
  for (const match of value.matchAll(pattern)) {
    const start = match.index, end = start + match[0].length;
    if (caret <= start || caret > end) continue;
    const raw = match[0];
    if (/^(AND|OR|NOT)$/i.test(raw)) return null;
    let query = value.slice(start, caret);
    if (query.startsWith('"')) {
      query = query.slice(1).replace(/"$/, '');
      try { query = JSON.parse(`"${query}"`); } catch { query = query.replace(/\\(["\\])/g, '$1'); }
    }
    query = query.replace(/^#/, '');
    return { start, end, query };
  }
  return null;
}

export function expressionField(site, rule, api) {
  const input = el('textarea', { class: 'field feed-expression', rows: '3', spellcheck: 'false',
    'aria-label': 'Выражение тегов', 'aria-autocomplete': 'list', 'aria-expanded': 'false',
    placeholder: '(loona AND solo) OR (helluva_boss AND duo)' });
  input.value = rule.tag_expression || '';
  const suggestions = el('div', { class: 'feed-suggestions', hidden: true });
  let version = 0, timer, items = [], active = 0, scope;
  const caret = () => input.selectionStart ?? input.value.length;
  function hide() { suggestions.hidden = true; input.setAttribute('aria-expanded', 'false'); }
  function choose(value, token, original) {
    if (input.value !== original) return;
    const tag = quoteFeedTag(value);
    input.value = original.slice(0, token.start) + tag + original.slice(token.end);
    rule.tag_expression = input.value;
    version++; clearTimeout(timer); hide(); input.focus();
    input.setSelectionRange?.(token.start + tag.length, token.start + tag.length);
    input.dispatchEvent(new Event('input', { bubbles: true }));
    // The completed suggestion should stay dismissed until the next edit/caret move.
    clearTimeout(timer); version++;
  }
  function selectActive() {
    [...suggestions.querySelectorAll('button')].forEach((button, index) => button.classList.toggle('active', index === active));
  }
  function suggest() {
    rule.tag_expression = input.value;
    clearTimeout(timer); scope?.close(); scope = new OperationScope(api(),input); const requestScope = scope; hide(); const request = ++version;
    const original = input.value, position = caret(), token = feedTagAtCaret(original, position);
    if (!token || token.query.length < 2) return;
    timer = setTimeout(async () => {
      try {
        const list = await requestScope.call('Autocomplete', site.key, token.query);
        if (request !== version || !input.isConnected || input.value !== original || caret() !== position) return;
        items = (list || []).slice(0, 12); active = 0;
        suggestions.replaceChildren(...items.map(suggestion => el('button', { class: 'feed-suggestion', type: 'button', text: suggestion.name,
          onclick: () => choose(suggestion.value || suggestion.name, token, original) })));
        suggestions.hidden = !items.length; input.setAttribute('aria-expanded', String(!!items.length)); selectActive();
      } catch (error) {
        if (request === version && input.isConnected) { suggestions.replaceChildren(el('p', { text: String(error) })); suggestions.hidden = false; }
      }
    }, 250);
  }
  input.addEventListener('input', suggest);
  input.addEventListener('click', suggest);
  input.addEventListener('keyup', event => { if (['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) suggest(); });
  input.addEventListener('keydown', event => {
    if (event.key === 'Escape') { clearTimeout(timer); version++; hide(); event.stopPropagation(); return; }
    if (suggestions.hidden || !items.length) return;
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault(); active = (active + (event.key === 'ArrowDown' ? 1 : -1) + items.length) % items.length; selectActive();
    } else if (event.key === 'Enter') { event.preventDefault(); suggestions.querySelectorAll('button')[active]?.click(); }
  });
  suggestions.addEventListener('pointerdown', event => event.preventDefault());
  input.addEventListener('blur', () => { scope?.close(); clearTimeout(timer); version++; hide(); });
  return el('label', { class: 'dlg-field feed-term-field' }, el('span', { text: 'Выражение тегов' }), input,
    el('span', { class: 'feed-expression-help', text: 'AND — все, OR — любой, NOT — исключить. Скобки задают группы. Пробел между тегами означает AND. Теги с пробелами или скобками — в двойных кавычках.' }), suggestions);
}
