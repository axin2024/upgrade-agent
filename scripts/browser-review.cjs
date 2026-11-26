'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.UPGRADE_REHEARSAL_PLAYWRIGHT || 'playwright');

async function main() {
  const {
    UPGRADE_REHEARSAL_ORIGIN: origin,
    UPGRADE_REHEARSAL_REVIEWER: token,
    UPGRADE_REHEARSAL_MAINTAINER: maintainer,
    UPGRADE_REHEARSAL_PLAN: plan,
    UPGRADE_REHEARSAL_PROPOSED: proposedFile,
    UPGRADE_REHEARSAL_COMPETING: competingFile,
    UPGRADE_REHEARSAL_SALE: sale,
    UPGRADE_REHEARSAL_CURRENCY: currency,
    UPGRADE_REHEARSAL_REPORT: report,
    UPGRADE_REHEARSAL_BROWSER_RESULT: output,
  } = process.env;
  const digest = /^[a-f0-9]{64}$/;
  assert.match(origin, /^http:\/\/127\.0\.0\.1:\d+$/);
  for (const input of [token, maintainer, plan, sale, currency, report]) assert.match(input, digest);
  const browser = await chromium.launch({
    headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined,
  });
  try {
    const page = await browser.newPage({ viewport: { width: 1380, height: 1040 } });
    page.setDefaultTimeout(10000);
    const errors = [];
    page.on('pageerror', (error) => errors.push(error.message));
    const observed = { prepares: 0, executions: 0, queries: 0 };
    let key;
    await page.route('**/api/v1/review-batches**', async (route) => {
      const request = route.request();
      const pathname = new URL(request.url()).pathname;
      if (request.method() === 'POST' && pathname === '/api/v1/review-batches') {
        key = request.headers()['idempotency-key'];
        const response = await route.fetch();
        assert.equal(response.status(), 200);
        observed.prepares++;
        return route.abort('connectionreset');
      }
      if (request.method() === 'POST' && pathname === '/api/v1/review-batches/' + key + '/run') {
        const response = await route.fetch();
        assert.equal(response.status(), 200);
        observed.executions++;
        return route.abort('connectionreset');
      }
      if (request.method() === 'GET' && pathname === '/api/v1/review-batches/' + key) observed.queries++;
      return route.continue();
    });
    const connect = async (credential = token) => {
      await page.locator('#token').fill(credential);
      await page.getByRole('button', { name: '连接', exact: true }).click();
      await page.locator('#review-batches').waitFor({ state: 'visible' });
    };
    const queue = async (candidate, action, reason, validation) => {
      await page.locator('#candidate-id').fill(candidate);
      await page.locator('#load-candidate button').click();
      await page.waitForFunction((batch) => document.querySelector('#candidate-info').textContent.startsWith(batch),
        candidate === sale ? 'rule-sale-price' : 'rule-currency-display');
      await page.locator('#decision').selectOption(action);
      await page.locator('#reason').fill(reason);
      if (validation) await page.locator('#evidence-id').fill(validation);
      await page.locator('#queue-review').click();
    };
    await page.goto(origin, { waitUntil: 'domcontentloaded' });
    await connect();
    await queue(sale, 'reject', 'Retain the signed credit behavior');
    await queue(currency, 'adopt', 'Confirm the saved historical report', report);
    assert.equal(await page.locator('#review-draft li').count(), 2);
    await page.locator('#prepare-reviews').click();
    await page.waitForFunction(() => document.querySelector('#review-progress').textContent.includes('0 / 2'));
    assert.equal(observed.prepares, 1);
    assert.equal(observed.executions, 0);

    await page.reload({ waitUntil: 'domcontentloaded' });
    await connect();
    await page.locator('#recent-reviews').selectOption(key);
    await page.waitForFunction(() => document.querySelector('#review-progress').textContent.includes('0 / 2'));
    const pending = await page.locator('#review-items tr').allTextContents();
    assert.equal(pending.length, 2);
    assert.ok(pending[0].includes(sale.slice(0, 12)) && pending[0].includes('未开始'));
    assert.ok(pending[1].includes(currency.slice(0, 12)) && pending[1].includes('未开始'));
    await page.locator('#run-reviews').click();
    await page.waitForFunction(() => document.querySelector('#review-progress').textContent.includes('2 / 2'));
    const body = await readJson(page, origin, token, '/review-batches/' + key);
    const audit = await readJson(page, origin, token, '/history');
    assert.equal(body.completed, 2);
    assert.equal(body.items[0].state, 'applied');
    assert.equal(body.items[0].receipt.review_batch, key);
    assert.equal(body.items[0].receipt.action, 'reject');
    assert.equal(body.items[1].state, 'rejected');
    assert.match(body.items[1].error, /latest validation .* must pass/);
    assert.equal(audit.filter((event) => event.detail?.review_batch === key).length, 1);
    assert.equal(observed.prepares, 1);
    assert.equal(observed.executions, 1);
    assert.ok(observed.queries >= 3);
    await page.locator('#review-batches').screenshot({ path: path.join(path.dirname(output), 'review-browser.png') });

    await page.locator('#plan-search > summary').click();
    await page.locator('#target-package').fill('upgrade-lab/framework');
    await page.locator('#target-version').fill('^2.0');
    await page.locator('#find-plans button').click();
    const match = page.locator('#plan-matches button');
    await match.waitFor({ state: 'visible' });
    assert.equal(await match.textContent(), 'pricing-v1 · ' + plan.slice(0, 12));
    await match.click();
    await page.locator('#impact > summary').click();
    const firstFinding = page.locator('#finding-locations button').first();
    const location = await firstFinding.textContent();
    await firstFinding.click();
    await page.locator('#source-lines .matched').waitFor({ state: 'visible' });
    assert.equal(await page.locator('#source-title').textContent(), location);
    assert.match(await page.locator('#source-lines .matched').textContent(), /::/);

    await page.reload({ waitUntil: 'domcontentloaded' });
    await connect(maintainer);
    const original = await readJson(page, origin, maintainer, '/workspace');
    await page.locator('#snapshot-file').setInputFiles(proposedFile);
    await page.locator('#pin button').click();
    await page.locator('#apply-pin').waitFor({ state: 'visible' });
    assert.match(await page.locator('#pin-delta').textContent(), /移除 · composer\.lock/);
    assert.equal((await readJson(page, origin, maintainer, '/workspace')).revision, original.revision);
    const competing = JSON.parse(fs.readFileSync(competingFile, 'utf8'));
    const response = await page.request.post(origin + '/api/v1/snapshots', {
      headers: { Authorization: 'Bearer ' + maintainer, 'Idempotency-Key': 'browser-concurrent-pin' },
      data: { expected_baseline: original.baseline, expected_revision: original.revision, snapshot: competing.snapshot },
    });
    assert.equal(response.status(), 200);
    const moved = await response.json();
    await page.locator('#apply-pin').click();
    await page.getByRole('status').filter({ hasText: '基线已移动' }).waitFor({ state: 'visible' });
    assert.match(await page.locator('#pin-delta').textContent(), /移除 · composer\.lock/);
    assert.equal((await readJson(page, origin, maintainer, '/workspace')).baseline, moved.baseline);
    await page.locator('#apply-pin').click();
    await page.locator('#pin-preview').waitFor({ state: 'hidden' });
    const confirmed = await readJson(page, origin, maintainer, '/workspace');
    assert.equal(confirmed.revision, moved.revision + 1);
    assert.equal(confirmed.baseline, JSON.parse(fs.readFileSync(proposedFile, 'utf8')).snapshot.sha256);
    assert.deepEqual(errors, []);
    fs.writeFileSync(output, JSON.stringify({
      browser: browser.version(), request_id: key, persisted_across_reload: true,
      dropped_prepare_response: true, dropped_execution_response: true, ...observed,
      receipt_items: body.items, applied_audit_count: 1,
      source_location: location, found_migrated_plan_by_target: true,
      baseline_preview: { before_revision: original.revision, concurrent_revision: moved.revision,
        confirmed_revision: confirmed.revision, stale_preview_rejected: true, removed_file: 'composer.lock' },
    }, null, 2) + '\n');
  } finally {
    await browser.close();
  }
}

async function readJson(page, origin, token, path) {
  const response = await page.request.get(origin + '/api/v1' + path, { headers: { Authorization: 'Bearer ' + token } });
  assert.equal(response.status(), 200);
  return response.json();
}

main().catch((error) => { process.stderr.write(String(error.stack || error) + '\n'); process.exitCode = 1; });
