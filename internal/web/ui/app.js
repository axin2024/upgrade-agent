'use strict';

const field = (id) => document.getElementById(id);
const text = (id, value) => { field(id).textContent = value; };
const value = (id) => field(id).value.trim();
const parts = (id) => value(id).split(',').map((item) => item.trim()).filter(Boolean);
const json = (item) => JSON.stringify(item, null, 2);
const idPattern = /^[a-f0-9]{64}$/;
let token = '';
let generation = 0;
let workspace;
let currentPlan;
let currentCandidate;
let currentPatch = '';
let cursor = 0;

function notice(message, failed = false) {
  const node = field('notice');
  node.hidden = !message;
  node.classList.toggle('error', failed);
  node.textContent = message;
}

function can(...roles) {
  return workspace?.role === 'admin' || roles.includes(workspace?.role);
}

async function api(path, data, key) {
  const expected = generation;
  const headers = { Authorization: 'Bearer ' + token };
  const options = { headers, cache: 'no-store' };
  if (data !== undefined) {
    options.method = 'POST';
    headers['Content-Type'] = 'application/json';
    headers['Idempotency-Key'] = key || crypto.randomUUID();
    options.body = JSON.stringify(data);
  }
  const response = await fetch('/api/v1/' + path, options);
  const body = response.headers.get('Content-Type')?.includes('json') ? await response.json() : await response.text();
  if (expected !== generation) {
    throw new Error('工作区凭据已更换');
  }
  if (!response.ok) {
    throw new Error(body.error || '工作区操作失败');
  }
  return body;
}

function handle(id, event, action) {
  field(id).addEventListener(event, async (evt) => {
    evt.preventDefault();
    const button = event === 'submit' ? evt.submitter : evt.currentTarget;
    if (button) button.disabled = true;
    notice('');
    try {
      await action();
    } catch (error) {
      notice(error.message, true);
    } finally {
      if (button) button.disabled = false;
    }
  });
}

async function reloadWorkspace() {
  workspace = await api('workspace');
  text('connection', workspace.tenant + ' / ' + workspace.workspace);
  text('identity', workspace.actor + ' · ' + workspace.role);
  text('baseline', workspace.baseline || '尚未固定');
  text('revision', workspace.revision);
  field('workspace').hidden = false;
  field('setup').hidden = !can('maintainer');
  field('verify').hidden = !can('maintainer', 'verifier');
  field('review').hidden = !can('maintainer', 'reviewer');
  field('export').hidden = !can('maintainer', 'reviewer');
}

async function history(reset) {
  if (reset) {
    cursor = 0;
    field('history').replaceChildren();
  }
  const events = await api('history?after=' + cursor);
  for (const event of events) {
    cursor = event.id;
    const row = document.createElement('li');
    let jump;
    if (event.action === 'create-plan') jump = () => loadPlan(event.object);
    if (event.action === 'register-candidate') jump = () => loadCandidate(event.object);
    if (event.action === 'record-validation') jump = () => loadReport(event.object);
    if (jump) {
      const button = document.createElement('button');
      button.type = 'button';
      button.className = 'link';
      button.textContent = event.action + ' · ' + event.object.slice(0, 12);
      button.addEventListener('click', () => jump().catch((error) => notice(error.message, true)));
      row.append(button);
    } else {
      row.append(event.action + ' · ' + event.object.slice(0, 12));
    }
    const detail = document.createElement('small');
    detail.textContent = event.actor + ' · ' + new Date(event.happened_at).toLocaleString();
    row.append(detail);
    field('history').append(row);
  }
  field('more').hidden = events.length < 100;
}

function newKey() {
  const key = crypto.randomUUID();
  field('request-id').value = key;
  return key;
}

async function showJob(job) {
  field('job-status').hidden = false;
  text('job-status', json(job));
  if (job.state === 'running') {
    notice('任务仍在执行，可按请求标识查询。');
  } else if (job.state !== 'completed') {
    notice('任务 ' + job.state + ' · ' + (job.failure || job.request_id), true);
  } else if (job.kind === 'candidate') {
    await loadCandidate(job.artifact);
  } else if (job.kind === 'validation') {
    await loadReport(job.artifact);
  }
}

async function loadPlan(id) {
  if (!idPattern.test(id)) throw new Error('请输入有效的计划摘要');
  const result = await api('plans/' + id);
  currentPlan = result;
  field('plan-id').value = id;
  field('plan-summary').hidden = false;
  text('resolution', result.resolution.state);
  text('findings', json({ constraints: result.inventory.constraints, findings: result.findings, toolchain_gaps: result.inventory.toolchain_gaps }));
  field('blockers').replaceChildren();
  for (const blocker of result.resolution.blockers || []) {
    const item = document.createElement('p');
    item.textContent = [blocker.package + ' ' + blocker.constraint, blocker.reason,
      (blocker.chain || []).map((edge) => edge.from + ' → ' + edge.to).join(' / ')].join(' · ');
    field('blockers').append(item);
  }
  field('batches').replaceChildren();
  for (const batch of result.batches) {
    const row = document.createElement('tr');
    for (const item of [batch.id + ' · ' + batch.state, batch.expected_files.join(', '), (batch.prerequisites || []).join(', ') || '—']) {
      const cell = document.createElement('td');
      cell.textContent = item;
      row.append(cell);
    }
    const controls = document.createElement('td');
    if (batch.state === 'ready' && can('maintainer', 'reviewer') && workspace.baseline === currentPlan.baseline) {
      addCandidateButton(controls, result.sha256, batch.id, batch.kind === 'dependencies' ? 'composer' : 'rule', '生成');
      if (batch.kind === 'calls' && result.model_budget_tokens > 0 && can('maintainer')) {
        addCandidateButton(controls, result.sha256, batch.id, 'model', '模型建议');
      }
    } else {
      controls.textContent = '—';
    }
    row.append(controls);
    field('batches').append(row);
  }
}

function addCandidateButton(parent, plan, batch, source, caption) {
  const button = document.createElement('button');
  button.type = 'button';
  button.textContent = caption;
  if (source === 'model') button.className = 'secondary';
  button.addEventListener('click', async () => {
    button.disabled = true;
    const key = newKey();
    notice('候选任务已发起，可按请求标识查询。');
    try {
      await showJob(await api('plans/' + plan + '/batches/' + batch + '/candidates', { source }, key));
      await history(true);
    } catch (error) {
      notice(error.message, true);
    } finally {
      button.disabled = false;
    }
  });
  parent.append(button);
}

async function loadCandidate(id) {
  if (!idPattern.test(id)) throw new Error('请输入有效的候选摘要');
  const [candidate, patch] = await Promise.all([api('candidates/' + id), api('candidates/' + id + '/patch')]);
  currentCandidate = candidate;
  currentPatch = patch;
  field('candidate-id').value = id;
  field('report-panel').hidden = true;
  field('evidence-id').value = '';
  field('candidate-summary').hidden = false;
  text('candidate-info', candidate.batch + ' · ' + candidate.source + ' · ' + candidate.tool);
  text('patch', patch);
}

async function loadReport(id) {
  const report = await api('validations/' + id);
  if (currentCandidate?.sha256 !== report.candidate) await loadCandidate(report.candidate);
  field('report-panel').hidden = false;
  field('report-panel').open = true;
  field('evidence-id').value = id;
  text('report-state', report.state);
  text('report', json(report));
}

function download(filename, content, type) {
  const href = URL.createObjectURL(new Blob([content], { type }));
  const link = document.createElement('a');
  link.href = href;
  link.download = filename;
  link.click();
  window.setTimeout(() => URL.revokeObjectURL(href), 1000);
}

async function fileJSON(id, limit) {
  const file = field(id).files[0];
  if (!file || file.size > limit) throw new Error('请选择允许大小的 JSON 文件');
  return JSON.parse(await file.text());
}

handle('connect', 'submit', async () => {
  generation++;
  token = value('token');
  currentCandidate = currentPlan = workspace = undefined;
  currentPatch = '';
  field('workspace').hidden = true;
  field('plan-summary').hidden = field('candidate-summary').hidden = field('job-status').hidden = true;
  for (const id of ['plan-id', 'candidate-id', 'request-id', 'parents', 'evidence-id', 'reason', 'affected', 'snapshot-file', 'plan-file']) {
    field(id).value = '';
  }
  text('connection', '工作区未连接');
  text('identity', '正在核对凭据');
  try {
    await reloadWorkspace();
  } finally {
    field('token').value = '';
  }
  await history(true);
});
handle('refresh', 'click', async () => { await reloadWorkspace(); await history(true); });
handle('more', 'click', () => history(false));
handle('load-plan', 'submit', () => loadPlan(value('plan-id')));
handle('load-candidate', 'submit', () => loadCandidate(value('candidate-id')));
handle('save-patch', 'click', () => download('migration.patch', currentPatch, 'text/x-diff'));
handle('pin', 'submit', async () => {
  const input = await fileJSON('snapshot-file', 32 * 1024 * 1024);
  const snapshot = input.snapshot || input;
  const result = await api('snapshots', { expected_baseline: workspace.baseline, snapshot });
  await reloadWorkspace();
  notice('基线已固定 · 修订 ' + result.revision + ' · ' + result.delta.length + ' 个文件变更');
  await history(true);
});
handle('create-plan', 'submit', async () => {
  const options = await fileJSON('plan-file', 2 * 1024 * 1024);
  const result = await api('plans', { baseline: workspace.baseline, options });
  await loadPlan(result.sha256);
  await history(true);
});
handle('verify', 'submit', async () => {
  const key = newKey();
  notice('正在执行已批准的验证任务，可按请求标识查询。');
  const job = await api('candidates/' + currentCandidate.sha256 + '/validations', { prerequisite_candidates: parts('parents') }, key);
  await showJob(job);
  await history(true);
});
handle('review', 'submit', async () => {
  const decision = await api('candidates/' + currentCandidate.sha256 + '/decisions', {
    action: value('decision'), baseline: workspace.baseline, validation: value('evidence-id'),
    reason: value('reason'), confirm_affected: parts('affected'),
  });
  notice('已记录 ' + decision.action + ' · ' + decision.batch);
  await history(true);
});
handle('export', 'click', async () => {
  const bundle = await api('plans/' + currentPlan.sha256 + '/exports', { baseline: workspace.baseline });
  download('upgrade-evidence.json', json(bundle), 'application/json');
  notice('已导出 · ' + bundle.patch_sha256);
  await history(true);
});
handle('job', 'submit', async () => {
  const request = value('request-id');
  await showJob(await api('jobs/' + encodeURIComponent(request)));
});
