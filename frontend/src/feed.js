import { OperationScope } from './operations.js';
import { el, state, setStatus, selectEl, checkEl, stepperEl } from './main.js';
import { postKey, goldMediaPost } from './contracts.js';
import { postContent, openAuthor as showAuthor, bindGridEvents, updateDownloadStates } from './grid.js';
import { showViewer } from './viewer.js';
import { showPostMenu } from './context-menu.js';
import { FeedModel } from './feed-model.js';
import { feedCard } from './feed-card.js';
import { FeedScrollAnchor } from './feed-scroll.js';
import { expressionField, quoteFeedTag } from './feed-expression-input.js';
import { icon } from './icons.js';
import { titlebar as windowTitlebar } from './titlebar.js';
import { dismissOverlay, revealOnce, forgetReveal } from './motion.js';

const api = () => window.go.main.App;
const plural = (n, forms) => `${n} ${forms[new Intl.PluralRules('ru').select(n)] || forms.many}`;
const button = (text, fn, extra = {}) => el('button', { class:'pv-btn ghost', type:'button', text, ...extra, onclick:async e => { try { await fn(e); } catch (error) { setStatus(String(error),true); } } });

export async function mountFeed(mount) {
  document.title = 'Лента — MassDownloader';
  let bottomUp = false, atStart = true, resetPosition = true;
  try { bottomUp = window.localStorage?.getItem('feed_display_direction') === 'bottom-up'; } catch {}
  const filterList = el('nav', { class:'feed-filter-list', 'aria-label':'Фильтры ленты' });
  const title = el('h1', { text:'Избранные авторы' }), subtitle = el('div', { class:'feed-subtitle' });
  const timeline = el('div', { class:'feed-timeline' }), errors = el('details', { class:'feed-errors', hidden:true });
  const more = button('Ещё посты',loadMore), progressText = el('span', { text:'Загрузка источников…' });
  const progress = el('div', { class:'feed-progress', role:'status', 'aria-live':'polite', 'aria-atomic':'true' },
    el('span', { class:'viewer-spinner', 'aria-hidden':'true' }),progressText);
  const end = el('div', { class:'feed-end' },more);
  const scroll = el('div', { class:'feed-scroll' });
  function arrangeScroll() {
    scroll.classList.toggle('bottom-up',bottomUp);
    scroll.replaceChildren(...(bottomUp ? [errors,end,timeline] : [errors,timeline,end]));
  }
  arrangeScroll();
  const sidebar = el('aside', { id:'feed-sidebar', class:'feed-sidebar' });
  const collapse = button('←',()=>toggleSidebar(true), { class:'pv-btn ghost feed-sidebar-toggle', title:'Скрыть панель фильтров', 'aria-controls':'feed-sidebar', 'aria-expanded':'true' });
  const expand = button('→',()=>toggleSidebar(false), { class:'pv-btn ghost feed-sidebar-expand', title:'Показать панель фильтров', 'aria-controls':'feed-sidebar', 'aria-expanded':'false', hidden:true });
  function toggleSidebar(collapsed) {
    sidebar.classList.toggle('collapsed',collapsed);
    sidebar.inert = collapsed;
    collapse.setAttribute('aria-expanded',String(!collapsed));
    expand.setAttribute('aria-expanded',String(!collapsed));
    expand.hidden = !collapsed;
    (collapsed ? expand : collapse).focus();
  }
  const display = selectEl([{value:'top-down',label:'Сверху вниз'},{value:'bottom-up',label:'Снизу вверх'}],bottomUp?'bottom-up':'top-down',value=> {
    bottomUp = value === 'bottom-up';
    try { window.localStorage?.setItem('feed_display_direction',value); } catch {}
    resetPosition = true; atStart = true; arrangeScroll(); render();
  }, { 'aria-label':'Отображение', title:'Сверху вниз: новые сверху. Снизу вверх: новые снизу.' });
  const optionsPanel = el('div', { id:'feed-options', class:'feed-options-panel', hidden:true },
    el('label', { class:'dlg-field' },el('span',{text:'Отображение'}),display));
  const optionsButton = button('Опции',()=>toggleOptions(optionsPanel.hidden), { 'aria-controls':'feed-options', 'aria-expanded':'false' });
  function toggleOptions(show) {
    optionsPanel.hidden = !show; optionsButton.setAttribute('aria-expanded',String(show));
    if (show) display.focus();
  }
  const options = el('div', { class:'feed-options' },optionsButton,optionsPanel);
  const retryErrors = button('Повторить ошибки',()=>model.retryErrors(), { class:'pv-btn ghost feed-retry-errors', hidden:true });
  const outsideOptions = e => { if (!options.contains(e.target)) toggleOptions(false); };
  document.addEventListener('pointerdown',outsideOptions);
  options.addEventListener('keydown',e=> { if(e.key==='Escape'){ e.preventDefault(); toggleOptions(false); optionsButton.focus(); } });
  const refreshButton = button('',()=>select(current), { class:'pv-btn ghost feed-refresh', title:'Обновить ленту', 'aria-label':'Обновить ленту' });
  refreshButton.append(icon('refresh',16));
  const toolbar = el('header', { class:'feed-header' }, el('span', { class:'feed-brand-icon', text:'≋' }),
    el('div', { class:'feed-heading' },title,subtitle,progress),retryErrors, refreshButton,options);
  const titlebar = windowTitlebar({ title:'MassDownloader · Лента', mark:'rss', closeTitle:'Закрыть ленту' });
  sidebar.append(el('div',{class:'feed-sidebar-head'},el('div', { class:'feed-sidebar-title', text:'Мои фильтры' }),collapse),
    filterList,button('+ Добавить фильтр',()=>editFilter()));
  mount.replaceChildren(el('div', { class:'window feed-window' },titlebar,toolbar,
    el('div', { class:'feed-body' },sidebar,expand,scroll),
    el('div', { class:'feed-status' },el('span', { id:'status-dot',class:'status-dot' }),el('span', { id:'status-line',text:'Правая кнопка мыши — сохранить пост или открыть автора' }))));

  let current = 'favorites', filters = [], rendered = 60, renderTimer, stateTimer, filterVersion = 0;
  const cards = new Map();
  const observer = typeof IntersectionObserver === 'undefined' ? null : new IntersectionObserver(entries => {
    for (const entry of entries) if (entry.isIntersecting) { observer.unobserve(entry.target); cards.get(entry.target.dataset.key)?.hydrate(); }
  }, { root:scroll,rootMargin:'350px' });
  const model = new FeedModel(api(),state.sites,()=> { renderProgress(); clearTimeout(renderTimer); renderTimer = setTimeout(render,60); });
  const positionAnchor = new FeedScrollAnchor(scroll,timeline,()=>bottomUp);
  window.addEventListener('beforeunload', () => { model.close(); clearCards(); positionAnchor.close(); document.removeEventListener('pointerdown',outsideOptions); });
  async function save(posts) {
    const settings = await api().GetAllSettings();
    const media = posts.filter(p=>p.file).map(p=>goldMediaPost(p,state.filtersState[p.site]));
    let count = 0;
    if (media.length) count += await api().DownloadSelection(media,settings.download_root,settings.subfolder_by_artist !== false,[]);
    for (const post of posts.filter(p=>!p.file)) { await api().SaveFeedText(post,postContent(post).textContent); count++; }
    setStatus(media.length ? `Добавлено в очередь: ${count}` : `Сохранено текстовых постов: ${count}`);
    return count;
  }
  async function open(post) {
    state.filtersState = await api().GetSetting('filters_state') || {};
    return showViewer(post,model.posts,{download:save,openAuthor,updateDownloadStates,authorQueries});
  }
  function openAuthor(site, query, name) {
    const filter = filters.find(filter=>filter.id===current);
    const supported = state.sites.find(value=>value.key===site)?.filters?.some(spec=>spec.key==='hide_text');
    const hideText = supported && (filter?.hide_text || filter?.rules.some(rule=>rule.site===site && rule.hide_text));
    return showAuthor(site,query,name,{hideText:!!hideText});
  }
  function authorQueries(post) {
    return model.sources.filter(s=>s.site===post.site && s.posts.some(p=>postKey(p)===postKey(post))).map(s=>s.query);
  }
  const menu = (post, event, attachment) => showPostMenu(post,event,{api,download:save,downloadPost:attachment || post,openAuthor,canDownload:()=>true,downloadLabel:'Сохранить локально'});
  function clearCards() { observer?.disconnect(); for (const card of cards.values()) {positionAnchor.unobserve(card.node);forgetReveal(card.node);card.dispose();} cards.clear(); timeline.replaceChildren(); rendered = 60; atStart = true; resetPosition = true; positionAnchor.afterUpdate(null,true); }
  async function select(id) { current = id; clearCards(); renderFilterList(); await model.select(id); }
  function renderFilterList() {
    title.textContent = filters.find(f=>f.id===current)?.name || 'Лента';
    filterList.replaceChildren(...filters.map(filter => el('div', { class:'feed-filter-row' + (filter.id===current?' active':''), dataset:{filterId:filter.id} },
      button(filter.name,()=>select(filter.id),{class:'feed-filter-name',title:filter.name}),
      button('⋯',()=>editFilter(filter),{class:'feed-filter-edit',title:filter.id==='favorites'?'Настройки фильтра':'Изменить или удалить фильтр'}))));
  }
  async function refreshFilters() {
    const version = ++filterVersion;
    const next = await api().FeedFilters();
    if (version !== filterVersion) return;
    filters = next; renderFilterList();
    if (!filters.some(f=>f.id===current)) await select('favorites');
  }
  let downloadStateVersion = 0;
  async function refreshStates() {
    const version = ++downloadStateVersion;
    const list = [...cards.values()];
    const states = await api().DownloadStates(list.map(c=>c.post));
    if (version !== downloadStateVersion) return;
    for (const card of list) if (card.node.isConnected) card.update(states[postKey(card.post)]);
  }
  function renderProgress() {
    retryErrors.hidden = !model.errors.length;
    retryErrors.disabled = model.busy;
    progress.hidden = !model.busy;
    progressText.textContent = !model.busy ? '' : model.total
      ? `Загрузка: источники ${model.completed}/${model.total} · ${plural(model.posts.length,{one:'пост',few:'поста',many:'постов'})}`
      : 'Загрузка источников…';
  }
  function render() {
    const anchor = positionAnchor.beforeUpdate();
    // Newer posts from slower sources are sorted in above existing ones. Never let
    // them push already mounted cards out of the window: those cards would be
    // disposed and later rebuilt, reloading every image.
    for (let i = model.posts.length - 1; i >= rendered; i--) if (cards.has(postKey(model.posts[i]))) { rendered = i + 1; break; }
    const shown = model.posts.slice(0,rendered);
    if (bottomUp) shown.reverse();
    const keep = new Set(shown.map(postKey));
    for (const [key,card] of cards) if (!keep.has(key)) { observer?.unobserve(card.node); positionAnchor.unobserve(card.node); forgetReveal(card.node); card.dispose(); card.node.remove(); cards.delete(key); }
    let position = timeline.firstElementChild;
    for (const post of shown) {
      const key = postKey(post);
      let card = cards.get(key);
      if (!card) {
        card = feedCard(post,{api:api(),open,menu,save,siteLabel:state.sites.find(s=>s.key===post.site)?.label,queries:()=>authorQueries(post)}); cards.set(key,card);
        timeline.insertBefore(card.node,position); positionAnchor.observe(card.node); revealOnce(card.node,{translate:false}); if (observer) observer.observe(card.node); else card.hydrate();
      } else if (card.node !== position) timeline.insertBefore(card.node,position);
      position = card.node.nextElementSibling;
    }
    if (!shown.length) {
      timeline.replaceChildren(el('div', { class:'feed-empty' }, model.busy ? el('span',{class:'viewer-spinner'}) : el('span',{class:'feed-empty-icon',text:'≋'}),
        el('h2',{text:model.busy?'Загрузка ленты…':model.sources.length?'Постов нет':current==='favorites'?'Добавь авторов в избранное':'Нет источников для этого фильтра'}),
        el('p',{text:model.busy?'Посты появятся по мере ответа сайтов':model.sources.length?'Измени фильтр или обнови ленту':current==='favorites'?'Или создай свой фильтр по сайтам, тегам и авторам':'Проверь сайты и условия тегов в фильтре'})));
    } else for (const empty of timeline.querySelectorAll('.feed-empty')) empty.remove();
    subtitle.textContent = `${plural(model.posts.length,{one:'пост',few:'поста',many:'постов'})} · ${plural(model.sources.length,{one:'источник',few:'источника',many:'источников'})}`;
    more.disabled = model.busy; more.hidden = !model.busy && !model.hasMore && rendered>=model.posts.length;
    more.textContent = model.busy?'Загрузка…':rendered<model.posts.length?'Ещё посты':model.sources.some(s=>s.sort)?'Загрузить ещё':'Загрузить более ранние';
    errors.hidden = !model.errors.length;
    errors.replaceChildren(el('summary',{text:`Не удалось проверить источники: ${model.errors.length}`}),
      ...model.errors.map(e=>el('p',{},el('strong',{text:e.name}),` — ${e.error}`)));
    positionAnchor.afterUpdate(anchor,resetPosition);
    resetPosition = false;
    clearTimeout(stateTimer); stateTimer = setTimeout(()=>refreshStates().catch(e=>setStatus(String(e),true)),100);
  }
  async function loadMore() {
    if (model.busy) return;
    if (rendered<model.posts.length) { rendered += 40; render(); }
    else { rendered += 40; await model.more(); }
  }
  scroll.addEventListener('scroll',()=> {
    const fromBottom = scroll.scrollHeight-scroll.scrollTop-scroll.clientHeight;
    atStart = bottomUp ? fromBottom<=2 : scroll.scrollTop<=2;
    if (bottomUp ? !atStart && scroll.scrollTop<350 : scroll.scrollTop>0 && fromBottom<350) loadMore();
  },{passive:true});
  let queueStateTimer;
  const refreshDownloadStates = () => {
    if (queueStateTimer) return;
    queueStateTimer=setTimeout(()=> { queueStateTimer=undefined; clearTimeout(stateTimer); refreshStates().catch(()=>{}); },80);
  };
  for (const event of ['job:queued','job:started','job:finished','history:changed','wave:finished']) window.runtime?.EventsOn(event,refreshDownloadStates);
  window.addEventListener('download-submissions-changed',refreshDownloadStates);
  window.addEventListener('beforeunload',()=> { clearTimeout(queueStateTimer); window.removeEventListener('download-submissions-changed',refreshDownloadStates); });
  window.runtime?.EventsOn('feed:filters-changed',()=>refreshFilters().catch(e=>setStatus(String(e),true)));
  window.runtime?.EventsOn('proxy:changed',()=>select(current).catch(e=>setStatus(String(e),true)));
  for (const event of ['favorites:changed','authors:changed']) window.runtime?.EventsOn(event,()=>window.dispatchEvent(new Event('favorites-changed')));
  bindGridEvents();
  async function editFilter(filter) {
    const overlay = el('div',{class:'viewer-overlay dialog-overlay'}), box = el('form',{class:'dialog-box feed-editor'});
    const previousFocus=document.activeElement;
    const close = () => {dismissOverlay(overlay);if(previousFocus?.isConnected)previousFocus.focus();};
    overlay.addEventListener('click',e=> { if(e.target===overlay)close(); }); overlay.addEventListener('keydown',e=> { if(e.key==='Escape')close(); });
    if (filter?.id === 'favorites') {
      let hideText = !!filter.hide_text;
      const spec = state.sites.flatMap(site=>site.filters || []).find(f=>f.key==='hide_text');
      box.append(button('×',close,{class:'viewer-close',title:'Закрыть'}),el('h2',{text:'Настройки фильтра «Избранные авторы»'}),
        el('p',{class:'muted',text:'Источники этого фильтра — авторы из избранного.'}));
      if (spec) box.append(feedHideTextField(spec,hideText,on=> { hideText=on; }));
      const error = el('p',{class:'feed-editor-error',role:'alert'});
      const submit = el('button',{class:'pv-btn',type:'submit',text:'Сохранить фильтр'});
      box.append(error,el('div',{class:'viewer-actions'},submit,button('Отмена',close)));
      box.addEventListener('submit',async event=> {
        event.preventDefault(); submit.disabled=true;
        try { await api().SaveFeedFilter({...filter,hide_text:hideText}); close(); await refreshFilters(); await select('favorites'); }
        catch(errorValue) { error.textContent=String(errorValue); } finally { submit.disabled=false; }
      });
      overlay.append(box); mount.append(overlay); submit.focus(); return;
    }
    const name = el('input',{class:'field',value:filter?.name || '',placeholder:'Например: Иллюстрации',maxlength:'200',required:true});
    const rows = el('div',{class:'feed-rule-list'}), rules = [];
    const error = el('p',{class:'feed-editor-error',role:'alert'});
    function addRule(value = {}) {
      const rule = { site:value.site || state.sites[0]?.key, tags:[...(value.tags || [])], tag_expression:value.tag_expression || '', exclude:[...(value.exclude || [])], authors:[...(value.authors || [])], sort:value.sort || '', rating:value.rating || '', score:value.score ? {...value.score} : undefined, faves:value.faves ? {...value.faves} : undefined, hide_text:!!value.hide_text };
      const node = el('section',{class:'feed-rule'});
      const selectSite = el('select',{class:'select-control','aria-label':'Сайт'});
      for (const site of state.sites) selectSite.append(el('option',{value:site.key,text:site.label})); selectSite.value = rule.site;
      const fields = el('div',{class:'feed-rule-fields'});
      const refresh = () => {
        const site = state.sites.find(s=>s.key===rule.site);
        fields.replaceChildren();
        const specs = site.filters || [];
        if (specs.some(f=>f.kind==='tags'||f.key==='q')) {
          // Legacy chips keep their AND meaning when opened in the expression editor.
          const legacy = rule.tags.map(quoteFeedTag).join(' AND ');
          if (legacy) rule.tag_expression = rule.tag_expression ? `(${rule.tag_expression}) AND (${legacy})` : legacy;
          rule.tags = [];
          fields.append(expressionField(site,rule,api));
        }
        if (specs.some(f=>f.kind==='exclude')) fields.append(termsField('Исключить теги','exclude',site,rule));
        if (specs.some(f=>f.kind==='authors')) fields.append(termsField('Авторы — любой из','authors',site,rule));
        for (const f of specs) {
          if (f.key==='rating' && f.kind==='choice') {
            if (!f.options?.some(o=>o.value===rule.rating)) rule.rating = f.options?.[0]?.value || '';
            fields.append(el('label',{class:'dlg-field',title:f.tooltip},el('span',{text:f.label}),
              selectEl(f.options || [],rule.rating,v=> { rule.rating=v; },{'aria-label':f.label})));
          } else if (['score','faves'].includes(f.key) && f.kind==='threshold') fields.append(feedThresholdField(f,rule));
          else if (f.key==='hide_text' && f.kind==='bool') fields.append(feedHideTextField(f,rule.hide_text,on=> { rule.hide_text=on; }));
        }
        if (!specs.some(f=>f.key==='hide_text')) rule.hide_text=false;
        if (!specs.some(f=>f.key==='rating' && f.kind==='choice')) rule.rating = '';
        for (const key of ['score','faves']) if (!specs.some(f=>f.key===key && f.kind==='threshold')) delete rule[key];
        const sort = specs.find(f=>f.kind==='choice' && f.key==='sort');
        if (sort) {
          if (!sort.options?.some(o=>o.value===rule.sort)) rule.sort = sort.options?.[0]?.value || '';
          fields.append(el('label',{class:'dlg-field',title:sort.tooltip},el('span',{text:sort.label}),
            selectEl(sort.options || [],rule.sort,v=> { rule.sort=v; },{'aria-label':sort.label})));
        } else rule.sort = '';
      };
      selectSite.addEventListener('change',()=> { rule.site=selectSite.value; rule.tags=[]; rule.tag_expression=''; rule.exclude=[]; rule.authors=[]; rule.sort=''; rule.rating=''; rule.hide_text=false; delete rule.score; delete rule.faves; refresh(); });
      node.append(el('div',{class:'feed-rule-head'},selectSite,button('Удалить сайт',()=> { node.remove(); rules.splice(rules.indexOf(rule),1); })),fields);
      rows.append(node); rules.push(rule); refresh();
    }
    box.append(button('×',close,{class:'viewer-close',title:'Закрыть'}),el('h2',{text:filter?'Изменить фильтр':'Новый фильтр'}),
      el('label',{class:'dlg-field'},el('span',{text:'Название'}),name),
      el('p',{class:'muted',text:'Задай выражение тегов для каждого сайта. Авторы объединяются по «ИЛИ», исключения применяются ко всему выражению. Пустые поля — любые посты.'}),rows,
      button('+ Добавить сайт',()=>addRule()),error);
    for (const rule of filter?.rules || [{}]) addRule(rule);
    const submit = el('button',{class:'pv-btn',type:'submit',text:'Сохранить фильтр'});
    box.append(el('div',{class:'viewer-actions'},submit,filter ? button('Удалить фильтр',async()=> { await api().DeleteFeedFilter(filter.id); close(); await refreshFilters(); },{class:'pv-btn feed-delete'}) : null,button('Отмена',close)));
    box.addEventListener('submit',async e=> {
      e.preventDefault(); submit.disabled = true; error.textContent = '';
      try { const saved = await api().SaveFeedFilter({id:filter?.id || '',name:name.value,rules}); close(); await refreshFilters(); await select(saved.id); }
      catch (e) { error.textContent = String(e); } finally { submit.disabled=false; }
    });
    overlay.append(box); mount.append(overlay); name.focus();
  }
  try { await refreshFilters(); await select(current); }
  finally { renderProgress(); }
}

// Use the main window's controls and engine-specific labels, limits and operators.
function feedHideTextField(spec, checked, change) {
  const check = checkEl(checked,on=> { change(on); check.setAttribute('aria-checked',String(on)); },
    {role:'checkbox',tabindex:'0','aria-label':spec.label,'aria-checked':String(checked)});
  return el('div',{class:'frow feed-hide-text',title:spec.tooltip,dataset:{filter:spec.key}},check,el('span',{class:'fname',text:spec.label}));
}
function feedThresholdField(f, rule) {
  const ops = (f.ops || '≥ > =').split(' '), tokens = (f.op_tokens || '>= > =').split(' ');
  const value = rule[f.key] = { enabled:false,op:tokens[0],n:f.default_value || 0,...rule[f.key] };
  const number = stepperEl(value.n,n=> { value.n=n; },{min:f.minimum ?? -1000,max:f.maximum || 1000000});
  number.querySelector('input').setAttribute('aria-label',`${f.label}: значение`);
  const op = selectEl(ops.map((label,i)=>({label,value:tokens[i] ?? label})),value.op,v=> { value.op=v; },
    {class:'op','aria-label':`${f.label}: условие`});
  const check = checkEl(value.enabled,on=> {
    value.enabled=on; row.classList.toggle('off',!on); check.setAttribute('aria-checked',String(on));
  }, {role:'checkbox',tabindex:'0','aria-label':f.label,'aria-checked':String(value.enabled)});
  const row = el('div',{class:'frow feed-threshold' + (value.enabled?'':' off'),title:f.tooltip,dataset:{filter:f.key}},
    check,el('span',{class:'fname',text:f.label}),op,number);
  return row;
}

// Chips retain engine-specific author tokens (Pixiv IDs, creator:service:id,
// Misskey IDs). Suggestion labels never replace the actual search value.
export function termsField(label, key, site, rule) {
  const chips = el('div',{class:'feed-terms'}), suggestions = el('div',{class:'feed-suggestions',hidden:true});
  const input = el('input',{class:'field',placeholder:key==='authors'?'Имя / ID автора…':'Введи и нажми Enter', 'aria-label':label});
  let version = 0, timer, scope;
  function render() { chips.replaceChildren(...rule[key].map(value=>button(`${value} ×`,()=> { rule[key]=rule[key].filter(x=>x!==value); render(); },{class:'feed-term'}))); }
  function add(value) {
    const pattern = key==='authors' || site.tag_phrase || site.filters?.some(f=>f.key==='q') ? /[,\n]/ : /[\s,]+/;
    for (const term of String(value).split(pattern).map(s=>s.trim()).filter(Boolean)) if(!rule[key].includes(term))rule[key].push(term);
    input.value=''; suggestions.hidden=true; version++; render();
  }
  input.addEventListener('keydown',e=> { if(e.key==='Enter'){e.preventDefault();add(input.value);} });
  input.addEventListener('change',()=> { if(input.value.trim())add(input.value); });
  input.addEventListener('input',()=> {
    clearTimeout(timer); scope?.close(); const requestScope = scope = new OperationScope(api(),input); const request = ++version, query = input.value.trim(); suggestions.hidden=true;
    if(query.length<2)return;
    timer=setTimeout(async()=> {
      try {
        const list = await requestScope.call(key==='authors'?'SearchArtists':'Autocomplete',site.key,query);
        if(request!==version || !input.isConnected)return;
        suggestions.replaceChildren(...(list || []).slice(0,12).map(s=>button(s.name,()=>add(s.value || s.name),{class:'feed-suggestion'}))); suggestions.hidden=!suggestions.childNodes.length;
      } catch(error) { if(request===version && input.isConnected){suggestions.replaceChildren(el('p',{text:String(error)}));suggestions.hidden=false;} }
    },250);
  });
  input.addEventListener('blur',()=> { scope?.close(); clearTimeout(timer); });
  // Choosing a suggestion must run before blur commits the display fragment.
  suggestions.addEventListener('pointerdown',e=>e.preventDefault());
  render();
  return el('label',{class:'dlg-field feed-term-field'},el('span',{text:label}),chips,input,suggestions);
}
