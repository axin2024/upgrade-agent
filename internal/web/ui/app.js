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
let planSearch;
let pinDraft;
let reviewDraft = [];
let reviewPreparation;
let reviewBatch;

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
    const error = new Error(body.error || '工作区操作失败');
    error.status = response.status;
    throw error;
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
  field('review-batches').hidden = field('review').hidden;
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
  const states = {
    candidate: '有待 Composer 验证的候选',
    missing_lock: '缺少锁文件。请生成 composer.lock 后重新固定基线，才能复现依赖解析。',
    blocked: '存在阻断约束',
    'needs-composer': '需补充 Composer 审核证据',
    unchanged: '无依赖变更',
  };
  text('resolution', states[result.resolution.state] || result.resolution.state);
  field('model-budget').hidden = result.model_budget_tokens === 0;
  text('model-budget', '模型预算上限 ' + result.model_budget_tokens.toLocaleString() + ' token');
  text('findings', json({ constraints: result.inventory.constraints, toolchain_gaps: result.inventory.toolchain_gaps }));
  renderFindings(result);
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
    for (const item of [batch.id + ' · ' + batch.state, batch.expected_files.join(', '),
      (batch.prerequisites || []).join(', ') || '—', (batch.tasks || []).map((task) => task.id).join(', ') || '—']) {
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

async function findPlans(reset) {
  if (reset) {
    planSearch = { package: value('target-package'), version: value('target-version') };
    field('plan-matches').replaceChildren();
    field('more-plans').hidden = true;
  }
  const search = planSearch;
  if (!search) return;
  const page = await api('plans?' + new URLSearchParams(search));
  if (search !== planSearch) return;
  for (const plan of page.items) {
    const row = document.createElement('li');
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'link';
    button.textContent = (plan.repository_ref || plan.baseline.slice(0, 12)) + ' · ' + plan.sha256.slice(0, 12);
    button.addEventListener('click', () => loadPlan(plan.sha256).catch((error) => notice(error.message, true)));
    const detail = document.createElement('small');
    detail.textContent = plan.rule_version + ' · ' + new Date(plan.created_at).toLocaleString();
    row.append(button, detail);
    field('plan-matches').append(row);
  }
  if (reset && !page.items.length) text('plan-matches', '当前工作区没有匹配的计划。');
  search.after = page.next || '';
  field('more-plans').hidden = !page.next;
}

function renderFindings(plan) {
  field('finding-source').hidden = true;
  field('finding-locations').replaceChildren();
  for (const finding of plan.findings || []) {
    const row = document.createElement('li');
    const link = document.createElement('button');
    link.className = 'link';
    link.type = 'button';
    link.textContent = finding.path + ':' + finding.line;
    link.addEventListener('click', () => loadFindingSource(plan.sha256, finding).catch((error) => notice(error.message, true)));
    const detail = document.createElement('small');
    detail.textContent = [finding.state === 'exact' ? '已定位' : '待核', finding.risk, finding.original, finding.replacement,
      ...(finding.verify || [])].filter(Boolean).join(' · ');
    const guide = document.createElement('a');
    guide.textContent = '升级依据';
    guide.href = finding.guide;
    guide.target = '_blank';
    guide.rel = 'noopener noreferrer';
    row.append(link, detail, guide);
    field('finding-locations').append(row);
  }
}

async function loadFindingSource(plan, finding) {
  const params = new URLSearchParams({ path: finding.path, line: finding.line });
  const source = await api('plans/' + plan + '/source?' + params);
  if (currentPlan?.sha256 !== plan) return;
  text('source-title', source.path + ':' + source.line);
  field('source-lines').replaceChildren(...source.lines.map((line, index) => {
    const row = document.createElement('span');
    const number = source.first_line + index;
    row.textContent = number + '  ' + line;
    row.classList.toggle('matched', number === source.line);
    return row;
  }));
  field('finding-source').hidden = false;
  field('source-lines').focus();
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

function clearPinPreview() {
  pinDraft = undefined;
  field('pin-preview').hidden = true;
  field('pin-delta').replaceChildren();
}

async function previewPin(snapshot, file) {
  const preview = await api('snapshots/preview', { snapshot });
  if (field('snapshot-file').files[0] !== file) return;
  pinDraft = { snapshot, file, preview, key: crypto.randomUUID() };
  text('pin-comparison', '修订 ' + preview.revision + ' · ' + (preview.baseline || '尚未固定') + ' → ' + preview.proposed);
  const labels = { added: '新增', modified: '修改', removed: '移除' };
  field('pin-delta').replaceChildren(...preview.delta.map((change) => {
    const item = document.createElement('li');
    item.textContent = labels[change.state] + ' · ' + change.path;
    return item;
  }));
  field('pin-preview').hidden = false;
}

const reviewActions = { adopt: '采纳', reject: '退回', withdraw: '撤回' };
const reviewStates = { pending: '未开始', applied: '已记录', rejected: '未通过' };

function reviewInput() {
  return {
    candidate: currentCandidate.sha256,
    action: value('decision'),
    baseline: workspace.baseline,
    validation: value('evidence-id'),
    reason: value('reason'),
    confirm_affected: parts('affected'),
  };
}

function renderReviewDraft() {
  const items = reviewDraft.map((item) => {
    const row = document.createElement('li');
    row.textContent = reviewActions[item.action] + ' · ' + item.candidate.slice(0, 12) + ' · ' + item.reason;
    return row;
  });
  field('review-draft').replaceChildren(...items);
}

async function refreshReviewBatches() {
  if (!can('reviewer', 'maintainer')) return;
  const ids = await api('review-batches');
  const options = ids.map((id) => new Option(id, id));
  field('recent-reviews').replaceChildren(new Option('选择批次', ''), ...options);
  if (ids.includes(reviewBatch?.request_id)) field('recent-reviews').value = reviewBatch.request_id;
}

function showReviewBatch(batch) {
  reviewBatch = batch;
  field('review-key').value = batch.request_id;
  if (Array.from(field('recent-reviews').options).some((item) => item.value === batch.request_id)) {
    field('recent-reviews').value = batch.request_id;
  }
  field('review-result').hidden = false;
  field('run-reviews').hidden = batch.completed === batch.items.length;
  text('review-progress', '已处理 ' + batch.completed + ' / ' + batch.items.length + ' · 基线修订 ' + batch.revision);
  field('review-items').replaceChildren(...batch.items.map((item) => {
    const row = document.createElement('tr');
    row.insertCell().textContent = item.candidate.slice(0, 12);
    row.insertCell().textContent = reviewActions[item.action];
    row.insertCell().textContent = reviewStates[item.state];
    row.insertCell().textContent = item.receipt
      ? item.receipt.batch + ' · ' + item.receipt.reason
      : (item.error || '—');
    return row;
  }));
}

async function loadReviewBatch(key) {
  showReviewBatch(await api('review-batches/' + encodeURIComponent(key)));
}

handle('connect', 'submit', async () => {
  generation++;
  token = value('token');
  currentCandidate = currentPlan = workspace = undefined;
  planSearch = undefined;
  field('plan-matches').replaceChildren();
  field('more-plans').hidden = true;
  clearPinPreview();
  reviewDraft = [];
  reviewPreparation = reviewBatch = undefined;
  renderReviewDraft();
  field('recent-reviews').replaceChildren(new Option('选择批次', ''));
  field('review-result').hidden = true;
  currentPatch = '';
  field('workspace').hidden = true;
  field('plan-summary').hidden = field('candidate-summary').hidden = field('job-status').hidden = true;
  for (const id of ['plan-id', 'target-package', 'target-version', 'candidate-id', 'request-id', 'parents', 'evidence-id',
    'reason', 'affected', 'snapshot-file', 'plan-file', 'review-key']) {
    field(id).value = '';
  }
  text('connection', '工作区未连接');
  text('identity', '正在核对凭据');
  try {
    await reloadWorkspace();
  } finally {
    field('token').value = '';
  }
  await Promise.all([history(true), refreshReviewBatches()]);
});
handle('refresh', 'click', async () => { await reloadWorkspace(); await Promise.all([history(true), refreshReviewBatches()]); });
handle('more', 'click', () => history(false));
handle('load-plan', 'submit', () => loadPlan(value('plan-id')));
handle('find-plans', 'submit', () => findPlans(true));
handle('more-plans', 'click', () => findPlans(false));
handle('load-candidate', 'submit', () => loadCandidate(value('candidate-id')));
handle('save-patch', 'click', () => download('migration.patch', currentPatch, 'text/x-diff'));
field('snapshot-file').addEventListener('change', clearPinPreview);
handle('pin', 'submit', async () => {
  const expected = generation;
  const file = field('snapshot-file').files[0];
  const input = await fileJSON('snapshot-file', 32 * 1024 * 1024);
  if (expected !== generation || field('snapshot-file').files[0] !== file) return;
  await previewPin(input.snapshot || input, file);
});
handle('apply-pin', 'click', async () => {
  if (!pinDraft) return;
  const { preview, snapshot, file, key } = pinDraft;
  let result;
  try {
    result = await api('snapshots', {
      expected_baseline: preview.baseline, expected_revision: preview.revision, snapshot,
    }, key);
  } catch (error) {
    if (error.status !== 409 || pinDraft?.key !== key) throw error;
    await previewPin(snapshot, file);
    notice('基线已移动，请核对更新后的差异并重新确认。', true);
    return;
  }
  if (pinDraft?.key === key) clearPinPreview();
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
  const { candidate, ...input } = reviewInput();
  const decision = await api('candidates/' + candidate + '/decisions', input);
  notice('已记录 ' + decision.action + ' · ' + decision.batch);
  await history(true);
});
handle('queue-review', 'click', async () => {
  if (reviewPreparation) throw new Error('请先重试保存或清空原选集');
  const item = reviewInput();
  if (item.reason.length < 3) throw new Error('请填写至少 3 个字符的审阅理由');
  if (reviewDraft.length >= 20) throw new Error('每次最多保存 20 个候选');
  if (reviewDraft.some((queued) => queued.candidate === item.candidate)) throw new Error('此候选已在选集中');
  reviewDraft.push(item);
  renderReviewDraft();
});
handle('clear-reviews', 'click', async () => {
  if (field('prepare-reviews').disabled) return;
  reviewDraft = [];
  reviewPreparation = undefined;
  renderReviewDraft();
});
handle('prepare-reviews', 'click', async () => {
  if (!reviewDraft.length) throw new Error('请先加入候选');
  reviewPreparation ??= { key: crypto.randomUUID(), input: { revision: workspace.revision, items: reviewDraft } };
  const { key, input } = reviewPreparation;
  field('review-key').value = key;
  try {
    showReviewBatch(await api('review-batches', input, key));
  } catch (error) {
    if (reviewPreparation?.key !== key) throw error;
    try { await loadReviewBatch(key); } catch { throw error; }
  }
  reviewPreparation = undefined;
  reviewDraft = [];
  renderReviewDraft();
  await refreshReviewBatches();
});
handle('load-reviews', 'submit', () => loadReviewBatch(value('review-key')));
handle('recent-reviews', 'change', async () => {
  const key = value('recent-reviews');
  if (key) await loadReviewBatch(key);
});
handle('run-reviews', 'click', async () => {
  const key = reviewBatch.request_id;
  const expected = generation;
  try {
    const result = await api('review-batches/' + encodeURIComponent(key) + '/run', {}, key);
    if (reviewBatch.request_id === key) showReviewBatch(result);
  } catch (error) {
    if (expected !== generation || reviewBatch?.request_id !== key) throw error;
    try { await loadReviewBatch(key); } catch { throw error; }
    if (reviewBatch.completed !== reviewBatch.items.length) throw error;
  }
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
