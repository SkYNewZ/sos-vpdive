// Run from internal/web: node testdata/chat_lifecycle.cjs static/app.js
// Executes the actual controller against deterministic DOM/lifecycle stand-ins.
// This does not claim that a particular browser retained a real BFCache entry.
const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const source = fs.readFileSync(process.argv[2], 'utf8');
class Element {
  constructor() { this.hidden = false; this.value = ''; this.children = []; this.handlers = {}; this.dataset = {}; this.scrollHeight = 0; this.scrollTop = 0; this.clientHeight = 0; this.classList = { toggle() {} }; this.style = { setProperty() {} }; }
  addEventListener(type, fn) { this.handlers[type] = fn; }
  setAttribute() {} append(...nodes) { this.children.push(...nodes); } replaceChildren(...nodes) { this.children = nodes; } focus() {}
}
function setup() {
  const parts = new Map();
  const root = new Element(); root.dataset = {chatSession:'session-A',chatUser:'alice'};
  root.querySelector = s => { if (!parts.has(s)) parts.set(s, new Element()); return parts.get(s); };
  const logout = new Element();
  const handlers = {};
  const state = {version:1,messages:[{id:'epoch:1',username:'alice',name:'Alice',avatar:'',text:'Old session private conversation',at:12345}],cursor:'epoch:1',draft:'Alice draft',unread:0,muted:false,open:true};
  const storage = {'sos:chat:session-A': JSON.stringify(state)};
  Object.defineProperties(storage, {getItem:{value(k){return this[k] || null}},setItem:{value(k,v){this[k]=v}},removeItem:{value(k){delete this[k]}}});
  let restarted = 0;
  const ctx = {root, console, sessionStorage:storage, wide:{matches:true}, live:null, restartLive:()=>restarted++, fetch:async()=>({status:204,ok:true,headers:{get:()=>"session-A"}}), ResizeObserver:class{observe(){}}, document:{title:'Committee',querySelectorAll:()=>[logout],createElement:()=>new Element(),hasFocus:()=>false,visibilityState:'visible',body:new Element(),documentElement:new Element(),addEventListener(){}},window:{addEventListener(t,fn){handlers[t]=fn}}};
  vm.createContext(ctx);
  vm.runInContext(source.slice(source.indexOf('function createCommitteeChat(root)'))+'\nvar controller = createCommitteeChat(root);',ctx);
  return {root,parts,logout,handlers,storage,ctx,get restarted(){return restarted}};
}
(async () => {
  let failed = 0;
  const cases = {
    async 'logout must erase memory and must not restore sessionStorage on pagehide'() {
      const h = setup();
      h.logout.handlers.submit({type:'submit'});
      assert.equal(h.storage['sos:chat:session-A'],undefined);
      h.handlers.pagehide();
      assert.equal(h.storage['sos:chat:session-A'],undefined,'pagehide recreated the logged-out chat history');
      assert.equal(h.parts.get('[data-chat-text]').value,'');
      assert.equal(h.parts.get('[data-chat-log]').children.length,0);
    },
    async 'restored document must validate its original session before showing old history'() {
      const h = setup();
      h.handlers.pagehide();
      // A login document clears A's storage and a new session B is now valid.
      delete h.storage['sos:chat:session-A'];
      h.storage['sos:chat:session-B'] = JSON.stringify({version:1,messages:[]});
      // A successful probe identifies current session B, not document A.
      h.ctx.fetch = async()=>({status:204,ok:true,headers:{get:()=>"session-B"}});
      h.handlers.pageshow({persisted:true});
      await new Promise(resolve=>setImmediate(resolve));
      assert.ok(h.root.hidden || h.parts.get('[data-chat-log]').children.length===0,'A history is visible after validating session B');
      assert.equal(h.parts.get('[data-chat-text]').value,'','A draft survived the change of session');
      assert.ok(h.storage['sos:chat:session-B'],'an old document must not erase the replacement session history');
    },
    async 'a reconnected stream must not bind another session to an old document'() {
      const h=setup();
      h.ctx.controller.receive({session:'session-B',cursor:'epoch:2',people:[],messages:[],gap:false});
      assert.equal(h.parts.get('[data-chat-text]').value,'');
      assert.equal(h.parts.get('[data-chat-log]').children.length,0);
      assert.equal(h.storage['sos:chat:session-A'],undefined);
    },
    async 'an accepted POST arriving after logout must not revive the old controller'() {
      const h=setup();
      h.ctx.controller.receive({session:'session-A',cursor:'epoch:1',people:[],messages:[],gap:false});
      let resolve;
      h.ctx.fetch=()=>new Promise(done=>{resolve=done});
      const pending=h.parts.get('[data-chat-form]').handlers.submit({preventDefault(){}});
      h.logout.handlers.submit({type:'submit'});
      resolve({status:200,ok:true,json:async()=>({id:'epoch:2'})});
      await pending;
      assert.match(h.parts.get('[data-chat-status]').textContent,/Session terminée/);
      assert.equal(h.storage['sos:chat:session-A'],undefined);
    },
    async 'an infrastructure failure must preserve the received history and draft'() {
      const h=setup();
      h.ctx.fetch=async()=>({status:503,ok:false});
      h.handlers.pagehide();
      h.handlers.pageshow({persisted:true});
      await new Promise(resolve=>setImmediate(resolve));
      assert.equal(h.parts.get('[data-chat-text]').value,'Alice draft');
      assert.equal(h.parts.get('[data-chat-log]').children.length,1);
      assert.ok(h.storage['sos:chat:session-A']);
    },
  };
  for (const [name, run] of Object.entries(cases)) {
    try { await run(); console.log('PASS',name); }
    catch(e) { failed++; console.error('FAIL',name,'\n ',e.message); }
  }
  process.exitCode = failed ? 1 : 0;
})();
