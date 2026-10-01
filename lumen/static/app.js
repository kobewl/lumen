let state = null;
let token = sessionStorage.getItem('lumen-token') || '';
let busy = false;
const $ = id => document.getElementById(id);
function node(tag, text, className) { const el = document.createElement(tag); if(text !== undefined) el.textContent = text; if(className) el.className = className; return el; }
async function api(path, body) {
  const response = await fetch('/api/' + path, {method:body ? 'POST':'GET',headers:{'Content-Type':'application/json','Authorization':'Bearer '+token},body:body ? JSON.stringify(body):undefined});
  const data = await response.json();
  if(!response.ok) throw new Error(data.error || '请求失败');
  return data;
}
function notify(text='') { $('notice').textContent=text; }
function date(value) { return value ? new Intl.DateTimeFormat('zh-CN',{timeZone:state?.timezone || 'Asia/Shanghai',dateStyle:'short',timeStyle:'short'}).format(new Date(value)) : ''; }
async function action(name,args) { try { await api('action',{name,args}); notify(); await refresh(); return true; } catch(e) { notify(e.message); return false; } }
function button(text,fn) { const b=node('button',text); b.type='button'; b.onclick=async()=>{b.disabled=true;try {await fn();} finally {b.disabled=false;}}; return b; }
function empty(list,text) { if(!list.children.length) list.append(node('p',text,'empty')); }
function render() {
  const messages=$('messages'); const nearBottom=messages.scrollHeight-messages.scrollTop-messages.clientHeight<100;
  messages.replaceChildren();
  if(!state.messages.length) {
    const welcome=node('div',undefined,'welcome'); welcome.append(node('div','✦','orb'),node('h1','有些事，不用一个人记着。'),node('p','聊聊今天，也可以把待办、偏好和提醒交给我。'));
    const suggestions=node('div',undefined,'suggestions');
    ['记住：我喜欢简洁的回答','帮我记一个 Todo：整理本周计划','每天早上 9 点提醒我规划今天'].forEach(text=>suggestions.append(button(text,()=>{$('input').value=text;$('input').focus();})));
    welcome.append(suggestions);messages.append(welcome);
  }
  for(const item of state.messages) {
    const el=node('div',undefined,'message '+(item.role==='user'?'user':'assistant')+(item.source==='schedule'?' schedule':''));
    el.append(node('small',(item.role==='user'?'你':'Lumen')+' · '+date(item.created_at)),node('div',item.content));messages.append(el);
  }
  if(nearBottom || busy) messages.scrollTop=messages.scrollHeight;
  $('status').textContent=state.model_configured?(state.feishu_enabled?'飞书已配置 · 网页管理面板':'对话 · 记忆 · 待办 · 定时任务'):'配置模型密钥后开启对话';
  $('todo-count').textContent=state.todos.filter(t=>!t.done).length || '';
  const todos=$('todo-list');todos.replaceChildren();
  state.todos.filter(t=>{const f=$('todo-filter').value;return f==='all'||f==='open'&&!t.done||f==='done'&&t.done||f==='high'&&t.priority==='high';}).forEach(t=>{
    const row=node('div',undefined,'card todo');const check=node('input');check.type='checkbox';check.checked=!!t.done;check.setAttribute('aria-label','完成 '+t.title);check.onchange=()=>action('update_todo',{id:t.id,done:check.checked});
    const title=node('div',t.title,'title'+(t.done?' done':''));if(t.due_at) title.append(node('p','截止 '+date(t.due_at),'muted'));
    title.append(node('p',(t.priority==='high'?'优先处理':'普通任务')+(t.project_id?' · '+(state.projects.find(p=>p.id===t.project_id)?.title||'项目'):''),'muted'));
    row.append(check,title,button('删除',()=>action('delete_todo',{id:t.id})));todos.append(row);
  });empty(todos,'暂无待办，给自己留一点空间。');
  const memories=$('memory-list');memories.replaceChildren();state.memories.forEach(m=>{const row=node('div',undefined,'card');row.append(node('strong',m.key),node('p',m.content),node('p',(m.status==='pending'?'待确认':m.status==='expired'||m.expires_at&&new Date(m.expires_at)<=new Date()?'已过期':'已记住')+(m.expires_at?' · 有效至 '+date(m.expires_at):''),'muted'));const controls=node('div',undefined,'actions');if(m.status==='pending')controls.append(button('确认记住',()=>action('confirm_memory',{id:m.id})));controls.append(button('修改',()=>{const value=prompt('更新记忆',m.content);if(value) return action('save_memory',{id:m.id,key:m.key,content:value});}),button('忘记',()=>action('delete_memory',{id:m.id})));row.append(controls);memories.append(row);});empty(memories,'还没有记忆。');
  $('zone').textContent='时间按 '+state.timezone+' 填写（与浏览器时区无关）';
  const schedules=$('schedule-list');schedules.replaceChildren();state.schedules.forEach(s=>{const row=node('div',undefined,'card');row.append(node('strong',s.title),node('p',s.prompt),node('p',date(s.run_at)+' · '+({none:'一次',daily:'每天',weekly:'每周'}[s.repeat])+' · '+(s.kind==='agent'?'Agent 任务':'提醒'),'muted'),node('p',s.enabled?(s.status==='running'?'执行中':'已启用'):(s.status==='success'?'已完成':s.status==='failed'?'执行失败':'已暂停'),'muted'));if(s.last_error)row.append(node('p',s.last_error,'muted'));const controls=node('div',undefined,'actions');if(s.status!=='running') controls.append(button(s.enabled?'暂停':'重新启用',()=>{if(s.enabled)return action('update_schedule',{id:s.id,enabled:false});const time=prompt('下一次执行时间（带时区的 ISO8601，如 2026-10-01T09:00:00+08:00）');if(time)return action('update_schedule',{id:s.id,enabled:true,run_at:time});}),button('删除',()=>action('delete_schedule',{id:s.id})));row.append(controls);schedules.append(row);});empty(schedules,'暂无定时任务。');
  const runs=$('run-list');runs.replaceChildren();state.runs.forEach(r=>{const row=node('div',undefined,'card');row.append(node('p',date(r.created_at)+' · '+(r.status==='success'?'成功':'失败'),'muted'),node('p',r.result));runs.append(row);});empty(runs,'任务执行后，结果会记录在这里。');
  const deliveries=$('delivery-list');deliveries.replaceChildren();(state.deliveries||[]).forEach(d=>{const row=node('div',undefined,'card');row.append(node('p',d.status==='sent'?'已发送':'等待发送 · 尝试 '+d.attempts+' 次','muted'));if(d.last_error)row.append(node('p',d.last_error,'muted'));deliveries.append(row);});empty(deliveries,state.feishu_enabled?'暂无发送记录。':'飞书尚未配置。');
  const notes=$('note-list');notes.replaceChildren();const term=$('note-search').value.toLowerCase();(state.notes||[]).filter(n=>(n.title+n.content+n.tags).toLowerCase().includes(term)).forEach(n=>{const row=node('div',undefined,'card');row.append(node('strong',n.title),node('p',n.content),node('p',n.tags+' · '+date(n.updated_at),'muted'),button('修改',()=>{const text=prompt('修改笔记',n.content);if(text)return action('save_note',{id:n.id,title:n.title,content:text,tags:n.tags});}),button('删除',()=>action('delete_note',{id:n.id})));notes.append(row);});empty(notes,'暂无匹配笔记。');
  const projects=$('project-list');projects.replaceChildren();(state.projects||[]).forEach(p=>{const row=node('div',undefined,'card');const tasks=state.todos.filter(t=>t.project_id===p.id);row.append(node('strong',p.title),node('p',p.goal),node('p',tasks.filter(t=>t.done).length+'/'+tasks.length+' 项完成 · '+({active:'进行中',completed:'已完成',paused:'暂停'}[p.status]),'muted'),button(p.status==='completed'?'重新开启':'标记完成',()=>action('save_project',{id:p.id,title:p.title,status:p.status==='completed'?'active':'completed'})));projects.append(row);});empty(projects,'还没有项目，先选一个想实现的目标。');
}
async function refresh() { try { state=await api('state');render(); }catch(e){notify(e.message);} }
$('new-conversation').onclick=async()=>{if(busy)return;if(!confirm('开始新对话？个人记忆、Todo 和定时任务会保留，旧对话不再用于回答。'))return;const button=$('new-conversation');button.disabled=true;try{await api('conversation/new',{});notify();await refresh();}catch(e){notify(e.message);}finally{button.disabled=false;}};
$('access').onclick=()=>{const value=prompt('访问令牌（本地未设置时可留空）',token);if(value!==null){token=value;sessionStorage.setItem('lumen-token',token);refresh();}};
document.querySelectorAll('.tab').forEach(b=>b.onclick=()=>{document.querySelectorAll('.tab').forEach(x=>x.classList.toggle('active',x===b));document.querySelectorAll('.panel').forEach(x=>x.hidden=x.id!==b.dataset.tab);});
$('chat').onsubmit=async e=>{e.preventDefault();if(busy)return;const text=$('input').value.trim();if(!text)return;busy=true;$('new-conversation').disabled=true;$('send').disabled=true;$('input').value='';notify('Lumen 正在处理…');try {await api('chat',{message:text});notify();}catch(e){notify(e.message);}finally{await refresh();busy=false;$('new-conversation').disabled=false;$('send').disabled=false;$('input').focus();}};
$('input').onkeydown=e=>{if(e.key==='Enter'&&!e.shiftKey&&!e.isComposing){e.preventDefault();$('chat').requestSubmit();}};
function bind(form,name,convert=x=>x){$(form).onsubmit=async e=>{e.preventDefault();const data=Object.fromEntries(new FormData(e.target));let args;try{args=convert(data);}catch(error){notify(error.message);return;}const submit=e.target.querySelector('button');submit.disabled=true;try{if(await action(name,args))e.target.reset();}finally{submit.disabled=false;}};}
$('todo-filter').onchange=render;$('note-search').oninput=render;bind('note-form','save_note');bind('project-form','save_project');
bind('todo-form','add_todo');bind('memory-form','save_memory');
// Convert wall-clock input in the configured IANA zone, rather than browser zone.
function zonedISO(value,zone){const target=Date.parse(value+'Z');if(!Number.isFinite(target))throw new Error('时间格式无效');let guess=target;const format=new Intl.DateTimeFormat('en-CA',{timeZone:zone,year:'numeric',month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit',second:'2-digit',hourCycle:'h23'});for(let i=0;i<4;i++){const p=Object.fromEntries(format.formatToParts(new Date(guess)).map(x=>[x.type,x.value]));const wall=Date.UTC(+p.year,+p.month-1,+p.day,+p.hour,+p.minute,+p.second);const diff=target-wall;if(!diff)return new Date(guess).toISOString();guess+=diff;}throw new Error('这个当地时间不存在，请选择另一个时间');}
bind('schedule-form','create_schedule',data=>({...data,run_at:zonedISO(data.run_at,state.timezone)}));
refresh();setInterval(()=>{if(!document.hidden)refresh();},4000);
