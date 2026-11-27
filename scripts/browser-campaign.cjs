'use strict';

const assert = require('node:assert/strict');
const { execFileSync } = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.UPGRADE_REHEARSAL_PLAYWRIGHT || 'playwright');

const {
  UPGRADE_REHEARSAL_ORIGIN: origin,
  UPGRADE_REHEARSAL_MAINTAINER: maintainer,
  UPGRADE_REHEARSAL_REVIEWER: reviewer,
  UPGRADE_REHEARSAL_CLI: cli,
  UPGRADE_REHEARSAL_LEGACY_CAMPAIGN: legacy,
  UPGRADE_REHEARSAL_CAMPAIGN_FIXTURE: workspace,
  UPGRADE_CAMPAIGN_BROWSER_RESULT: output,
} = process.env;

function fixture(name, source) {
  const repo = fs.mkdtempSync(path.join(workspace, 'browser-contract-'));
  fs.mkdirSync(path.join(repo, 'src'));
  fs.writeFileSync(path.join(repo, 'src', 'tax.php'), source);
  fs.writeFileSync(path.join(repo, 'src', 'test.php'), '<?php\nrequire __DIR__."/tax.php";\n');
  const body = execFileSync(cli, ['capture', '-root', repo, '-ref', name, '-allow', 'src']);
  const { snapshot } = JSON.parse(body);
  return { snapshot, upload: { name: `${name}.json`, mimeType: 'application/json', buffer: body } };
}

async function main() {
  assert.match(origin, /^http:\/\/127\.0\.0\.1:\d+$/);
  assert.match(legacy, /^[a-f0-9]{64}$/);
  const browser = await chromium.launch({ executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined, headless: true });
  try {
    const page = await browser.newPage({ viewport: { width: 1376, height: 1000 } });
    page.setDefaultTimeout(15000);
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    const ui = id => page.locator(`#${id}`);
    const change = async (form, endpoint, method = 'POST') => {
      const response = page.waitForResponse(value => new URL(value.url()).pathname === `/api/v1/${endpoint}` && value.request().method() === method);
      await ui(form).locator('button[type=submit],button:not([type])').last().click();
      const result = await response;
      assert.equal(result.status(), 200, (await result.text()).slice(0, 300));
      return result.json();
    };
    await page.goto(origin);
    await ui('token').fill(maintainer);
    await ui('connect').getByRole('button').click();
    await ui('campaign-open').click();
    await ui('campaign-list').locator(`option[value="${legacy}"]`).waitFor({ state: 'attached' });
    await ui('campaign-list').selectOption(legacy);
    await ui('campaign-legacy-details').getByRole('button', { name: '查看原计划及审阅记录' }).waitFor();

    const register = async (name, variants) => {
      if (await ui('estate-registration').getAttribute('open') === null) await ui('estate-registration').locator('summary').click();
      await ui('estate-package').fill(`tax/${name}`);
      await ui('estate-origin').fill(`https://example.invalid/tax/${name}`);
      await ui('estate-version').fill('1.0.0');
      const repo = await change('estate-register', 'estate/repositories');
      const endpoint = `estate/repositories/${repo.repository_id}`;
      await ui('estate-owner-tools').waitFor();
      if (await ui('estate-owner-tools').getAttribute('open') === null) await ui('estate-owner-tools').locator('summary').click();
      const denied = await page.request.post(`${origin}/api/v1/${endpoint}/snapshots`, {
        headers: { 'Authorization': `Bearer ${reviewer}`, 'Content-Type': 'application/json', 'Idempotency-Key': 'browser-unauthorized-fixture' },
        data: '{invalid JSON must not be read',
      });
      assert.equal(denied.status(), 403);
      for (const field of ['estate-index', 'estate-model', 'estate-export']) await ui(field).fill('src');
      await ui('estate-runners').fill('isolated-local');
      await change('estate-policy', `${endpoint}/policy`, 'PUT');
      for (const [index, version] of variants.entries()) {
        await ui('estate-pin-version').fill(`${index + 1}.0.0`);
        await ui('estate-stage').setChecked(index !== 0);
        await ui('estate-pin-file').setInputFiles(version.upload);
        await change('estate-import', `${endpoint}/snapshots`);
        await ui('notice').getByText(`已导入快照 ${version.snapshot.sha256}`, { exact: true }).waitFor({ state: 'visible' });
      }
      return repo.repository_id;
    };
    const before = fixture('provider-v1', '<?php\nnamespace Tax;\nfunction quote(string $sku): int { return 8; }\n');
    const compat = fixture('provider-compatible', '<?php\nnamespace Tax;\nfunction quote(string $sku): int { return 8; }\nfunction quoteCurrent(string $sku): int { return 9; }\n');
    const after = fixture('provider-v2', '<?php\nnamespace Tax;\nfunction quoteCurrent(string $sku): int { return 9; }\n');
    const oldConsumer = fixture('checkout-v1', '<?php\nfunction checkout(): int { return \\Tax\\quote("A42"); }\n');
    const newConsumer = fixture('checkout-v2', '<?php\nfunction checkout(): int { return \\Tax\\quoteCurrent("A42"); }\n');
    const provider = await register('provider', [before, compat, after]);
    await ui('estate-catalog').click();
    await ui('estate-catalog-output').getByText(/quote/i).waitFor();
    const consumer = await register('checkout', [oldConsumer, newConsumer]);
    await ui('campaign-create-panel').locator('summary').click();
    await ui('campaign-name').fill('税费契约升级');
    await ui('campaign-contract-set').fill('tax-api-v2');
    await ui('campaign-provider').selectOption(provider);
    await ui('campaign-provider-bridge').fill(compat.snapshot.sha256);
    await ui('campaign-provider-next').fill(after.snapshot.sha256);
    await ui('campaign-provider-test').fill('src/test.php');
    await ui('campaign-consumers').locator('[name=repository]').selectOption(consumer);
    await ui('campaign-consumers').locator('[name=next]').fill(newConsumer.snapshot.sha256);
    await ui('campaign-consumers').locator('[name=test]').fill('src/test.php');
    await ui('campaign-legacy').fill('php:tax\\quote');
    const campaign = await change('campaign-create', 'migration-campaigns');
    const frozen = page.waitForResponse(value => new URL(value.url()).pathname.endsWith(`/${campaign.campaign_id}/freeze`));
    await ui('campaign-freeze').click();
    const response = await frozen;
    assert.equal(response.status(), 200, await response.text());
    const data = await response.json();
    assert.equal(data.waves.length, 3);
    await ui('campaign-waves').getByRole('row').nth(2).waitFor();
    await page.getByText('策略对比与调用影响', { exact: true }).click();
    await ui('campaign-impact').getByText(/php:tax\\quote · 1 处调用/i).waitFor();
    await page.setViewportSize({ width: 390, height: 850 });
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth));
    assert.deepEqual(errors, []);
    fs.writeFileSync(output, JSON.stringify({ browser: browser.version(), mobile_width: 390, legacy_campaign: legacy, new_campaign: data.campaign_id,
      revision: data.campaign_rev, waves: data.waves.map(wave => wave.wave_id), scope_rejected_before_decode: true, page_errors: errors }, null, 2));
  } finally {
    await browser.close();
  }
}

main().catch(error => { console.error(error); process.exitCode = 1; });
