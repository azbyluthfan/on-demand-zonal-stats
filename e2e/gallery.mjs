// Screenshot gallery for docs/gallery.md: one image per data layer and view,
// taken from the running demo stack (DI Yogyakarta sample release).
//
//   make gallery        (runs in mcr.microsoft.com/playwright on the compose network)
//
// Choropleth shots run a real province query; tile shots switch the village
// results off and show live DuckDB tiles at a fixed #map=zoom/lat/lng.
import { chromium } from 'playwright';
import { mkdirSync } from 'node:fs';

const BASE = process.env.BASE_URL || 'http://lb:8080';
const OUT = process.env.SHOTS || '/work/docs/img/gallery';
mkdirSync(OUT, { recursive: true });

const browser = await chromium.launch({ args: ['--use-gl=angle', '--use-angle=swiftshader', '--enable-unsafe-swiftshader'] });
const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });
const errors = [];
page.on('pageerror', (e) => errors.push(e.message));
page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });

let loads = 0;
const load = async (hash = '') => {
  // A unique query string forces a full reload (a hash-only change would not).
  await page.goto(`${BASE}/?shot=${++loads}${hash}`, { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => document.getElementById('release').textContent !== '…'
    && document.body.dataset.mapReady === '1', null, { timeout: 60000 });
};
const run = async () => {
  await page.click('#btn-run');
  await page.waitForFunction(() => /villages ·/.test(document.getElementById('status').textContent)
    || !document.getElementById('error').hidden, null, { timeout: 60000 });
  if (!(await page.isHidden('#error'))) throw new Error(await page.textContent('#error'));
};
const view = async (id) => { await page.selectOption('#sel-view', id); await page.waitForTimeout(800); };
const layer = async (id, on = true) => {
  const sel = `#map-layers [data-ml="${id}"]`;
  if (on) await page.check(sel); else await page.uncheck(sel);
};
// Wait until tiles stop arriving (the live tile line stops changing).
const tilesSettled = async () => {
  let prev = '';
  for (let i = 0; i < 40; i++) {
    await page.waitForTimeout(750);
    const cur = await page.textContent('#tile-stats');
    if (cur && cur === prev) return cur;
    prev = cur;
  }
  return prev;
};
const shot = async (name) => {
  await page.screenshot({ path: `${OUT}/${name}.jpg`, type: 'jpeg', quality: 82 });
  console.log(`✓ ${name}.jpg`);
};

try {
  // ---- Choropleths: per-village results for the whole province ----------
  await load();
  for (const id of ['nl-on', 'lc-on', 'bld-on']) await page.check(`#${id}`);
  await run();
  await page.click('#diag-toggle'); // collapse diagnostics: the map is the subject
  await page.waitForTimeout(2500);
  for (const [v, name] of [
    ['nl', 'choropleth-nightlights'],
    ['lc_dominant', 'choropleth-landcover-dominant'],
    ['lc_50', 'choropleth-landcover-builtup'],
    ['bld_share', 'choropleth-building-footprint'],
    ['people_per_bld', 'choropleth-people-per-building'],
    ['pop', 'choropleth-population'],
  ]) {
    await view(v);
    await page.waitForTimeout(1200);
    await shot(name);
  }

  // ---- Live tiles: each layer itself, rendered per request by DuckDB -----
  const tiles = [
    // [name, #map=zoom/lat/lng, layers to switch on]
    ['tiles-landcover', '#map=11/-7.83/110.38', ['r_landcover_2021', 'villages']],
    ['tiles-nightlights', '#map=10/-7.85/110.40', ['r_nightlights_2023', 'villages']],
    ['tiles-population', '#map=12.5/-7.80/110.37', ['r_population_2020', 'villages']],
    ['tiles-h3-split', '#map=14.5/-7.7955/110.372', ['h3', 'villages']],
    ['tiles-buildings', '#map=15.5/-7.7955/110.372', ['buildings', 'villages']],
    ['tiles-pois', '#map=15/-7.7925/110.3675', ['pois', 'villages']],
  ];
  for (const [name, hash, ids] of tiles) {
    await load(hash);
    await layer('choropleth', false);
    for (const id of ids) await layer(id);
    // POI tiles follow the selected categories; un-press the default preset to show all.
    for (const b of await page.$$('#presets button[aria-pressed="true"]')) await b.click();
    const stats = await tilesSettled();
    // Show the layer list and the live DuckDB tile line in the panel.
    await page.$eval('#tile-stats', (el) => el.scrollIntoView({ block: 'end' }));
    await page.waitForTimeout(1500); // fade-in
    await shot(name);
    console.log(`  ${stats}`);
  }
  if (errors.length) throw new Error('console errors: ' + errors.join(' | '));
} catch (e) {
  console.error(`✗ ${e.message}`);
  process.exitCode = 1;
} finally {
  await browser.close();
}
