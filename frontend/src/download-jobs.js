import { el, setStatus, state } from './main.js';

const views = new WeakMap();
const ready = job => !job.local_missing && ['done', 'skipped'].includes(job.status);
const image = ext => ['jpg','jpeg','png','gif','webp','avif','bmp'].includes((ext || '').toLowerCase());
const archive = ext => ['zip','rar','7z'].includes((ext || '').toLowerCase());
const archiveIcon = '<svg viewBox="0 0 32 32" width="28" height="28" fill="none" stroke="currentColor" stroke-width="1.6" aria-hidden="true"><rect x="6" y="3" width="20" height="26" rx="3"/><path d="M14 3v4h4v4h-4v4h4v4h-4"/><rect x="13" y="20" width="6" height="5" rx="1"/></svg>';
const statusLabels = { queued: 'В очереди', running: 'Загружается', done: 'Готово', skipped: 'Пропущен', failed: 'Ошибка', cancelled: 'Отменён' };
const text = (node, value) => { value=String(value ?? ''); if (node.textContent !== value) node.textContent=value; };
const attr = (node, name, value) => { value=String(value); if (node.getAttribute(name)!==value) node.setAttribute(name,value); };
const previewKey = job => `${job.dest}\0${job.result_path || ''}\0${job.local_revision || 0}`;
const overscan = 8, previewLimit = 128, staticLimit = 64;
const sameJob = (previous, job) => previous && Object.keys(previous).length === Object.keys(job).length && Object.keys(job).every(key => previous[key] === job[key]);

function createView(mount, api) {
  const table=el('table',{class:'jobs-table'}), body=el('tbody');
  const head=el('thead',{},el('tr',{},...['','#','Файл','Источник','Статус','Прогресс','Ошибка'].map(text=>el('th',{text}))));
  table.append(el('colgroup',{},...['thumb','id','name','source','status','progress','error'].map(key=>el('col',{class:`job-col-${key}`}))),head,body);
  mount.replaceChildren(table);
  const view={mount,api,table,head,body,jobs:new Map(),order:[],rows:new Map(),spacers:[],previews:new Map(),pending:new Map(),rowHeight:60,failed:0,geometryDirty:true};
  view.schedule=()=> {
    if (view.frame != null || view.closed) return;
    const request=window.requestAnimationFrame?.bind(window) || (fn=>setTimeout(fn,16));
    view.frame=request(()=> {view.frame=null;paint(view);});
  };
  // Short lists keep their rows (and decoded previews) mounted. Scrolling them
  // needs no JavaScript; long lists only reconcile when their window changes.
  view.onScroll=()=> {if(view.order.length>staticLimit)view.schedule();};
  view.onResize=()=> {view.geometryDirty=true;view.schedule();};
  mount.addEventListener('scroll',view.onScroll,{passive:true});
  mount.addEventListener('focusout',view.schedule);
  view.resize=typeof ResizeObserver==='undefined'?null:new ResizeObserver(view.onResize);
  view.resize?.observe(mount);
  view.resize?.observe(head);
  window.addEventListener('resize',view.onResize);
  views.set(mount,view);
  return view;
}

export function disposeDownloadJobs(mount) {
  const view=views.get(mount); if (!view) return;
  view.closed=true; view.resize?.disconnect();
  mount.removeEventListener('scroll',view.onScroll); mount.removeEventListener('focusout',view.schedule);
  window.removeEventListener('resize',view.onResize);
  if(view.frame != null)(window.cancelAnimationFrame?.bind(window) || clearTimeout)(view.frame);
  view.rows.clear();view.previews.clear();view.pending.clear();views.delete(mount);
}

export function renderDownloadJobs(mount, jobs, api) {
  let view=views.get(mount);
  if (!view || !mount.contains(view.table)) { disposeDownloadJobs(mount);view=createView(mount,api); }
  view.api=api;
  view.jobs=new Map((jobs || []).map(job=>[job.id,sameJob(view.jobs.get(job.id),job)?view.jobs.get(job.id):{...job}]));
  view.order=(jobs || []).map(job=>job.id);
  view.failed=(jobs || []).filter(job=>job.status==='failed').length;
  for(const [id,cached] of view.previews) {
    const job=view.jobs.get(id);
    if(!job || !ready(job) || previewKey(job)!==cached.key)view.previews.delete(id);
  }
  for(const [id,pending] of view.pending) {
    const job=view.jobs.get(id);
    if(!job || !ready(job) || previewKey(job)!==pending.key)view.pending.delete(id);
  }
  if(!view.order.length)mount.scrollTop=0;
  paint(view);
}

// Keep offscreen data current; only mounted rows incur DOM writes.
export function updateDownloadJobs(mount, changes) {
  const view=views.get(mount);
  if (!view || !mount?.contains(view.table)) return false;
  let added=false;
  for(const change of changes || []) {
    const previous=view.jobs.get(change.id);
    if (!previous && change.dest == null) continue;
    const job={...previous,...change};
    if(sameJob(previous,job))continue;
    view.failed+=Number(job.status==='failed')-Number(previous?.status==='failed');
    view.jobs.set(job.id,job);
    if(!previous) {view.order.push(job.id);added=true;}
    const cached=view.previews.get(job.id);
    if(cached && (!ready(job) || cached.key!==previewKey(job)))view.previews.delete(job.id);
    const pending=view.pending.get(job.id);
    if(pending && (!ready(job) || pending.key!==previewKey(job)))view.pending.delete(job.id);
    const entry=view.rows.get(job.id);
    if(entry)updateRow(view,entry,job);
  }
  if(added) {view.order.sort((a,b)=>a-b);paint(view);}
  return true;
}

export function downloadFailureCount(mount) { return views.get(mount)?.failed || 0; }
export function downloadJobCount(mount) { return views.get(mount)?.order.length || 0; }

function spacer(height) {
  const cell=el('td',{colspan:7});cell.style.height=`${height}px`;
  return el('tr',{class:'jobs-spacer','aria-hidden':'true'},cell);
}

function paint(view) {
  if(view.closed || !view.mount.contains(view.table))return;
  const {mount,table,head,body,order}=view;
  if(view.geometryDirty) {
    view.headerHeight=head.getBoundingClientRect?.().height || 32;
    view.tableTop=(table.getBoundingClientRect?.().top || 0)-(mount.getBoundingClientRect?.().top || 0)+(mount.scrollTop || 0);
    view.viewportHeight=mount.clientHeight || 600;
    const measured=view.rows.values().next().value?.row.getBoundingClientRect?.().height;
    if(measured>0 && Math.abs(measured-view.rowHeight)>.5) {
      const offset=Math.max(0,(mount.scrollTop || 0)-view.tableTop-view.headerHeight);
      mount.scrollTop=view.tableTop+view.headerHeight+offset*measured/view.rowHeight;
      view.rowHeight=measured;
    }
    view.geometryDirty=false;
  }
  const offset=Math.max(0,(mount.scrollTop || 0)-view.tableTop-view.headerHeight);
  const start=order.length<=staticLimit?0:Math.max(0,Math.min(order.length-1,Math.floor(offset/view.rowHeight))-overscan);
  const end=order.length<=staticLimit?order.length:Math.min(order.length,Math.ceil((offset+view.viewportHeight)/view.rowHeight)+overscan);
  const indices=[];for(let index=start;index<end;index++)indices.push(index);
  // Retain a focused row if the user wheels it outside the viewport.
  const focused=document.activeElement?.closest?.('tr[data-job-id]');
  if(focused && body.contains(focused)) {
    const index=order.indexOf(Number(focused.dataset.jobId));
    if(index>=0 && (index<start || index>=end))indices.push(index);
  }
  indices.sort((a,b)=>a-b);
  const ids=indices.map(index=>order[index]);
  if(view.paintedOrder===order && view.paintedCount===order.length && view.paintedRowHeight===view.rowHeight && view.paintedIDs?.length===ids.length && ids.every((id,index)=>view.paintedIDs[index]===id))return;
  view.paintedOrder=order;view.paintedCount=order.length;view.paintedRowHeight=view.rowHeight;view.paintedIDs=ids;
  const keep=new Set(), nodes=[];
  let next=0,spacerCount=0;
  const addSpacer=height=> {
    const node=view.spacers[spacerCount] ||= spacer(0);
    const value=`${height}px`;if(node.firstElementChild.style.height!==value)node.firstElementChild.style.height=value;
    nodes.push(node);spacerCount++;
  };
  for(const index of indices) {
    if(index>next)addSpacer((index-next)*view.rowHeight);
    const id=order[index],job=view.jobs.get(id);keep.add(id);
    let entry=view.rows.get(id);
    if(!entry) {entry=createRow(view,job);view.rows.set(id,entry);updateRow(view,entry,job);}
    else if(entry.job!==job)updateRow(view,entry,job);
    attr(entry.row,'aria-rowindex',index+2);nodes.push(entry.row);next=index+1;
  }
  if(next<order.length)addSpacer((order.length-next)*view.rowHeight);
  for(const [id,entry] of view.rows) if(!keep.has(id)) {entry.row.remove();view.rows.delete(id);}
  for(const node of view.spacers.slice(spacerCount))node.remove();
  let cursor=body.firstElementChild;
  for(const node of nodes) {if(node!==cursor)body.insertBefore(node,cursor);cursor=node.nextElementSibling;}
  attr(table,'aria-rowcount',order.length+1);
}

function createRow(view,job) {
  const thumb=el('td',{class:'job-thumb'}),name=el('td',{class:'job-name'}),source=el('td',{class:'job-source'}),status=el('span');
  const progressText=el('span'),meter=el('div',{class:'job-meter',role:'progressbar','aria-label':`Загрузка ${job.name}`,hidden:true},el('i'));
  attr(meter,'aria-valuemin',0);attr(meter,'aria-valuemax',100);
  const error=el('td',{class:'job-error'}),row=el('tr',{'data-job-id':job.id},thumb,el('td',{text:job.id}),name,source,el('td',{},status),el('td',{class:'job-progress'},progressText,meter),error);
  const entry={row,thumb,name,source,status,progressText,meter,error,job,previewRequested:false};
  const reveal=async()=> {if(ready(entry.job))try{await view.api().RevealJob(entry.job.id);}catch(err){setStatus(String(err),true);}};
  row.addEventListener('click',reveal);
  row.addEventListener('keydown',e=> {
    if(['Enter',' '].includes(e.key) && ready(entry.job)){e.preventDefault();reveal();}
    if(!['ArrowUp','ArrowDown','Home','End'].includes(e.key))return;
    e.preventDefault();
    const index=view.order.indexOf(entry.job.id);
    const target=Math.max(0,Math.min(view.order.length-1,e.key==='Home'?0:e.key==='End'?view.order.length-1:index+(e.key==='ArrowUp'?-1:1)));
    view.mount.scrollTop=view.tableTop+view.headerHeight+target*view.rowHeight;
    paint(view);view.rows.get(view.order[target])?.row.focus();
  });
  return entry;
}

function updateRow(view,entry,job) {
  const done=ready(job),ext=job.ext || job.name?.split('.').at(-1) || '';
  const identity=`${previewKey(job)}\0${ext}\0${done}`;
  if(entry.identity!==identity) {
    entry.previewGeneration=(entry.previewGeneration || 0)+1;
    entry.identity=identity;entry.previewRequested=false;
    entry.icon=el('span',{class:'job-file-icon'+(archive(ext)?' archive':''),'aria-label':archive(ext)?'Архив':'Файл',...(archive(ext)?{innerHTML:archiveIcon}:{text:image(ext)?'▧':'▤'})});
    entry.thumb.replaceChildren(entry.icon);
  }
  entry.job=job;
  text(entry.name,job.name);attr(entry.name,'title',job.dest);
  text(entry.source,state.sites.find(site=>site.key===job.site)?.label || job.site || '—');
  attr(entry.status,'class',`job-status ${job.local_missing?'local-missing':job.status}`);text(entry.status,job.local_missing?'Удалён локально':statusLabels[job.status] || job.status);
  text(entry.progressText,job.total>0?`${((job.received || 0)/1048576).toFixed(1)} / ${(job.total/1048576).toFixed(1)} МБ`:`${((job.received || 0)/1048576).toFixed(1)} МБ`);
  const percent=job.total>0?Math.max(0,Math.min(100,100*(job.received || 0)/job.total)):0;
  const hidden=!(job.total>0);if(entry.meter.hidden!==hidden)entry.meter.hidden=hidden;
  if(!hidden) {
    const transform=`scaleX(${percent/100})`;
    if(entry.meter.firstElementChild.style.transform!==transform)entry.meter.firstElementChild.style.transform=transform;
    attr(entry.meter,'aria-valuenow',Math.round(percent));
  }
  attr(entry.row,'data-status',job.status);
  text(entry.error,job.error || '');attr(entry.error,'title',job.error || '');
  if(entry.row.classList.contains('job-ready')!==done)entry.row.classList.toggle('job-ready',done);
  const tabIndex=done?0:-1;if(entry.row.tabIndex!==tabIndex)entry.row.tabIndex=tabIndex;
  attr(entry.row,'title',done?`Открыть расположение: ${job.result_path || job.dest}`:job.dest);
  if(done && image(ext) && !entry.previewRequested) requestPreview(view,entry);
}

function requestPreview(view,entry) {
  entry.previewRequested=true;
  const {job}=entry,key=previewKey(job),generation=entry.previewGeneration;
  const show=url=> {
    if(!url || view.closed || view.rows.get(job.id)!==entry || entry.previewGeneration!==generation)return;
    const icon=entry.icon;
    const img=el('img',{src:url,loading:'lazy',alt:'Мини-превью скачанного изображения',onerror:()=>{img.remove();icon.hidden=false;},onload:()=>{icon.hidden=true;img.classList.add('media-ready');}});
    entry.thumb.append(img);
  };
  const cached=view.previews.get(job.id);
  if(cached?.key===key){view.previews.delete(job.id);view.previews.set(job.id,cached);show(cached.url);return;}
  let pending=view.pending.get(job.id);
  if(pending?.key!==key) {
    pending={key};
    pending.promise=Promise.resolve().then(()=>view.api().JobFileURL(job.id)).then(url=> {
      const current=view.jobs.get(job.id);
      if(url && !view.closed && view.pending.get(job.id)===pending && current && ready(current) && previewKey(current)===key) {
        view.previews.set(job.id,{key,url});
        while(view.previews.size>previewLimit)view.previews.delete(view.previews.keys().next().value);
      }
      return url;
    }).finally(()=> {if(view.pending.get(job.id)===pending)view.pending.delete(job.id);});
    view.pending.set(job.id,pending);
  }
  pending.promise.then(show).catch(()=> {if(entry.previewGeneration===generation)entry.previewRequested=false;});
}
