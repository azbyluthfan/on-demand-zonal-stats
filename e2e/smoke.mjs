// Frontend smoke test (Playwright; checklist in docs/frontend.md). Also refreshes
// the README screenshots in docs/img/.
//
//   make e2e            (runs in mcr.microsoft.com/playwright on the compose network)
//
// Exits non-zero on the first failed check.
import { chromium } from 'playwright';
import { mkdirSync } from 'node:fs';

const BASE = process.env.BASE_URL || 'http://lb:8080';
const OUT = process.env.SHOTS || '/work/docs/img';
const SHOTS = process.env.NO_SHOTS ? null : OUT;
mkdirSync(OUT, { recursive: true });

const browser = await chromium.launch({ args: ['--use-gl=angle', '--use-angle=swiftshader', '--enable-unsafe-swiftshader'] });
const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });
const errors = [];
page.on('pageerror', (e) => errors.push(e.message));
page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });

let step = 0;
const ok = (cond, msg) => {
  step++;
  if (!cond) { console.error(`✗ ${step}. ${msg}`); process.exitCode = 1; throw new Error(msg); }
  console.log(`✓ ${step}. ${msg}`);
};
const villages = async () => Number((await page.textContent('#status')).match(/^(\d+) villages/)?.[1] || -1);
const run = async () => {
  await page.click('#btn-run');
  await page.waitForFunction(() => /villages ·/.test(document.getElementById('status').textContent)
    || !document.getElementById('error').hidden, null, { timeout: 30000 });
  ok(await page.isHidden('#error'), 'query ran without an error message');
};
const settle = async () => { await page.waitForTimeout(2500); }; // tiles + fit animation
const shot = async (name) => { if (SHOTS) await page.screenshot({ path: `${SHOTS}/${name}.png` }); };

try {
  await page.goto(BASE, { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => document.getElementById('release').textContent !== '…', null, { timeout: 30000 });
  ok(/\S+/.test(await page.textContent('#release')), `release badge shows ${await page.textContent('#release')}`);
  await page.waitForFunction(() => document.body.dataset.mapReady === '1', null, { timeout: 60000 });
  ok(true, 'map style loaded (WebGL)');

  // 1. Area picker loads codes.
  const regencies = await page.$$eval('#sel-regency option', (o) => o.length - 1);
  ok(regencies > 0, `area picker lists ${regencies} regencies`);

  // 2. Province, default metrics (cafés + population) -> choropleth.
  await run();
  const n = await villages();
  ok(n > 0, `province query returned ${n} villages`);
  ok(await page.isVisible('#legend'), 'legend visible');
  ok(await page.isVisible('#diag'), 'diagnostics panel visible');
  const diagText = await page.textContent('#diag-body');
  ok(/Exact ST_Covers tests/.test(diagText) && /COG blocks read/.test(diagText), 'diagnostics show POI and raster sections');
  await settle();
  await shot('choropleth');

  // 3. Legend matches values: max tick equals the top village value.
  const ticks = await page.$$eval('#legend .ticks span', (s) => s.map((x) => x.textContent));
  const topVal = await page.textContent('#top tbody tr:first-child td.num');
  ok(ticks[ticks.length - 1] === topVal, `legend max (${ticks[ticks.length - 1]}) equals top village value (${topVal})`);

  // 4. Accuracy view.
  await page.selectOption('#sel-view', 'accuracy_pct');
  await page.waitForTimeout(500);
  ok(/Accuracy/.test(await page.textContent('#legend h3')), 'accuracy view legend shown');
  await settle();
  await shot('accuracy');

  // 5. Regency (Kota Yogyakarta, if present) and a popup.
  const kota = await page.$('#sel-regency option[value="3471"]');
  await page.selectOption('#sel-regency', kota ? '3471' : { index: 1 });
  await page.selectOption('#sel-view', 'pois');
  await run();
  ok((await villages()) > 0, `regency query returned ${await villages()} villages`);
  await settle();
  const box = await page.locator('#map').boundingBox();
  await page.mouse.click(box.x + box.width / 2, box.y + box.height / 2);
  await page.waitForTimeout(500);
  ok(await page.isVisible('.maplibregl-popup'), 'clicking a village opens a popup');
  await shot('popup');
  await page.keyboard.press('Escape');

  // 6. Draw a polygon (with human-speed clicks) and run it.
  await page.click('#tab-draw');
  await page.click('#btn-draw');
  const cx = box.x + box.width / 2, cy = box.y + box.height / 2, d = Math.min(box.width, box.height) / 6;
  for (const [x, y] of [[cx - d, cy - d], [cx + d, cy - d], [cx + d, cy + d], [cx - d, cy + d], [cx - d, cy - d]]) {
    await page.mouse.click(x, y);
    await page.waitForTimeout(400);
  }
  ok(/Polygon ready/.test(await page.textContent('#draw-status')), 'polygon drawn and closed');
  await run();
  ok((await villages()) > 0, `drawn polygon returned ${await villages()} villages`);
  await settle();
  await shot('draw');

  ok(errors.length === 0, `no console errors${errors.length ? ': ' + errors.join(' | ') : ''}`);
} catch (e) {
  if (!process.exitCode) { console.error(e); process.exitCode = 1; }
} finally {
  await browser.close();
}
