// Execute the shipped browser assets against a bounded DOM/network boundary.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const asset = name => fs.readFileSync(new URL('../assets/' + name, import.meta.url), 'utf8');
const settle = () => new Promise(resolve => setTimeout(resolve, 0));

// commandsPage records actual form/network/reload effects of commands.js.
function commandsPage(outcome) {
  const listeners = {}, requests = [], forms = [], controls = [];
  let reloads = 0;
  const notice = {hidden:true};
  const document = {
    activeElement:null,
    body:{},
    querySelector(selector) {
      if (selector.startsWith('meta')) return {getAttribute:()=> 'token'};
      return forms.find(form => form.dataset.dirty || form.dataset.commandPending) || null;
    },
    querySelectorAll(selector) { if (selector.includes("button")) return controls; if (selector === "[data-plan-editor]") return []; return forms; },
    getElementById:()=>notice,
    addEventListener(name, listener) {(listeners[name] ||= []).push(listener);}
  };
  const window = {crypto:{randomUUID:()=> 'request-1'},location:{reload(){reloads++;}},setTimeout};
  const context = vm.createContext({window,document,Uint8Array,Promise,MutationObserver:class{observe(){}},CustomEvent:class{constructor(type,options){this.type=type;this.detail=options.detail;}},fetch:async(url,options)=>{requests.push(JSON.parse(options.body));return{status:outcome==='applied'?200:409,json:async()=>({outcome,state:'completed',detail:'coordinator decision'})};}});
  function form() {
    const handlers = {}, status = {};
    const form = {dataset:{sessionCommand:'answer_run',commandScope:'run',subjectId:'run-1',expectedRevision:'4'},
      elements:[{name:'packet_version',value:'2',dataset:{type:'number'}},{name:'question_id',value:'q1',dataset:{}},{name:'question_digest',value:'sha256:question',dataset:{}},{name:'answer',value:'JSON',dataset:{}}],
      addEventListener(name,handler){handlers[name]=handler;},querySelector(){return status;},querySelectorAll(){return[];},
      dispatchEvent(event){for(const listener of listeners[event.type]||[])listener({...event,target:form});}};
    forms.push(form);return{form,handlers,status};
  }
  const first=form();
  vm.runInContext(asset('commands.js'),context);
  return {first,form,window,document,requests,notice,controls,load:name=>vm.runInContext(asset(name),context),dispatch:(name,event)=>{for(const listener of listeners[name]||[])listener(event);},reloads:()=>reloads};
}

// A rejected bound Run answer retains text and uses the protected scoped inbox.
{
  const page=commandsPage('conflict');
  page.first.handlers.input({});
  page.first.handlers.submit({preventDefault(){}});
  await settle();await settle();
  assert.equal(page.requests[0].scope,'run','Run form must submit explicit scope');
  assert.equal(page.requests[0].subject_id,'run-1');
  assert.equal(page.requests[0].session_id,undefined);
  assert.equal(page.requests[0].payload.packet_version,2);
  assert.equal(page.first.form.dataset.dirty,'true','rejected/stale input must remain protected');
  assert.equal(page.first.form.elements[3].value,'JSON');
}

// Applying one form cannot reload away another unsent form.
{
  const page=commandsPage('applied');
  const other=page.form();other.form.dataset.dirty='true';
  page.first.handlers.input({});
  page.first.handlers.submit({preventDefault(){}});
  await settle();await settle();
  assert.equal(page.reloads(),0,'other unsent input blocks command-completion reload');
  assert.equal(typeof page.window.factoryCommands.refresh,'function');
  delete other.form.dataset.dirty;
  page.window.factoryCommands.refresh(page.first.form);
  assert.equal(page.reloads(),1,'clean page can display updated coordinator state');
}

// Typing after a refresh starts but before its response arrives cannot be wiped.
{
  let dirty=false,refresh,resolveResponse,swaps=0;
  const region={replaceWith(){swaps++;}};
  const fresh={getElementById:()=>({})};
  const document={body:{dataset:{refreshSeconds:'1'}},hidden:false,activeElement:null,getElementById:()=>region,querySelector:()=>dirty?{}:null,importNode:value=>value,addEventListener(){}};
  const window={location:{href:'http://127.0.0.1/issues'},setInterval(callback){refresh=callback;}};
  const context=vm.createContext({window,document,Promise,DOMParser:class{parseFromString(){return fresh;}},fetch:()=>new Promise(resolve=>{resolveResponse=resolve;})});
  vm.runInContext(asset('refresh.js'),context);
  refresh();dirty=true;
  resolveResponse({headers:{get:()=> 'text/html'},text:async()=> '<html></html>'});
  await settle();await settle();
  assert.equal(swaps,0,'in-flight refresh discarded newly typed input');
}
// Unsaved edits and an in-flight plan save both withdraw older approval actions.
{
  const page=commandsPage('applied');
  const control={disabled:false,dataset:{}};page.controls.push(control);
  page.load('plan.js');
  const editor={dataset:{},matches:selector=>selector==='.plan-edit-form'};
  page.dispatch('input',{target:{closest:selector=>selector==='.plan-edit-form'?editor:null}});
  assert.equal(control.disabled,true,'typing into a draft must withdraw the old visible preview approval');
  assert.equal(control.dataset.draftBlocked,'true');
  control.disabled=false;control.dataset={};
  page.dispatch('submit',{target:editor});
  assert.equal(control.disabled,true,'saving a plan must withdraw preview actions while in flight');
}
process.stdout.write('scoped forms, rejected input, guarded reload and in-flight refresh passed\n');
