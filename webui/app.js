var $ = {
  login:   id('login'),    token:   id('tokenInput'), loginErr: id('loginError'),
  loginForm: id('loginForm'),
  app:     id('app'),      status:  id('statusBar'),
  fName:   id('fName'),    fAuth:   id('fAuth'),      fEndpoint: id('fEndpoint'), fProvider: id('fProvider'), addBtn: id('addBtn'),
  reload:  id('reloadBtn'),refresh: id('refreshUsageBtn'), logout: id('logoutBtn'),
  tb:      id('tb'),
  cacheTb: id('cacheTb'), cachePager: id('cachePager'), cacheCount: id('cacheCount')
};

function id(s){ return document.getElementById(s); }

/* ---- 状态栏 ---- */
function status(msg, ok){ $.status.textContent = msg||''; $.status.className = 'status-bar'+(ok===true?' ok':ok===false?' err':''); }

/* ---- 视图切换 ---- */
function showLogin(){ $.app.hidden=true; $.login.hidden=false; $.token.focus(); }
function showApp(){   $.login.hidden=true; $.app.hidden=false; }

/* ---- API 封装 ---- */
async function api(path, opt){
  opt = opt || {};
  var res = await fetch(path, {
    method: opt.method||'GET',
    headers: Object.assign({'Content-Type':'application/json'}, opt.headers||{}),
    body: opt.body,
    credentials: 'same-origin'
  });
  if (res.status === 401){ showLogin(); throw new Error('登录失效'); }
  var txt = await res.text();
  var data;
  try { data = txt ? JSON.parse(txt) : null; } catch(e){ data = {raw:txt}; }
  if (!res.ok) throw new Error((data&&(data.error||data.message)) || 'HTTP '+res.status);
  return data;
}

/* ---- 格式化 ---- */
function fmtTs(ms){
  if (!ms || ms===0) return '-';
  var d = new Date(ms);
  return d.toLocaleString('zh-CN',{hour12:false});
}

function countdown(until){
  if (!until) return '-';
  var left = until - Date.now();
  if (left <= 0) return '已到期';
  var s = Math.floor(left/1000);
  if (s < 60) return s+'秒';
  if (s < 3600) return Math.floor(s/60)+'分';
  if (s < 86400) return Math.floor(s/3600)+'时';
  return Math.floor(s/86400)+'天';
}

function labelType(t){
  return t==='temporary'?'临时':t==='monthly'?'月度配额':'永久';
}

function badgeClass(t){
  return t==='temporary'?'badge-yellow':t==='monthly'?'badge-orange':'badge-red';
}

function statusBadge(s){
  if (s==='active') return '<span class="badge badge-green">active</span>';
  if (s==='dead')   return '<span class="badge badge-red">dead</span>';
  return '<span class="badge badge-gray">disabled</span>';
}

/* ---- 表格渲染 ---- */
function row(k){
  var usage = (typeof k.character_count==='number' && typeof k.character_limit==='number')
    ? (k.character_count+'/'+k.character_limit) : '-';
  var prov = k.provider||'deepl';
  var disableType = k.disable_type ? '<span class="'+badgeClass(k.disable_type)+' badge">'+labelType(k.disable_type)+'</span>' : '-';
  var until = k.disabled_until ? countdown(k.disabled_until) : '-';
  var errTip = k.last_error_message ? ' title="'+esc(k.last_error_message)+'"' : '';

  // active 状态不展示 disable 信息
  if (k.status === 'active') { disableType = '-'; until = '-'; }

  // 状态操作按钮
  var statusActions = '';
  if (k.status === 'active' || k.status === 'dead') {
    statusActions = '<button class="btn-sm" onclick="enable('+k.id+',\'disabled\')">禁用</button>';
  } else {
    statusActions = '<button class="btn-sm" onclick="enable('+k.id+',\'active\')">启用</button>';
  }

  return '<tr data-id="'+k.id+'">'+
    '<td>'+k.id+'</td>'+
    '<td contenteditable onblur="upd('+k.id+',\'name\',this.innerText)">'+esc(k.name||'')+'</td>'+
    '<td class="cell-mono" contenteditable onblur="upd('+k.id+',\'endpoint\',this.innerText)"'+errTip+'>'+esc(k.endpoint||'')+'</td>'+
    '<td><span class="badge '+(prov==='deeplx'?'badge-orange':'badge-gray')+'">'+esc(prov)+'</span></td>'+
    '<td>'+statusBadge(k.status)+'</td>'+
    '<td>'+disableType+'</td>'+
    '<td>'+until+'</td>'+
    '<td class="cell-mono">'+usage+'</td>'+
    '<td>'+statusActions+' <button class="btn-sm" onclick="del('+k.id+')">删除</button></td>'+
    '</tr>';
}

function esc(s){ return String(s||'').replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;').replace(/'/g,'&#39;'); }

async function loadKeys(silent){
  if (!silent) status('加载中...');
  var data = await api('/admin/keys');
  $.tb.innerHTML = (data.keys||[]).map(row).join('') || '<tr><td colspan="9">暂无数据</td></tr>';
  if (!silent) status('已刷新', true);
}

/* ---- Key CRUD ---- */
async function addKey(){
  var payload = {
    name:     $.fName.value.trim(),
    auth_key: $.fAuth.value.trim(),
    endpoint: $.fEndpoint.value.trim(),
    provider: $.fProvider.value
  };
  if (!payload.name || !payload.endpoint){ status('Name 和 Endpoint 必填', false); return; }
  await api('/admin/keys',{method:'POST',body:JSON.stringify(payload)});
  $.fName.value=''; $.fAuth.value=''; $.fEndpoint.value='';
  status('新增成功', true);
  await loadKeys();
}

async function upd(id, field, value){
  var body = {}; body[field] = value;
  await api('/admin/keys/'+id,{method:'PUT',body:JSON.stringify(body)});
  status('已更新 '+field, true);
}

async function del(id){
  if (!confirm('删除 Key #'+id+'?')) return;
  await api('/admin/keys/'+id,{method:'DELETE'});
  status('已删除', true);
  await loadKeys();
}

async function enable(id, toStatus){
  await upd(id, 'status', toStatus);
  await loadKeys();
}

/* ---- 认证 ---- */
async function login(){
  var token = $.token.value.trim();
  if (!token){ $.loginErr.textContent = '请输入 ADMIN_TOKEN'; return; }
  try {
    await api('/admin/login',{method:'POST',body:JSON.stringify({admin_token:token})});
    $.token.value = ''; $.loginErr.textContent = ''; showApp(); await loadKeys(); await loadCache();
  } catch(e){ $.loginErr.textContent = e.message; }
}

async function boot(){
  try {
    var data = await api('/admin/session');
    if (data && data.authenticated){ showApp(); await loadKeys(); await loadCache(); return; }
  } catch(e){}
  showLogin();
}

/* ---- 缓存管理 ---- */
var cachePage = { limit: 30, offset: 0, total: 0 };

function fmtSize(b){
  if (b < 1024) return b+' B';
  if (b < 1048576) return (b/1024).toFixed(1)+' KB';
  return (b/1048576).toFixed(1)+' MB';
}

function cacheRow(c){
  var keyPre = c.cache_key.length > 48 ? c.cache_key.slice(0,48)+'...' : c.cache_key;
  var badge = c.expired ? '<span class="badge badge-red">expired</span>' : '<span class="badge badge-green">active</span>';
  return '<tr>'+
    '<td class="cell-mono" title="'+esc(c.cache_key)+'">'+esc(keyPre)+'</td>'+
    '<td class="cell-mono">'+esc(c.preview)+'</td>'+
    '<td>'+fmtSize(c.body_size)+'</td>'+
    '<td>'+fmtTs(c.created_at)+'</td>'+
    '<td>'+fmtTs(c.expires_at)+'</td>'+
    '<td>'+badge+'</td>'+
    '<td><button class="btn-sm" onclick="delCache(\''+esc(c.cache_key)+'\')">删除</button></td>'+
    '</tr>';
}

async function loadCache(silent){
  if (!silent) status('缓存加载中...');
  var q = '?limit='+cachePage.limit+'&offset='+cachePage.offset;
  try {
    var data = await api('/admin/cache'+q);
    $.cacheTb.innerHTML = (data.entries||[]).map(cacheRow).join('') || '<tr><td colspan="7">暂无缓存</td></tr>';
    $.cacheCount.textContent = '('+data.total+' 条)';
    cachePage.total = data.total;
    renderCachePager();
    if (!silent) status('', null);
  } catch(e){ if (!silent) status(e.message, false); else throw e; }
}

async function delCache(key){
  if (!confirm('删除缓存条目 '+key.slice(0,40)+'…?')) return;
  await api('/admin/cache?key='+encodeURIComponent(key), {method:'DELETE'});
  status('已删除', true);
  await loadCache();
}

function renderCachePager(){
  var totalPages = Math.ceil(cachePage.total / cachePage.limit);
  var cur = Math.floor(cachePage.offset / cachePage.limit) + 1;
  if (totalPages <= 1){ $.cachePager.innerHTML = ''; return; }
  var html = '<span>第 '+cur+'/'+totalPages+' 页</span> ';
  if (cachePage.offset > 0){
    html += '<button class="btn-sm" id="cachePrev">上一页</button> ';
  }
  if (cachePage.offset + cachePage.limit < cachePage.total){
    html += '<button class="btn-sm" id="cacheNext">下一页</button>';
  }
  $.cachePager.innerHTML = html;

  var prev = id('cachePrev'), next = id('cacheNext');
  if (prev) prev.addEventListener('click', function(){
    cachePage.offset = Math.max(0, cachePage.offset - cachePage.limit);
    loadCache();
  });
  if (next) next.addEventListener('click', function(){
    cachePage.offset += cachePage.limit;
    loadCache();
  });
}

/* ---- 事件 ---- */
$.loginForm.addEventListener('submit', function(e){ e.preventDefault(); login().catch(function(e){ $.loginErr.textContent = e.message; }); });
$.addBtn.addEventListener('click', function(){ addKey().catch(function(e){ status(e.message, false); }); });
$.reload.addEventListener('click', function(){
  status('加载中...');
  Promise.all([
    loadKeys(true),
    loadCache(true)
  ]).then(function(){ status('已刷新', true); }).catch(function(e){ status(e.message, false); });
});
$.refresh.addEventListener('click', function(){
  api('/admin/usage-refresh',{method:'POST'}).then(function(){ return loadKeys(); }).catch(function(e){ status(e.message, false); });
});
$.logout.addEventListener('click', function(){
  api('/admin/logout',{method:'POST'}).finally(function(){ showLogin(); });
});
boot();
