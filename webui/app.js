const el = {
  app: document.getElementById('app'), authModal: document.getElementById('authModal'), tokenInput: document.getElementById('tokenInput'),
  verifyBtn: document.getElementById('verifyBtn'), modalError: document.getElementById('modalError'), statusBar: document.getElementById('statusBar'),
  addBtn: document.getElementById('addBtn'), reloadBtn: document.getElementById('reloadBtn'), refreshUsageBtn: document.getElementById('refreshUsageBtn'), logoutBtn: document.getElementById('logoutBtn'),
  tb: document.getElementById('tb')
};

function setStatus(msg, type='') { el.statusBar.textContent = msg || ''; el.statusBar.className = `status-bar ${type}`; }
function showLogin(){ el.app.hidden=true; el.authModal.hidden=false; }
function showApp(){ el.authModal.hidden=true; el.app.hidden=false; }

async function readJsonSafe(res){const text=await res.text(); try{return text?JSON.parse(text):null;}catch{return{raw:text}}}
async function api(path, options={}) {
  const res = await fetch(path, { method: options.method||'GET', headers: { 'Content-Type':'application/json', ...(options.headers||{}) }, body: options.body, credentials: 'same-origin' });
  const data = await readJsonSafe(res);
  if (res.status === 401) { showLogin(); throw new Error((data && (data.error || data.message)) || '登录失效'); }
  if (!res.ok) throw new Error((data && (data.error || data.message)) || `HTTP ${res.status}`);
  return data;
}

function rowTemplate(k) {
  const usage = (typeof k.character_count === 'number' && typeof k.character_limit === 'number') ? `${k.character_count}/${k.character_limit}` : '-';
  const err = [k.last_error_code, k.last_error_message].filter(Boolean).join(' | ') || '-';
  return `<tr>
    <td>${k.id}</td>
    <td contenteditable onblur="window.upd(${k.id}, 'name', this.innerText)">${k.name || ''}</td>
    <td class="mono" contenteditable onblur="window.upd(${k.id}, 'endpoint', this.innerText)">${k.endpoint || ''}</td>
    <td><select onchange="window.upd(${k.id}, 'site_type', this.value)"><option value="deepl_pro" ${k.site_type === 'deepl_pro' ? 'selected' : ''}>deepl-pro.com</option><option value="official" ${k.site_type === 'official' ? 'selected' : ''}>official</option></select></td>
    <td><select onchange="window.upd(${k.id}, 'status', this.value)"><option value="active" ${k.status === 'active' ? 'selected' : ''}>active</option><option value="disabled" ${k.status === 'disabled' ? 'selected' : ''}>disabled</option><option value="dead" ${k.status === 'dead' ? 'selected' : ''}>dead</option></select></td>
    <td>${usage}</td><td class="mono">${err}</td>
    <td><button onclick="window.delKey(${k.id})">删除</button></td>
  </tr>`;
}

async function loadKeys(){ setStatus('正在加载列表...'); const data=await api('/admin/keys'); el.tb.innerHTML=(data.keys||[]).map(rowTemplate).join('')||'<tr><td colspan="8">暂无数据</td></tr>'; setStatus('列表已更新','success'); }

async function addKey(){
  const payload = { name: document.getElementById('name').value.trim(), auth_key: document.getElementById('auth').value.trim(), endpoint: document.getElementById('endpoint').value.trim(), site_type: document.getElementById('siteType').value };
  await api('/admin/keys',{ method:'POST', body: JSON.stringify(payload)}); setStatus('新增成功','success'); await loadKeys();
}
window.upd = async (id, field, value) => { await api(`/admin/keys/${id}`, { method: 'PUT', body: JSON.stringify({ [field]: value }) }); setStatus('更新成功','success'); };
window.delKey = async (id) => { if(!confirm('确认删除?'))return; await api(`/admin/keys/${id}`, { method: 'DELETE' }); setStatus('删除成功','success'); await loadKeys(); };

async function verifyTokenAndEnter(){
  const token = (el.tokenInput.value||'').trim(); if(!token){el.modalError.textContent='请输入 ADMIN_TOKEN'; return;}
  const res = await fetch('/admin/login',{ method:'POST', headers:{'Content-Type':'application/json; charset=utf-8'}, body: JSON.stringify({admin_token: token}), credentials:'same-origin'});
  const data = await readJsonSafe(res); if(!res.ok){el.modalError.textContent=(data && (data.error||data.message)) || '验证失败'; return;}
  el.tokenInput.value=''; el.modalError.textContent=''; showApp(); await loadKeys();
}

async function boot(){
  try { const res = await fetch('/admin/session', { credentials:'same-origin' }); const data = await readJsonSafe(res);
    if (res.ok && data?.authenticated) { showApp(); await loadKeys(); } else showLogin();
  } catch { showLogin(); }
}

document.getElementById('verifyBtn').addEventListener('click',()=>verifyTokenAndEnter().catch(e=>el.modalError.textContent=e.message));
el.addBtn.addEventListener('click',()=>addKey().catch(e=>setStatus(e.message,'error')));
el.reloadBtn.addEventListener('click',()=>loadKeys().catch(e=>setStatus(e.message,'error')));
el.refreshUsageBtn.addEventListener('click',()=>api('/admin/usage-refresh',{method:'POST'}).then(()=>loadKeys()).catch(e=>setStatus(e.message,'error')));
el.logoutBtn.addEventListener('click',()=>api('/admin/logout',{method:'POST'}).finally(()=>showLogin()));
boot();
