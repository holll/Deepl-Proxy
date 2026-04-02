const msg = document.getElementById('msg');
const tb = document.getElementById('tb');

async function req(url, options = {}) {
  const res = await fetch(url, {
    ...options,
    headers: { 'Content-Type': 'application/json', ...(options.headers || {}) },
    credentials: 'same-origin',
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || data.message || `HTTP ${res.status}`);
  return data;
}

function rowTemplate(k) {
  return `<tr>
    <td>${k.id}</td>
    <td contenteditable onblur="window.upd(${k.id}, 'name', this.innerText)">${k.name || ''}</td>
    <td contenteditable onblur="window.upd(${k.id}, 'endpoint', this.innerText)">${k.endpoint || ''}</td>
    <td>
      <select onchange="window.upd(${k.id}, 'site_type', this.value)">
        <option value="deepl_pro" ${k.site_type === 'deepl_pro' ? 'selected' : ''}>deepl-pro.com</option>
        <option value="official" ${k.site_type === 'official' ? 'selected' : ''}>official</option>
      </select>
    </td>
    <td>
      <select onchange="window.upd(${k.id}, 'status', this.value)">
        <option value="active" ${k.status === 'active' ? 'selected' : ''}>active</option>
        <option value="disabled" ${k.status === 'disabled' ? 'selected' : ''}>disabled</option>
        <option value="dead" ${k.status === 'dead' ? 'selected' : ''}>dead</option>
      </select>
    </td>
    <td><button onclick="window.delKey(${k.id})">删除</button></td>
  </tr>`;
}

async function load() {
  const data = await req('/admin/keys');
  tb.innerHTML = (data.keys || []).map(rowTemplate).join('');
}

async function addKey() {
  try {
    await req('/admin/keys', {
      method: 'POST',
      body: JSON.stringify({
        name: document.getElementById('name').value,
        auth_key: document.getElementById('auth').value,
        endpoint: document.getElementById('endpoint').value,
        site_type: document.getElementById('siteType').value,
      }),
    });
    msg.textContent = '新增成功';
    await load();
  } catch (e) {
    msg.textContent = e.message;
  }
}

window.upd = async (id, field, value) => {
  try {
    await req(`/admin/keys/${id}`, { method: 'PUT', body: JSON.stringify({ [field]: value }) });
    msg.textContent = '更新成功';
  } catch (e) {
    msg.textContent = e.message;
  }
};

window.delKey = async (id) => {
  if (!confirm('确认删除?')) return;
  try {
    await req(`/admin/keys/${id}`, { method: 'DELETE' });
    msg.textContent = '删除成功';
    await load();
  } catch (e) {
    msg.textContent = e.message;
  }
};

document.getElementById('addBtn').addEventListener('click', () => { addKey(); });
load();
