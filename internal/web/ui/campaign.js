'use strict';

let estateRepos = [];
let estateCampaign, estateCandidate, estateRehearsal, estateBundle;

const estateEndpoint = 'estate/repositories';
const campaignEndpoint = 'migration-campaigns';
const campaignPath = () => campaignEndpoint + '/' + estateCampaign.campaign_id;
const estateRepo = (id) => estateRepos.find(repo => repo.repository_id === id);
const repoLabel = (id) => {
  const repo = estateRepo(id);
  return repo ? `${repo.package} · ${repo.version} · ${new URL(repo.origin).pathname.slice(1)}` : id.slice(0, 12);
};
const activeCampaignWave = () => estateCampaign?.waves?.[estateCampaign.active_wave];
const campaignStates = {pending: '待授权或导入', indexed: '已索引', inventory: '盘点中', frozen: '已冻结', partial: '部分采纳', delivered: '已交付',
  queued: '排队中', running: '执行中', passed: '通过', blocked: '未通过', ready: '可使用', adopted: '已采纳',
  failed: '失败', rejected: '已拒绝', 'dependency-unavailable': '依赖不可用', timeout: '超时', unavailable: '环境不可用', cancelled: '取消',
  legacy: '单仓历史活动', drifted: '基线已变更'};
const campaignLabel = (key) => campaignStates[key] || key;

function refillOptions(select, entries, placeholder, preferred = select.value) {
  select.replaceChildren(new Option(placeholder, ''), ...entries.map(([value, title]) => new Option(title, value)));
  select.value = entries.some(([key]) => key === preferred) ? preferred : '';
}

function resetCampaignWorkspace() {
  estateRepos = [];
  estateCampaign = estateCandidate = estateRehearsal = estateBundle = undefined;
  field('campaign-workbench').hidden = true;
  field('campaign-open').setAttribute('aria-expanded', 'false');
  field('campaign-detail').hidden = true;
  field('estate-catalog-panel').hidden = true;
  field('campaign-consumers').replaceChildren();
  for (const id of ['estate-repo-info', 'estate-catalog-output', 'campaign-state', 'campaign-matrix-state', 'campaign-bundle-state', 'campaign-model-status']) text(id, '');
  for (const id of ['campaign-impact', 'campaign-hunks', 'campaign-pairs', 'campaign-waves']) field(id).replaceChildren();
  for (const id of ['estate-register', 'estate-policy', 'estate-import', 'campaign-create', 'campaign-legacy-import', 'campaign-find-candidate', 'campaign-find-rehearsal', 'campaign-find-bundle', 'campaign-rebase', 'campaign-adopt']) field(id).reset();
}

function makeCampaignCells(...values) {
  const row = document.createElement('tr');
  for (const value of values) {
    const cell = document.createElement('td');
    cell.append(value instanceof Node ? value : document.createTextNode(value));
    row.append(cell);
  }
  return row;
}

function selectRepository() {
  const repo = estateRepo(value('estate-repo'));
  field('estate-owner-tools').hidden = !repo || !can('maintainer') || repo.owner !== workspace.actor;
  field('estate-catalog').disabled = !repo?.snapshot_id;
  field('estate-catalog-panel').hidden = true;
  text('estate-repo-info', repo ? `${repo.origin} · ${repo.owner} · ${campaignLabel(repo.state)}${repo.snapshot_id ? ' · ' + repo.snapshot_id : ''}` : '');
  if (!repo) return;
  for (const [id, key] of [['estate-index', 'allowed_paths'], ['estate-model', 'model_paths'], ['estate-export', 'export_paths'], ['estate-runners', 'runner_profiles']]) {
    field(id).value = (repo.policy[key] || []).join(', ');
  }
  field('estate-pin-version').value = repo.version;
}

async function refreshCampaignWorkspace(preferredRepo, preferredCampaign) {
  const [repos, campaigns] = await Promise.all([api(estateEndpoint), api(campaignEndpoint)]);
  estateRepos = repos;
  const all = repos.map(repo => [repo.repository_id, repoLabel(repo.repository_id)]);
  const indexed = all.filter(([id]) => estateRepo(id).snapshot_id);
  refillOptions(field('estate-repo'), all, '选择仓库', preferredRepo);
  refillOptions(field('campaign-provider'), indexed, '选择提供方');
  for (const select of field('campaign-consumers').querySelectorAll('select[name=repository]')) refillOptions(select, indexed, '选择消费方');
  const available = campaigns.map(c => [c.campaign_id, `${c.name} · ${campaignLabel(c.state)}`]);
  refillOptions(field('campaign-list'), available, '选择活动', preferredCampaign);
  field('estate-registration').hidden = field('campaign-create-panel').hidden = field('campaign-legacy-panel').hidden = !can('maintainer');
  if (!field('campaign-consumers').children.length) addCampaignConsumer();
  selectRepository();
  const current = value('campaign-list');
  if (current) renderCampaign(await api(campaignEndpoint + '/' + current));
}

function addCampaignConsumer() {
  const section = field('campaign-consumer-template').content.cloneNode(true);
  const select = section.querySelector('select[name=repository]');
  refillOptions(select, estateRepos.filter(repo => repo.snapshot_id).map(repo => [repo.repository_id, repoLabel(repo.repository_id)]), '选择消费方');
  const container = field('campaign-consumers');
  select.addEventListener('change', () => { select.closest('fieldset').querySelector('input[name=old]').value = estateRepo(select.value)?.snapshot_id || ''; });
  section.querySelector('.remove-consumer').addEventListener('click', evt => {
    if (container.children.length > 1) evt.currentTarget.closest('fieldset').remove();
  });
  container.append(section);
}

function renderCampaign(campaign) {
  if (campaign.campaign_id !== estateCampaign?.campaign_id || campaign.active_wave !== estateCampaign?.active_wave) {
    renderEstateCandidate(undefined);
    renderEstateRehearsal(undefined);
    renderEstateBundle(undefined);
  }
  estateCampaign = campaign;
  field('campaign-detail').hidden = false;
  const wave = activeCampaignWave();
  text('campaign-state', `${campaign.name} · ${campaignLabel(campaign.state)} · 修订 ${campaign.campaign_rev}${wave ? ' · 当前 ' + wave.wave_id : ''}`);
  field('campaign-freeze').hidden = !can('maintainer') || campaign.state !== 'inventory';
  field('campaign-actions').hidden = campaign.state === 'inventory' || campaign.state === 'legacy';
  const legacy = field('campaign-legacy-details');
  legacy.hidden = !campaign.legacy_plan;
  legacy.replaceChildren();
  if (campaign.legacy_plan) {
    const open = document.createElement('button');
    open.type = 'button';
    open.className = 'link';
    open.textContent = '查看原计划及审阅记录';
    open.addEventListener('click', () => loadPlan(campaign.legacy_plan).catch(error => notice(error.message, true)));
    const summary = document.createElement('p');
    summary.textContent = `基线 ${campaign.producer.old_snapshot} · 已采纳批次 ${campaign.legacy_selections?.length || 0}`;
    legacy.append(summary, open);
  }
  field('campaign-generate').hidden = !wave || !can('maintainer');
  field('campaign-submit-rehearsal').hidden = !wave || !can('verifier');
  field('campaign-export-bundle').hidden = !wave || !can('maintainer');
  refillOptions(field('campaign-change-repo'), (wave?.repositories || []).filter(item => estateRepo(item.repository_id)?.owner === workspace.actor)
    .map(item => [item.repository_id, repoLabel(item.repository_id)]), '选择当前波次仓库');
  field('campaign-waves').replaceChildren(...(campaign.waves || []).map(item => makeCampaignCells(item.wave_id, item.description,
    item.repositories.map(repo => repoLabel(repo.repository_id)).join('、'))));
  const impact = campaign.impact;
  text('campaign-impact-summary', campaign.impact_restricted ? '仓库授权已变更，现有影响详情已隐藏。' :
    impact ? `影响分析覆盖 ${impact.consumer_count} 个消费仓，待确认调用 ${impact.unresolved} 个。` : '');
  field('campaign-impact').replaceChildren();
  for (const item of campaign.choices || []) {
    const note = document.createElement('p');
    note.textContent = `${item.strategy} · ${item.available ? '可选' : '受阻'} · ${item.reason}`;
    field('campaign-impact').append(note);
  }
  for (const change of impact?.changes || []) {
    const detail = document.createElement('details');
    const heading = document.createElement('summary');
    heading.textContent = `${change.previous.target} · ${change.consumer_edges.length} 处调用`;
    const info = document.createElement('pre');
    info.textContent = json({旧签名: change.previous.signature, 新签名: change.current?.signature, 移除字段: change.removed_fields,
      旧样例: change.previous.response_example, 消费者: change.consumer_edges.map(edge => ({仓库: repoLabel(edge.repository), 调用: edge.callsite}))});
    detail.append(heading, info);
    field('campaign-impact').append(detail);
  }
}

function renderEstateCandidate(candidate) {
  estateCandidate = candidate;
  field('campaign-candidate-id').value = candidate?.candidate_id || '';
  text('campaign-model-status', candidate ? `${campaignLabel(candidate.state)} · ${candidate.candidate_hash || candidate.candidate_id}` +
    (candidate.usage ? ` · ${candidate.usage.model || '本地模型'} · ${candidate.usage.duration_ms} ms · ${candidate.usage.prompt_tokens}+${candidate.usage.completion_tokens} tokens` : '') : '');
  const list = field('campaign-hunks');
  list.replaceChildren();
  for (const item of candidate?.hunks || []) {
    const detail = document.createElement('details');
    const summary = document.createElement('summary');
    summary.textContent = `${item.path}:${item.line} · ${item.decision || (item.suggested ? '待审阅' : '未建议')}`;
    const patch = document.createElement('pre');
    patch.className = 'patch';
    patch.textContent = item.patch;
    detail.append(summary, patch);
    if (can('reviewer') && workspace.actor !== candidate.actor && candidate.state === 'ready' && activeCampaignWave()?.wave_id === candidate.wave_id) {
      const form = document.createElement('form');
      form.className = 'inline';
      const decision = document.createElement('select');
      decision.setAttribute('aria-label', '审阅决定');
      if (item.suggested) decision.append(new Option('采纳', 'accept'));
      decision.append(new Option('退回', 'reject'), new Option('撤回决定', 'reset'));
      const reason = document.createElement('input');
      reason.required = true;
      reason.maxLength = 512;
      reason.placeholder = '审阅理由';
      reason.setAttribute('aria-label', '审阅理由');
      const submit = document.createElement('button');
      submit.textContent = '记录';
      form.append(decision, reason, submit);
      form.addEventListener('submit', async evt => {
        evt.preventDefault();
        submit.disabled = true;
        try {
          const path = `${campaignPath()}/candidates/${candidate.candidate_id}/hunks/${item.hunk_id}`;
          renderEstateCandidate(await api(path, {candidate_rev: candidate.revision, decision: decision.value, reason: reason.value.trim()}));
          notice('已保存审阅决定。');
        } catch (error) { notice(error.message, true); }
        finally { submit.disabled = false; }
      });
      detail.append(form);
    }
    list.append(detail);
  }
}

function renderEstateRehearsal(result) {
  estateRehearsal = result;
  field('campaign-rehearsal-id').value = result?.rehearsal_id || '';
  text('campaign-matrix-state', result ? `${campaignLabel(result.state)} · ${result.pairs?.length || 0} 个组合 · ${result.matrix_id}` : '');
  field('campaign-retry-rehearsal').hidden = result?.state !== 'blocked' || !can('verifier');
  field('campaign-pairs').replaceChildren(...(result?.pairs || []).map(pair => {
    const details = document.createElement('details');
    const title = document.createElement('summary');
    title.textContent = `${pair.evidence?.length || 0} 次执行`;
    const proof = document.createElement('pre');
    proof.textContent = json(pair.evidence || []);
    details.append(title, proof);
    return makeCampaignCells(pair.combination, repoLabel(pair.consumer.repository_id), campaignLabel(pair.state), details);
  }));
}

function renderEstateBundle(bundle) {
  estateBundle = bundle;
  field('campaign-bundle-id').value = bundle?.bundle_id || '';
  text('campaign-bundle-state', bundle ? `${campaignLabel(bundle.state)} · ${bundle.bundle_hash || bundle.bundle_id}${bundle.adoption_ref ? ' · ' + bundle.adoption_ref : ''}` : '');
  field('campaign-download').hidden = !bundle?.patch;
  field('campaign-adopt').hidden = bundle?.state !== 'ready' || bundle.owner !== workspace.actor;
  field('campaign-rebase').hidden = bundle?.state !== 'drifted' || bundle.owner !== workspace.actor || !bundle.current_snapshot;
}

async function receiveBundle(request) {
  try {
    return await request;
  } catch (error) {
    if (error.receipt?.state === 'drifted') renderEstateBundle(error.receipt);
    throw error;
  }
}

function campaignRepoDigests(campaign, wave) {
  const values = new Set();
  const append = (provider, revision) => {
    values.add(provider);
    for (const consumer of campaign.consumers) values.add(consumer[revision]);
  };
  if (campaign.strategy === 'one-cut' || wave === 'W1') append(campaign.producer.old_snapshot, 'old_snapshot');
  if (campaign.strategy !== 'one-cut' && wave !== 'W3') append(campaign.compatible_snapshot, 'old_snapshot');
  if (campaign.strategy !== 'one-cut' && wave !== 'W1') append(campaign.compatible_snapshot, 'next_snapshot');
  if (campaign.strategy === 'one-cut' || wave !== 'W1') append(campaign.producer.next_snapshot, 'next_snapshot');
  return [...values].sort();
}

handle('campaign-open', 'click', async () => {
  const view = field('campaign-workbench');
  view.hidden = !view.hidden;
  field('campaign-open').setAttribute('aria-expanded', String(!view.hidden));
  if (!view.hidden) await refreshCampaignWorkspace();
});
handle('campaign-refresh', 'click', () => refreshCampaignWorkspace());
field('estate-repo').addEventListener('change', selectRepository);
handle('estate-catalog', 'click', async () => {
  const repo = estateRepo(value('estate-repo'));
  const data = await api(`${estateEndpoint}/${repo.repository_id}/contracts`);
  text('estate-catalog-output', json(data));
  field('estate-catalog-panel').hidden = false;
  field('estate-catalog-panel').open = true;
});
handle('estate-register', 'submit', async () => {
  const repo = await api(estateEndpoint, {package: value('estate-package'), origin: value('estate-origin'), version: value('estate-version'), owner: value('estate-owner')});
  field('estate-register').reset();
  await refreshCampaignWorkspace(repo.repository_id);
});
handle('estate-policy', 'submit', async () => {
  const repo = estateRepo(value('estate-repo'));
  await api(`${estateEndpoint}/${repo.repository_id}/policy`, {expected_revision: repo.revision, allowed_paths: parts('estate-index'),
    model_paths: parts('estate-model'), export_paths: parts('estate-export'), runner_profiles: parts('estate-runners')}, undefined, 'PUT');
  await refreshCampaignWorkspace(repo.repository_id);
});
handle('estate-import', 'submit', async () => {
  const repo = estateRepo(value('estate-repo'));
  const file = field('estate-pin-file').files[0];
  if (!file || file.size > 32 * 1024 * 1024) throw new Error('请选择 32 MB 以内的快照 JSON。');
  const capture = JSON.parse(await file.text());
  const data = capture.snapshot || capture;
  if (!idPattern.test(data?.sha256 || '')) throw new Error('快照文件缺少 SHA-256。');
  await api(`${estateEndpoint}/${repo.repository_id}/snapshots`, {expected_revision: repo.revision, version: value('estate-pin-version'),
    snapshot: data, stage_only: field('estate-stage').checked});
  field('estate-pin-file').value = '';
  notice(`已导入快照 ${data.sha256}`);
  await refreshCampaignWorkspace(repo.repository_id);
});
field('campaign-list').addEventListener('change', async () => {
  const id = value('campaign-list');
  if (!id) { field('campaign-detail').hidden = true; return; }
  try { renderCampaign(await api(campaignEndpoint + '/' + id)); }
  catch (error) { notice(error.message, true); }
});
field('campaign-add-consumer').addEventListener('click', addCampaignConsumer);
field('campaign-provider').addEventListener('change', () => {
  field('campaign-provider-old').value = estateRepo(value('campaign-provider'))?.snapshot_id || '';
});
field('campaign-strategy').addEventListener('change', () => {
  const direct = value('campaign-strategy') === 'one-cut';
  field('campaign-bridge-label').hidden = direct;
  field('campaign-provider-bridge').required = !direct;
});
handle('campaign-create', 'submit', async () => {
  const consumers = [...field('campaign-consumers').children].map(section => {
    const get = name => section.querySelector(`[name=${name}]`).value.trim();
    return {repository_id: get('repository'), old_snapshot: get('old'), next_snapshot: get('next'), test_path: get('test'), language: get('language')};
  });
  const strategy = value('campaign-strategy');
  const campaign = await api(campaignEndpoint, {name: value('campaign-name'), strategy, producer: {repository_id: value('campaign-provider'),
    old_snapshot: value('campaign-provider-old'), next_snapshot: value('campaign-provider-next'), test_path: value('campaign-provider-test'), language: 'php'},
    compatible_snapshot: strategy === 'one-cut' ? '' : value('campaign-provider-bridge'), provider_router: value('campaign-router'), consumers,
    legacy_targets: value('campaign-legacy').split('\n').map(line => line.trim()).filter(Boolean), contract_set: value('campaign-contract-set'), runner_profile: value('campaign-runner')});
  await refreshCampaignWorkspace(undefined, campaign.campaign_id);
});
handle('campaign-legacy-import', 'submit', async () => {
  const campaign = await api(campaignEndpoint + '/legacy', {name: value('campaign-legacy-name'), plan_id: value('campaign-legacy-plan')});
  await refreshCampaignWorkspace(undefined, campaign.campaign_id);
});
handle('campaign-freeze', 'click', async () => renderCampaign(await api(campaignPath() + '/freeze', {campaign_rev: estateCampaign.campaign_rev})));
handle('campaign-generate', 'submit', async () => {
  const mode = value('campaign-mode');
  renderEstateCandidate(await api(campaignPath() + '/candidates', {campaign_rev: estateCampaign.campaign_rev, wave_id: activeCampaignWave().wave_id,
    repository_id: value('campaign-change-repo'), mode, token_budget: mode === 'model' ? 9000 : 0}));
});
handle('campaign-find-candidate', 'submit', async () => renderEstateCandidate(await api(campaignPath() + '/candidates/' + encodeURIComponent(value('campaign-candidate-id')))));
handle('campaign-submit-rehearsal', 'click', async () => {
  const wave = activeCampaignWave().wave_id;
  renderEstateRehearsal(await api(campaignPath() + '/rehearsals', {campaign_rev: estateCampaign.campaign_rev, wave_id: wave,
    repo_digests: campaignRepoDigests(estateCampaign, wave), contract_set: estateCampaign.contract_set, runner_profile: estateCampaign.runner_profile}));
});
handle('campaign-find-rehearsal', 'submit', async () => renderEstateRehearsal(await api(campaignPath() + '/rehearsals/' + encodeURIComponent(value('campaign-rehearsal-id')))));
handle('campaign-retry-rehearsal', 'click', async () => renderEstateRehearsal(await api(`${campaignPath()}/rehearsals/${estateRehearsal.rehearsal_id}/retry`, {})));
handle('campaign-find-bundle', 'submit', async () => renderEstateBundle(await api(campaignPath() + '/bundles/' + encodeURIComponent(value('campaign-bundle-id')))));
handle('campaign-export-bundle', 'click', async () => {
  if (!estateCandidate || !estateRehearsal) throw new Error('请先读回已审阅的候选和通过的演练。');
  renderEstateBundle(await receiveBundle(api(campaignPath() + '/bundles', {campaign_rev: estateCampaign.campaign_rev,
    candidate_id: estateCandidate.candidate_id, rehearsal_id: estateRehearsal.rehearsal_id})));
});
handle('campaign-rebase', 'submit', async () => {
  renderCampaign(await api(campaignPath() + '/rebase', {campaign_rev: estateCampaign.campaign_rev,
    repository_id: estateBundle.repository_id, current_snapshot: estateBundle.current_snapshot, target_snapshot: value('campaign-rebase-target')}));
  renderEstateCandidate(undefined);
  renderEstateRehearsal(undefined);
  renderEstateBundle(undefined);
  notice('已更新基线，请重新审阅补丁并运行演练。');
});
handle('campaign-download', 'click', () => {
  download(`migration-${estateBundle.bundle_id.slice(0, 8)}.patch`, estateBundle.patch, 'text/x-diff');
  download('migration-bundle.json', json(estateBundle), 'application/json');
});
handle('campaign-adopt', 'submit', async () => {
  const bundle = await receiveBundle(api(`${campaignPath()}/bundles/${estateBundle.bundle_id}/adopt`, {campaign_rev: estateCampaign.campaign_rev,
    adoption_ref: value('campaign-adoption-ref'), observed_snapshot: value('campaign-observed')}));
  await refreshCampaignWorkspace();
  renderEstateBundle(bundle);
});
