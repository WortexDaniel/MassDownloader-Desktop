let nextScope=0;
export class OperationScope {
  constructor(api,owner) {
    this.api=api;this.prefix=`op-${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}-${++nextScope}`;this.pending=new Set();this.next=0;this.closed=false;
    const Observer=globalThis.MutationObserver || globalThis.window?.MutationObserver;
    if(owner?.isConnected && Observer) {this.observer=new Observer(()=>{if(!owner.isConnected)this.close();});this.observer.observe(owner.ownerDocument,{childList:true,subtree:true});}
  }
  call(method,...args) {
    if(this.closed)return Promise.reject(new Error('Операция отменена'));
    if(!this.api.RunOperation)return this.api[method](...args);
    const id=`${this.prefix}-${++this.next}`;this.pending.add(id);
    return Promise.resolve().then(()=>this.api.RunOperation(id,method,args)).finally(()=>this.pending.delete(id));
  }
  close() {if(this.closed)return;this.closed=true;this.observer?.disconnect();for(const id of this.pending)Promise.resolve().then(()=>this.api.CancelOperation?.(id)).catch(()=>{});this.pending.clear();}
}
