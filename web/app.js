/* Village Stats frontend: plain JS, no build step. Talks to /v1/*. */
(() => {
  'use strict';

  const $ = (id) => document.getElementById(id);
  const fmt = new Intl.NumberFormat('en', { maximumFractionDigits: 1 });
  const fmt0 = new Intl.NumberFormat('en', { maximumFractionDigits: 0 });
  const fmtSmart = (v) => (v == null || !isFinite(v) ? '–' : Math.abs(v) >= 100 ? fmt0.format(v) : fmt.format(v));

  const SEQ = ['#ffffcc', '#c7e9b4', '#7fcdbb', '#41b6c4', '#2c7fb8', '#253494'];
  const DIV = ['#2166ac', '#67a9cf', '#d1e5f0', '#f7f7f7', '#fddbc7', '#ef8a62', '#b2182b'];
  const NODATA = '#d0d4d9';
  const STAGE_COLORS = {
    resolve_area: '#8c96a3', reference: '#b48ead', h3_cells: '#5e81ac', poi_scan: '#88c0d0', poi_interior: '#a3be8c',
    poi_boundary: '#ebcb8b', building_cells: '#8fbcbb', building_boundary: '#d8b66a', raster_geometry: '#d08770', raster_read: '#bf616a', raster_coverage: '#e5a0a8', assemble: '#4c566a',
  };

  const PRESETS = [
    { label: 'Cafés', cats: ['cafe', 'coffee_shop'] },
    { label: 'Schools', cats: ['school', 'elementary_school', 'middle_school', 'high_school', 'preschool'] },
    { label: 'Places of worship', cats: ['muslim_place_of_worship', 'christian_place_of_worship', 'roman_catholic_place_of_worship', 'hindu_place_of_worship', 'buddhist_place_of_worship', 'place_of_worship'] },
    { label: 'Restaurants', cats: ['restaurant'] },
    { label: 'Health', cats: ['hospital', 'clinic', 'doctor', 'pharmacy', 'community_health_center'] },
    { label: 'Shops', cats: ['convenience_store', 'grocery_store', 'supermarket'] },
  ];

  const state = {
    layers: null, areas: [], categories: new Map(), selected: new Set(),
    mode: 'admin', drawn: null, result: null, rasterId: null, nl: null, lc: null, hasBuildings: false, draw: null, map: null, hoverId: null, drawFinishedAt: -1e9,
  };

  // ---------- API ----------
  async function api(path, opts = {}) {
    const res = await fetch(path, opts);
    let body = null;
    try { body = await res.json(); } catch (_) { /* non-JSON */ }
    if (!res.ok) {
      const err = new Error((body && body.message) || `HTTP ${res.status}`);
      err.status = res.status; err.body = body;
      throw err;
    }
    return body;
  }

  // ---------- init ----------
  async function init() {
    initMap();
    try {
      const [layers, areas] = await Promise.all([api('/v1/layers'), api('/v1/areas')]);
      state.layers = layers;
      state.areas = areas.areas;
      layers.categories.forEach((c) => state.categories.set(c.category, c.count));
      // Layer roles from the release: population = the continuous layer in
      // people; night lights = another continuous layer; land cover = the
      // first categorical layer.
      const cont = layers.rasters.filter((r) => r.kind !== 'categorical');
      const pop = cont.find((r) => r.units === 'people') || null;
      state.rasterId = pop ? pop.id : null;
      state.nl = cont.find((r) => r !== pop) || null;
      state.lc = layers.rasters.find((r) => r.kind === 'categorical') || null;
      state.hasBuildings = !!layers.buildings;
      if (state.nl) { $('nl-row').hidden = false; $('nl-label').textContent = `${labelOf(state.nl)} (mean, ${state.nl.units})`; }
      if (state.lc) { $('lc-row').hidden = false; $('lc-label').textContent = `${labelOf(state.lc)} (class shares)`; }
      if (state.hasBuildings) $('bld-row').hidden = false;
      $('release').textContent = layers.release_id;
      $('release').title = `Release ${layers.release_id} · boundaries ${layers.boundary_version} · POIs ${layers.poi_release} · H3 res ${layers.h3_resolution}`;
      if (state.rasterId) {
        const r = pop;
        $('pop-label').textContent = `${r.id.replace(/_/g, ' ')} (${r.units})`;
      } else {
        $('pop-on').checked = false; $('pop-on').disabled = true;
      }
      fillAreas();
      fillMapLayers();
      fillPresets();
      fillAttribution();
      togglePreset(PRESETS[0]); // a sensible default: cafés + population
      updateViewOptions();
    } catch (e) {
      showError(e.status === 503 ? 'The API is starting or storage is unavailable. Retry in a moment.' : e.message);
    }
    wire();
  }

  function initMap() {
    const map = new maplibregl.Map({
      container: 'map',
      style: 'https://tiles.openfreemap.org/styles/positron',
      center: [110.4, -7.85], zoom: 9.2, attributionControl: { compact: true },
      hash: 'map', // #map=zoom/lat/lng in the URL: shareable views (and scripted screenshots)
    });
    map.addControl(new maplibregl.NavigationControl({ showCompass: false }), 'top-right');
    map.addControl(new maplibregl.ScaleControl(), 'bottom-right');
    state.map = map;
    map.on('load', () => {
      const firstSymbol = map.getStyle().layers.find((l) => l.type === 'symbol');
      const before = firstSymbol ? firstSymbol.id : undefined;
      map.addSource('villages', { type: 'geojson', data: { type: 'FeatureCollection', features: [] }, promoteId: 'village_id' });
      map.addLayer({ id: 'v-fill', type: 'fill', source: 'villages', paint: { 'fill-color': NODATA, 'fill-opacity': 0.78 } }, before);
      map.addLayer({ id: 'v-line', type: 'line', source: 'villages', paint: { 'line-color': '#ffffff', 'line-width': 0.6 } }, before);
      map.addLayer({
        id: 'v-hover', type: 'line', source: 'villages',
        paint: { 'line-color': '#1f2328', 'line-width': ['case', ['boolean', ['feature-state', 'hover'], false], 2, 0] },
      }, before);
      map.on('mousemove', 'v-fill', (e) => {
        if (!e.features.length) return;
        setHover(e.features[0].id);
        map.getCanvas().style.cursor = 'pointer';
      });
      map.on('mouseleave', 'v-fill', () => { setHover(null); map.getCanvas().style.cursor = ''; });
      map.on('click', 'v-fill', (e) => {
        // No popups while drawing, nor for the click that closes a polygon.
        if (isDrawing() || performance.now() - state.drawFinishedAt < 400) return;
        if (layerPopup(e, false)) return; // a POI or building under the cursor wins (map click handler)
        showPopup(e.features[0], e.lngLat);
      });
      initDraw();
      state.mapLoaded = true;
      addMapLayers();
      document.body.dataset.mapReady = '1';
      // A query may have finished before the style loaded (slow network/GPU).
      if (state.result) render(true);
    });
  }

  function setHover(id) {
    const map = state.map;
    if (state.hoverId != null) map.setFeatureState({ source: 'villages', id: state.hoverId }, { hover: false });
    state.hoverId = id;
    if (id != null) map.setFeatureState({ source: 'villages', id }, { hover: true });
  }

  // ---------- drawing ----------
  function initDraw() {
    if (!window.terraDraw || !window.terraDrawMaplibreGlAdapter) return;
    const draw = new terraDraw.TerraDraw({
      adapter: new terraDrawMaplibreGlAdapter.TerraDrawMapLibreGLAdapter({ map: state.map }),
      modes: [new terraDraw.TerraDrawPolygonMode({
        styles: { fillColor: '#2459d6', fillOpacity: 0.08, outlineColor: '#2459d6', outlineWidth: 2 },
      })],
    });
    draw.start();
    draw.on('finish', (id) => {
      state.drawFinishedAt = performance.now();
      const f = draw.getSnapshot().find((x) => x.id === id);
      if (f) {
        state.drawn = f.geometry;
        $('draw-status').textContent = `Polygon ready (${f.geometry.coordinates[0].length - 1} vertices). Press Run query.`;
      }
      draw.setMode('static');
    });
    state.draw = draw;
  }
  const isDrawing = () => state.draw && state.draw.getMode() === 'polygon';

  // ---------- controls ----------
  function fillAreas() {
    const provinces = state.areas.filter((a) => a.level === 'province');
    $('sel-province').innerHTML = provinces.map((p) => `<option value="${esc(p.code)}">${esc(p.name)} (${p.villages})</option>`).join('');
    onProvince();
  }
  function onProvince() {
    const p = $('sel-province').value;
    const regs = state.areas.filter((a) => a.level === 'regency' && a.parent === p);
    $('sel-regency').innerHTML = '<option value="">All regencies</option>' +
      regs.map((r) => `<option value="${esc(r.code)}">${esc(r.name)} (${r.villages})</option>`).join('');
    onRegency();
  }
  function onRegency() {
    const r = $('sel-regency').value;
    const ds = r ? state.areas.filter((a) => a.level === 'district' && a.parent === r) : [];
    $('sel-district').innerHTML = '<option value="">All districts</option>' +
      ds.map((d) => `<option value="${esc(d.code)}">${esc(d.name)} (${d.villages})</option>`).join('');
    $('sel-district').disabled = !r;
  }

  function fillPresets() {
    const box = $('presets');
    box.innerHTML = '';
    PRESETS.forEach((p) => {
      p.cats = p.cats.filter((c) => state.categories.has(c));
      if (!p.cats.length) return;
      const b = document.createElement('button');
      b.className = 'chip'; b.type = 'button'; b.textContent = p.label;
      b.title = p.cats.join(', ');
      b.setAttribute('aria-pressed', 'false');
      b.addEventListener('click', () => togglePreset(p));
      p.button = b;
      box.appendChild(b);
    });
  }
  function togglePreset(p) {
    const on = p.cats.every((c) => state.selected.has(c));
    p.cats.forEach((c) => (on ? state.selected.delete(c) : state.selected.add(c)));
    renderSelected();
  }
  function renderSelected() {
    PRESETS.forEach((p) => p.button && p.button.setAttribute('aria-pressed', String(p.cats.length > 0 && p.cats.every((c) => state.selected.has(c)))));
    const box = $('cat-selected');
    box.innerHTML = '';
    [...state.selected].sort().forEach((c) => {
      const b = document.createElement('button');
      b.className = 'chip'; b.type = 'button'; b.textContent = c.replace(/_/g, ' ');
      b.title = `Remove ${c}`;
      b.addEventListener('click', () => { state.selected.delete(c); renderSelected(); });
      box.appendChild(b);
    });
    updateViewOptions();
    updatePoiTiles();
  }
  function searchCategories() {
    const q = $('cat-search').value.trim().toLowerCase().replace(/\s+/g, '_');
    const list = $('cat-results');
    if (!q) { list.hidden = true; return; }
    const hits = [...state.categories.entries()].filter(([c]) => c.includes(q)).slice(0, 40);
    list.innerHTML = hits.length
      ? hits.map(([c, n]) => `<li data-c="${esc(c)}" class="${state.selected.has(c) ? 'on' : ''}"><span>${esc(c.replace(/_/g, ' '))}</span><span class="n">${fmt0.format(n)}</span></li>`).join('')
      : '<li><span class="hint">No matching category</span></li>';
    list.hidden = false;
  }

  function fillAttribution() {
    const s = (state.layers.sources || []).map((x) => {
      const name = x.url ? `<a href="${esc(x.url)}" target="_blank" rel="noopener">${esc(x.name)}</a>` : esc(x.name);
      return `${name}: ${esc(x.license)}${x.attribution ? ` · ${esc(x.attribution)}` : ''}`;
    });
    $('attribution').innerHTML = s.join('<br>') + '<br>Basemap: OpenFreeMap, © OpenStreetMap contributors';
  }

  const labelOf = (r) => r.id.replace(/_/g, ' ').replace(/^./, (c) => c.toUpperCase());
  const on = (id) => $(id).checked && !$(id).disabled;

  // Views available given the current selections: [id, label, kind]. Kinds:
  // count (per-km² toggle), rate, value, percent, diverging, categorical.
  function views() {
    const hasPoi = state.selected.size > 0;
    const hasPop = on('pop-on') && state.rasterId;
    const hasNl = state.nl && on('nl-on');
    const hasLc = state.lc && on('lc-on');
    const hasBld = state.hasBuildings && on('bld-on');
    const v = [];
    if (hasPoi) v.push(['pois', 'POI count', 'count']);
    if (hasPop) v.push(['pop', 'Population', 'count']);
    if (hasPoi && hasPop) v.push(['pois_per_1k', 'POIs per 1,000 people', 'rate']);
    if (hasNl) v.push(['nl', `${labelOf(state.nl)}: mean radiance (${state.nl.units})`, 'value']);
    if (hasNl && hasPop) v.push(['nl_per_1k', `${labelOf(state.nl)}: radiance × km² per 1,000 people`, 'rate']);
    if (hasBld) {
      v.push(['bld', 'Buildings', 'count']);
      v.push(['bld_share', 'Building footprint (% of village area)', 'percent']);
      v.push(['bld_size', 'Mean building footprint (m²)', 'value']);
    }
    if (hasBld && hasPop) v.push(['people_per_bld', 'People per building', 'rate']);
    if (hasLc) {
      v.push(['lc_dominant', `${labelOf(state.lc)}: dominant class`, 'categorical']);
      state.lc.classes.forEach((c) => v.push([`lc_${c.value}`, `${labelOf(state.lc)}: ${c.label} (% of area)`, 'percent']));
    }
    if (hasPop) {
      v.push(['accuracy', 'Accuracy: centre − fractional (people)', 'diverging']);
      v.push(['accuracy_pct', 'Accuracy: centre vs fractional (%)', 'diverging']);
    }
    return v;
  }
  function updateViewOptions() {
    const sel = $('sel-view');
    const cur = sel.value;
    const vs = views();
    sel.innerHTML = vs.map(([id, label]) => `<option value="${id}">${label}</option>`).join('');
    if (vs.some(([id]) => id === cur)) sel.value = cur;
    $('per-km2').disabled = !(vs.find(([id]) => id === sel.value) || [])[2] || vs.find(([id]) => id === sel.value)[2] !== 'count';
    $('btn-run').disabled = vs.length === 0;
  }

  // ---------- query ----------
  function buildRequest() {
    let area;
    if (state.mode === 'draw') {
      if (!state.drawn) throw new Error('Draw a polygon first.');
      area = { geometry: state.drawn };
    } else if ($('sel-district').value) area = { district_code: $('sel-district').value };
    else if ($('sel-regency').value) area = { regency_code: $('sel-regency').value };
    else area = { province_code: $('sel-province').value };

    const metrics = [];
    if (state.selected.size) metrics.push({ id: 'pois', type: 'poi_count', categories: [...state.selected].sort() });
    if (on('pop-on') && state.rasterId) {
      // Both methods: the accuracy view needs them and the second costs little.
      metrics.push({ id: 'pop_fractional', type: 'raster_sum', layer: state.rasterId, method: 'fractional' });
      metrics.push({ id: 'pop_center', type: 'raster_sum', layer: state.rasterId, method: 'center' });
    }
    if (state.nl && on('nl-on')) {
      metrics.push({ id: 'nl_mean', type: 'raster_mean', layer: state.nl.id, method: 'fractional' });
    }
    if (state.lc && on('lc-on')) {
      // One fraction per class: dominant-class and per-class views from one request.
      state.lc.classes.forEach((c) => metrics.push({
        id: `lc_${c.value}`, type: 'raster_class_fraction', layer: state.lc.id, method: 'fractional', classes: [c.value],
      }));
    }
    if (state.hasBuildings && on('bld-on')) {
      metrics.push({ id: 'bld_n', type: 'building_count' }, { id: 'bld_area', type: 'building_area' });
    }
    if (!metrics.length) throw new Error('Choose at least one POI category or layer.');
    return { area, metrics, include_geometry: true, diagnostics: true };
  }

  async function run() {
    hideError();
    let req;
    try { req = buildRequest(); } catch (e) { showError(e.message); return; }
    $('btn-run').disabled = true;
    $('status').textContent = 'Running…';
    const t0 = performance.now();
    try {
      const headers = { 'Content-Type': 'application/json' };
      if ($('no-cache').checked) headers['Cache-Control'] = 'no-cache';
      const fc = await api('/v1/aggregate', { method: 'POST', headers, body: JSON.stringify(req) });
      const ms = performance.now() - t0;
      state.result = fc;
      $('status').textContent = `${fc.summary.village_count} villages · ${fmt0.format(ms)} ms round trip`;
      render(true);
      renderDiagnostics(fc.diagnostics, ms);
    } catch (e) {
      $('status').textContent = '';
      const fields = e.body && e.body.fields ? '\n' + e.body.fields.map((f) => `• ${f.field}: ${f.message}`).join('\n') : '';
      const hint = e.status === 413 ? '\nTip: pick a regency or district, or draw a smaller polygon.' : '';
      showError(`${e.message}${fields}${hint}`);
    } finally {
      $('btn-run').disabled = false;
    }
  }

  // ---------- rendering ----------
  function popMethod() { return document.querySelector('input[name=method]:checked').value; }

  function valueOf(p, view, perKm2) {
    const m = p.metrics || {};
    const pop = m['pop_' + popMethod()];
    let v;
    switch (view) {
      case 'pois': v = m.pois; break;
      case 'pop': v = pop; break;
      case 'pois_per_1k': v = pop > 0 ? (1000 * m.pois) / pop : null; break;
      case 'accuracy': v = m.pop_center - m.pop_fractional; break;
      case 'accuracy_pct': v = m.pop_fractional > 0 ? (100 * (m.pop_center - m.pop_fractional)) / m.pop_fractional : null; break;
      case 'nl': v = m.nl_mean; break;
      // Mean radiance × area ≈ total light; per 1,000 people.
      case 'nl_per_1k': v = m.nl_mean != null && pop > 0 ? (1000 * m.nl_mean * (p.area_m2 / 1e6)) / pop : null; break;
      case 'bld': v = m.bld_n; break;
      case 'bld_share': v = p.area_m2 > 0 ? (100 * m.bld_area) / p.area_m2 : null; break;
      case 'bld_size': v = m.bld_n > 0 ? m.bld_area / m.bld_n : null; break;
      case 'people_per_bld': v = m.bld_n > 0 ? pop / m.bld_n : null; break;
      case 'lc_dominant': v = dominantClass(m); break;
      default:
        if (view.startsWith('lc_')) v = m[view] == null ? null : 100 * m[view];
        else v = null;
    }
    if (v != null && perKm2 && (view === 'pois' || view === 'pop' || view === 'bld')) v /= p.area_m2 / 1e6;
    return v == null || !isFinite(v) ? null : v;
  }

  // The land-cover class with the largest share (null when no share is known).
  function dominantClass(m) {
    let best = null; let share = -1;
    (state.lc ? state.lc.classes : []).forEach((c) => {
      const f = m[`lc_${c.value}`];
      if (f != null && f > share) { share = f; best = c.value; }
    });
    return share > 0 ? best : null;
  }

  function quantileBreaks(vals, k) {
    const s = vals.filter((v) => v != null).sort((a, b) => a - b);
    if (!s.length) return [];
    const out = [];
    for (let i = 1; i < k; i++) {
      const q = s[Math.min(s.length - 1, Math.floor((i * s.length) / k))];
      if (!out.length || q > out[out.length - 1]) out.push(q);
    }
    return out.filter((b) => b > s[0]); // first class starts at the minimum
  }
  function divergingBreaks(vals) {
    const a = vals.filter((v) => v != null).map(Math.abs).sort((x, y) => x - y);
    if (!a.length || a[a.length - 1] === 0) return [];
    const q = (p) => a[Math.min(a.length - 1, Math.floor(p * a.length))];
    const t = [...new Set([q(0.4), q(0.7), q(0.9)])].filter((x) => x > 0).sort((x, y) => x - y);
    return [...t.slice().reverse().map((x) => -x), ...t];
  }

  function render(fit) {
    const fc = state.result;
    if (!fc || !state.map.getSource('villages')) return;
    const view = $('sel-view').value;
    const kind = (views().find(([id]) => id === view) || [])[2];
    const perKm2 = $('per-km2').checked && kind === 'count';
    const feats = fc.features.map((f) => {
      const v = valueOf(f.properties, view, perKm2);
      return { type: 'Feature', id: f.id, geometry: f.geometry, properties: { village_id: f.id, _v: v } };
    });
    const vals = feats.map((f) => f.properties._v);

    let colors; let breaks;
    if (kind === 'categorical') {
      const match = ['match', ['get', '_v']];
      state.lc.classes.forEach((c) => match.push(c.value, c.color || '#999'));
      match.push(NODATA);
      state.map.getSource('villages').setData({ type: 'FeatureCollection', features: feats });
      state.map.setPaintProperty('v-fill', 'fill-color', ['case', ['==', ['get', '_v'], null], NODATA, match]);
      renderClassLegend(view, vals);
      renderSummary(view, perKm2);
      if (fit && fc.features.length) fitTo(fc.features);
      return;
    }
    if (kind === 'diverging') {
      breaks = divergingBreaks(vals);
      const mid = Math.floor(DIV.length / 2);
      const n = breaks.length / 2;
      colors = DIV.slice(mid - n, mid + n + 1);
    } else {
      breaks = quantileBreaks(vals, SEQ.length);
      colors = SEQ.slice(SEQ.length - breaks.length - 1);
    }
    const step = ['step', ['get', '_v'], colors[0]];
    breaks.forEach((b, i) => step.push(b, colors[i + 1]));
    const paint = breaks.length || colors.length ? ['case', ['==', ['get', '_v'], null], NODATA, step] : NODATA;

    state.map.getSource('villages').setData({ type: 'FeatureCollection', features: feats });
    state.map.setPaintProperty('v-fill', 'fill-color', paint);
    renderLegend(view, kind, perKm2, breaks, colors, vals);
    renderSummary(view, perKm2);
    if (fit && fc.features.length) fitTo(fc.features);
  }

  function renderClassLegend(view, vals) {
    const label = (views().find(([id]) => id === view) || [, ''])[1];
    const counts = new Map();
    vals.forEach((v) => v != null && counts.set(v, (counts.get(v) || 0) + 1));
    const rows = state.lc.classes.filter((c) => counts.has(c.value)).map((c) =>
      `<span class="sw" style="background:${esc(c.color || '#999')}"></span><span>${esc(c.label)}</span><span class="n">${counts.get(c.value)}</span>`).join('');
    const nulls = vals.filter((v) => v == null).length;
    $('legend').innerHTML = `<h3>${esc(label)}</h3><div class="classes">${rows}</div><div class="note">Villages per class; fractional coverage.</div>` +
      (nulls ? `<div class="note">no value (${nulls})</div>` : '');
    $('legend').hidden = false;
  }

  function renderLegend(view, kind, perKm2, breaks, colors, vals) {
    const label = (views().find(([id]) => id === view) || [, ''])[1] + (perKm2 ? ' per km²' : '');
    const present = vals.filter((v) => v != null);
    const lo = present.length ? Math.min(...present) : 0;
    const hi = present.length ? Math.max(...present) : 0;
    const unit = kind === 'percent' ? '%' : '';
    const ticks = [lo, ...breaks, hi].map((x) => fmtSmart(x) + unit);
    const note = kind === 'diverging'
      ? '<div class="note">Red: counting whole pixels by their centre gives more people than exact fractional coverage; blue: fewer.</div>'
      : ['pop', 'pois_per_1k', 'nl_per_1k', 'people_per_bld'].includes(view) ? `<div class="note">Population method: ${popMethod()}</div>`
        : view.startsWith('lc_') || view === 'nl' ? '<div class="note">Fractional pixel coverage</div>' : '';
    const nulls = vals.length - present.length;
    $('legend').innerHTML = `<h3>${esc(label)}</h3><div class="ramp">${colors.map((c) => `<span style="background:${c}"></span>`).join('')}</div>` +
      `<div class="ticks">${ticks.map((t) => `<span>${t}</span>`).join('')}</div>${note}` +
      (nulls ? `<div class="note"><span style="display:inline-block;width:9px;height:9px;background:${NODATA};border-radius:2px"></span> no value (${nulls})</div>` : '');
    $('legend').hidden = false;
  }

  function renderSummary(view, perKm2) {
    const fc = state.result;
    const t = fc.summary.totals;
    const rows = [['Villages', fc.summary.village_count]];
    if ('pois' in t) rows.push(['POIs', t.pois]);
    if ('pop_fractional' in t) rows.push(['Population (fractional)', t.pop_fractional], ['Population (pixel centre)', t.pop_center]);
    if ('nl_mean' in t) rows.push([`${labelOf(state.nl)}, mean (${state.nl.units})`, t.nl_mean]);
    if ('bld_n' in t) {
      const area = fc.features.reduce((s, f) => s + f.properties.area_m2, 0);
      rows.push(['Buildings', t.bld_n], ['Building footprint (km²)', t.bld_area / 1e6]);
      if (area > 0) rows.push(['Footprint share of area (%)', (100 * t.bld_area) / area]);
    }
    if (state.lc && `lc_${state.lc.classes[0].value}` in t) {
      state.lc.classes
        .map((c) => [c.label, t[`lc_${c.value}`]])
        .filter(([, f]) => f != null && f >= 0.005)
        .sort((a, b) => b[1] - a[1])
        .forEach(([l, f]) => rows.push([`${l} (% of area)`, 100 * f]));
    }
    if (fc.diagnostics && fc.diagnostics.unassigned_pois && 'pois' in fc.diagnostics.unassigned_pois) {
      rows.push(['POIs in no village', fc.diagnostics.unassigned_pois.pois]);
    }
    $('summary').innerHTML = rows.map(([k, v]) => `<span>${k}</span><span class="v">${fmtSmart(v)}</span>`).join('');

    const classLabel = (v) => ((state.lc && state.lc.classes.find((c) => c.value === v)) || {}).label || '–';
    const cat = view === 'lc_dominant';
    const ranked = fc.features
      .map((f) => ({ f, v: cat ? dominantShare(f.properties.metrics) : valueOf(f.properties, view, perKm2), c: cat ? dominantClass(f.properties.metrics) : null }))
      .filter((x) => x.v != null)
      .sort((a, b) => (view.startsWith('accuracy') ? Math.abs(b.v) - Math.abs(a.v) : b.v - a.v))
      .slice(0, 10);
    const top = $('top');
    top.querySelector('caption').textContent = view.startsWith('accuracy') ? 'Largest centre-vs-fractional gaps'
      : cat ? 'Most uniform villages (dominant class share)' : 'Top villages';
    top.querySelector('thead').innerHTML = '<tr><th>Village</th><th>District</th><th class="num">Value</th></tr>';
    top.querySelector('tbody').innerHTML = ranked.map(({ f, v, c }) =>
      `<tr data-id="${esc(f.id)}"><td>${esc(f.properties.village_name)}</td><td>${esc(f.properties.district_name)}</td>` +
      `<td class="num">${cat ? `${esc(classLabel(c))} ${fmtSmart(v)}%` : fmtSmart(v)}</td></tr>`).join('');
    $('summary-block').hidden = false;
  }

  function dominantShare(m) {
    const c = dominantClass(m);
    return c == null ? null : 100 * m[`lc_${c}`];
  }

  function showPopup(feature, lngLat) {
    const f = state.result.features.find((x) => x.id === feature.id);
    if (!f) return;
    const p = f.properties; const m = p.metrics;
    const rows = [];
    if ('pois' in m) rows.push(['POIs', m.pois]);
    if ('pop_fractional' in m) {
      rows.push(['Population (fractional)', m.pop_fractional], ['Population (pixel centre)', m.pop_center]);
      rows.push(['Centre − fractional', m.pop_center - m.pop_fractional]);
    }
    if ('pois' in m && 'pop_fractional' in m && m.pop_fractional > 0) rows.push(['POIs per 1,000 people', (1000 * m.pois) / m['pop_' + popMethod()]]);
    if ('nl_mean' in m) rows.push([`${labelOf(state.nl)}, mean (${state.nl.units})`, m.nl_mean]);
    if ('bld_n' in m) {
      rows.push(['Buildings', m.bld_n], ['Footprint (% of area)', p.area_m2 > 0 ? (100 * m.bld_area) / p.area_m2 : null]);
      if (m.bld_n > 0) rows.push(['Mean footprint (m²)', m.bld_area / m.bld_n]);
    }
    if (state.lc) {
      state.lc.classes
        .filter((c) => m[`lc_${c.value}`] != null && m[`lc_${c.value}`] >= 0.005)
        .sort((a, b) => m[`lc_${b.value}`] - m[`lc_${a.value}`])
        .forEach((c) => rows.push([`${c.label} (%)`, 100 * m[`lc_${c.value}`]]));
    }
    rows.push(['Area (km²)', p.area_m2 / 1e6]);
    new maplibregl.Popup({ closeButton: true, maxWidth: '300px' })
      .setLngLat(lngLat)
      .setHTML(`<h3>${esc(p.village_name)}</h3><div class="sub">${esc(p.district_name)}, ${esc(p.regency_name)} · ${esc(p.village_id)}</div>` +
        `<table>${rows.map(([k, v]) => `<tr><td>${k}</td><td class="num">${fmtSmart(v)}</td></tr>`).join('')}</table>`)
      .addTo(state.map);
  }

  function bboxOf(features) {
    let b = [Infinity, Infinity, -Infinity, -Infinity];
    const visit = (c) => {
      if (typeof c[0] === 'number') { b = [Math.min(b[0], c[0]), Math.min(b[1], c[1]), Math.max(b[2], c[0]), Math.max(b[3], c[1])]; return; }
      c.forEach(visit);
    };
    features.forEach((f) => f.geometry && visit(f.geometry.coordinates));
    return b;
  }
  function fitTo(features) {
    const b = bboxOf(features);
    if (isFinite(b[0])) state.map.fitBounds([[b[0], b[1]], [b[2], b[3]]], { padding: 40, duration: 600 });
  }

  function renderDiagnostics(d, roundTrip) {
    if (!d) return;
    const stages = Object.entries(d.stages_ms).filter(([, v]) => v > 0.05).sort((a, b) => order(a[0]) - order(b[0]));
    const sum = stages.reduce((s, [, v]) => s + v, 0) || 1;
    const bar = stages.map(([k, v]) => `<span title="${k}: ${fmt.format(v)} ms" style="width:${(100 * v) / sum}%;background:${STAGE_COLORS[k] || '#999'}"></span>`).join('');
    const legend = stages.map(([k, v]) => `<div class="stage"><span class="sw" style="background:${STAGE_COLORS[k] || '#999'}"></span>${k.replace(/_/g, ' ')}</div><span class="v">${fmt.format(v)} ms</span>`).join('');
    const c = d.cache || {};
    const kv = (rows) => `<div class="kv">${rows.map(([k, v]) => `<span>${k}</span><span class="v">${v}</span>`).join('')}</div>`;
    let html = `<div class="bar">${bar}</div><div class="kv">${legend}</div>`;
    html += '<h4>POI assignment</h4>' + kv([
      ['Villages computed (cache misses)', fmt0.format(d.poi_villages_computed)],
      ['Points scanned', fmt0.format(d.points_scanned)],
      ['Interior path (no geometry test)', fmt0.format(d.points_interior)],
      ['Boundary points', fmt0.format(d.points_boundary)],
      ['Exact ST_Covers tests', fmt0.format(d.points_boundary_tested)],
      ['Won by a neighbour outside the area', fmt0.format(d.points_other_village)],
    ]);
    if (d.buildings) {
      const b = d.buildings;
      html += '<h4>Buildings</h4>' + kv([
        ['Villages computed (cache misses)', fmt0.format(b.villages_computed)],
        ['Interior cells read (pre-summed)', fmt0.format(b.cell_rows)],
        ['Buildings via interior cells', fmt0.format(b.cell_buildings)],
        ['Boundary building points', fmt0.format(b.boundary_points)],
        ['Exact ST_Covers tests', fmt0.format(b.boundary_tests)],
      ]);
    }
    if (d.raster_windows) {
      html += '<h4>Raster</h4>' + kv([
        ['Village windows', fmt0.format(d.raster_windows)],
        ['COG blocks read', fmt0.format(d.raster_blocks || 0)],
        ['Pixels read', fmt0.format(d.raster_pixels_read)],
        ['Nodata weight (pixels)', fmt.format(d.raster_nodata_weight || 0)],
      ]);
    }
    html += '<h4>Cache</h4>' + kv([
      ['Results hit / miss', `${fmt0.format(c.result_hits || 0)} / ${fmt0.format(c.result_misses || 0)}`],
      ['COG blocks hit / miss', `${c.block_hits || 0} / ${c.block_misses || 0}`],
      ['Provinces hit / loaded', `${c.reference_hits || 0} / ${c.reference_misses || 0}`],
    ]);
    const prof = Object.entries(d.duckdb_profile || {}).sort((a, b) => order(a[0]) - order(b[0]));
    if (prof.length) {
      html += '<h4>DuckDB profile</h4><div class="kv">' + prof.map(([k, p]) =>
        `<span>${k.replace(/_/g, ' ')}</span><span class="v">${fmt.format(1000 * (p.latency_s || 0))} ms · ${fmt0.format(p.rows_scanned || 0)} rows</span>`).join('') + '</div>';
    }
    html += `<p class="hint">Server ${fmt.format(d.total_ms)} ms · round trip ${fmt0.format(roundTrip)} ms · partitions ${(d.partitions || []).join(', ') || '–'}</p>`;
    $('diag-body').innerHTML = html;
    $('diag-total').textContent = `${fmt.format(d.total_ms)} ms`;
    $('diag').hidden = false;
  }
  const order = (k) => { const i = Object.keys(STAGE_COLORS).indexOf(k); return i < 0 ? 99 : i; };

  // ---------- live map layers: tiles rendered per request by DuckDB ----------
  // Vector tiles: /v1/tiles/{layer}/{z}/{x}/{y}.mvt (ST_AsMVT over Parquet).
  // Raster tiles: /v1/tiles/raster/{layer}/{z}/{x}/{y}.png (cog extension).
  // Tiles load through the "duck://" protocol: a main-thread fetch that
  // reads each tile's Server-Timing header (MapLibre fetches vector tiles in
  // a worker, where the page cannot see their timing).
  const tileUrl = (tmpl, vars) => 'duck://' + tmpl.replace(/^\//, '').replace(/\{(layer)\}/g, () => vars.layer);
  const tileLog = [];
  maplibregl.addProtocol('duck', async (params, abort) => {
    // A plain fetch: cache:'no-store' would make the browser send
    // Cache-Control: no-cache, which bypasses the server tile cache. The
    // "Bypass cache" checkbox asks for that explicitly.
    const headers = $('no-cache').checked ? { 'Cache-Control': 'no-cache' } : {};
    const res = await fetch(location.origin + '/' + params.url.slice('duck://'.length), { signal: abort.signal, headers });
    if (!res.ok) throw new Error(`tile ${res.status}`);
    const st = res.headers.get('Server-Timing') || '';
    const m = st.match(/duckdb;dur=([\d.]+)/);
    const f = st.match(/features;desc="(\d+)"/);
    tileLog.push({ duckdb: m ? Number(m[1]) : null, hit: /cache;/.test(st), empty: /empty;/.test(st), features: f ? Number(f[1]) : 0,
      kind: params.url.includes('/raster/') ? 'raster' : 'vector' });
    if (tileLog.length > 500) tileLog.shift();
    renderTileStats();
    return { data: await res.arrayBuffer() };
  });
  const H3_COLORS = { interior: '#2ca25f', boundary: '#f03b20' };

  function mapLayerDefs() {
    const L = state.layers;
    const vec = new Map((L.tiles ? L.tiles.vector.layers : []).map((l) => [l.id, l]));
    const defs = [{ id: 'choropleth', label: 'Village results (choropleth)', kind: 'choropleth', opacity: 0.78, on: true }];
    if (vec.has('villages')) defs.push({ id: 'villages', label: 'Village outlines', kind: 'vector', minzoom: vec.get('villages').minzoom, opacity: 0.9 });
    if (vec.has('h3')) defs.push({ id: 'h3', label: 'H3 cells: interior / boundary', kind: 'vector', minzoom: vec.get('h3').minzoom, opacity: 0.45 });
    if (vec.has('pois')) defs.push({ id: 'pois', label: 'POIs (selected categories, else all)', kind: 'vector', minzoom: vec.get('pois').minzoom, opacity: 0.9 });
    if (vec.has('buildings')) defs.push({ id: 'buildings', label: 'Buildings: cells + boundary points', kind: 'vector', minzoom: vec.get('buildings').minzoom, opacity: 0.8 });
    (L.rasters || []).forEach((r) => defs.push({ id: 'r_' + r.id, layer: r.id, label: `${labelOf(r)} (raster)`, kind: 'raster', minzoom: L.tiles ? L.tiles.raster.minzoom : 0, opacity: 0.75 }));
    return defs;
  }

  function fillMapLayers() {
    state.mapDefs = mapLayerDefs();
    const box = $('map-layers');
    box.innerHTML = '';
    state.mapDefs.forEach((d) => {
      const row = document.createElement('div');
      row.className = 'ml';
      row.innerHTML = `<label class="check"><input type="checkbox" data-ml="${d.id}" ${d.on ? 'checked' : ''}> <span title="${esc(d.label)}">${esc(d.label)}</span></label>` +
        `<input type="range" min="0" max="1" step="0.05" value="${d.opacity}" data-mlo="${d.id}" aria-label="${esc(d.label)} opacity">` +
        (d.minzoom ? `<div class="ml-note" data-mln="${d.id}">from zoom ${d.minzoom}</div>` : '') +
        `<div class="ml-legend" data-mll="${d.id}" hidden></div>`;
      box.appendChild(row);
    });
    box.addEventListener('change', (e) => {
      const id = e.target.dataset.ml;
      if (!id) return;
      const d = state.mapDefs.find((x) => x.id === id);
      d.on = e.target.checked;
      applyMapLayer(d);
      if (d.on && d.kind === 'raster') showRasterLegend(d);
      box.querySelector(`[data-mll="${id}"]`).hidden = !d.on || !box.querySelector(`[data-mll="${id}"]`).innerHTML;
    });
    box.addEventListener('input', (e) => {
      const id = e.target.dataset.mlo;
      if (!id) return;
      const d = state.mapDefs.find((x) => x.id === id);
      d.opacity = Number(e.target.value);
      applyMapLayer(d);
    });
    addMapLayers();
  }

  // Map layers per definition: [layer id, opacity paint property, opacity factor].
  function addMapLayers() {
    if (!state.mapLoaded || !state.mapDefs || state.mapLayersAdded) return;
    state.mapLayersAdded = true;
    const map = state.map;
    const L = state.layers;
    const top = (map.getStyle().layers.find((l) => l.type === 'symbol') || {}).id; // under labels
    state.mapDefs.forEach((d) => {
      if (d.kind === 'choropleth') { d.parts = [['v-fill', 'fill-opacity', 1], ['v-line', 'line-opacity', 1]]; return; }
      const src = `ml-src-${d.id}`;
      if (d.kind === 'raster') {
        map.addSource(src, { type: 'raster', tiles: [tileUrl(L.tiles.raster.url, { layer: d.layer })], tileSize: 256, minzoom: d.minzoom, maxzoom: 15 });
        map.addLayer({ id: `ml-${d.id}`, type: 'raster', source: src, layout: { visibility: 'none' },
          paint: { 'raster-resampling': 'nearest', 'raster-fade-duration': 0 } }, 'v-fill'); // under the choropleth
        d.parts = [[`ml-${d.id}`, 'raster-opacity', 1]];
        return;
      }
      map.addSource(src, { type: 'vector', tiles: [vectorUrl(d.id)], minzoom: d.minzoom, maxzoom: 15 });
      const add = (id, spec, prop, f) => { map.addLayer({ id, source: src, layout: { visibility: 'none' }, ...spec }, top); d.parts.push([id, prop, f]); };
      d.parts = [];
      if (d.id === 'villages') {
        add('ml-villages', { type: 'line', 'source-layer': 'villages', paint: { 'line-color': '#1f2328', 'line-width': 1 } }, 'line-opacity', 1);
      } else if (d.id === 'h3') {
        add('ml-h3', { type: 'fill', 'source-layer': 'h3', paint: { 'fill-color': ['match', ['get', 'cell_type'], 'interior', H3_COLORS.interior, H3_COLORS.boundary], 'fill-outline-color': '#ffffff' } }, 'fill-opacity', 1);
      } else if (d.id === 'pois') {
        add('ml-pois', { type: 'circle', 'source-layer': 'pois', paint: { 'circle-color': '#d6336c', 'circle-radius': ['interpolate', ['linear'], ['zoom'], 13, 2, 17, 5], 'circle-stroke-color': '#ffffff', 'circle-stroke-width': 0.5 } }, 'circle-opacity', 1);
      } else if (d.id === 'buildings') {
        add('ml-bcells', { type: 'fill', 'source-layer': 'building_cells', paint: { 'fill-color': ['interpolate', ['linear'], ['get', 'buildings'], 1, '#fee6ce', 10, '#fd8d3c', 40, '#a63603'] } }, 'fill-opacity', 0.8);
        add('ml-bpoints', { type: 'circle', 'source-layer': 'building_points', paint: { 'circle-color': '#7f2704', 'circle-radius': ['interpolate', ['linear'], ['zoom'], 14, 1.2, 17, 3] } }, 'circle-opacity', 1);
      }
    });
    state.mapDefs.forEach(applyMapLayer);
    map.on('click', (e) => { if (!isDrawing()) layerPopup(e, true); });
  }

  function vectorUrl(id) {
    const base = tileUrl(state.layers.tiles.vector.url, { layer: id });
    return id === 'pois' && state.selected.size ? `${base}?categories=${[...state.selected].sort().map(encodeURIComponent).join(',')}` : base;
  }
  function updatePoiTiles() {
    const src = state.map && state.mapLayersAdded && state.map.getSource('ml-src-pois');
    if (src) src.setTiles([vectorUrl('pois')]);
  }

  function applyMapLayer(d) {
    if (!d.parts || !state.map) return;
    d.parts.forEach(([id, prop, f]) => {
      if (!state.map.getLayer(id)) return;
      state.map.setLayoutProperty(id, 'visibility', d.on ? 'visible' : 'none');
      state.map.setPaintProperty(id, prop, d.opacity * f);
    });
  }

  async function showRasterLegend(d) {
    const el = $('map-layers').querySelector(`[data-mll="${d.id}"]`);
    try {
      const lg = await api(state.layers.tiles.raster.legend.replace('{layer}', encodeURIComponent(d.layer)));
      el.innerHTML = lg.kind === 'categorical'
        ? lg.classes.map((c) => `<span><span class="sw" style="background:${esc(c.color)}"></span>${esc(c.label)}</span>`).join('')
        : `<div class="ramp">${lg.colors.map((c) => `<span style="background:${esc(c)}"></span>`).join('')}</div>` +
          `<span>deciles of values &gt; 0: ${fmtSmart(lg.breaks[0])} … ${fmtSmart(lg.breaks[lg.breaks.length - 1])} ${esc(lg.units)}</span>`;
      el.hidden = false;
    } catch (_) { el.hidden = true; }
  }

  // Per-tile cost from the Server-Timing header (duckdb;dur=… or cache;desc=hit).
  let tileStatsPending = false;
  function renderTileStats() {
    if (tileStatsPending) return;
    tileStatsPending = true;
    requestAnimationFrame(() => {
      tileStatsPending = false;
      const db = tileLog.filter((t) => t.duckdb != null).map((t) => t.duckdb).sort((a, b) => a - b);
      const hits = tileLog.filter((t) => t.hit).length;
      const empty = tileLog.filter((t) => t.empty).length;
      const feats = tileLog.reduce((s, t) => s + t.features, 0);
      const q = (p) => db[Math.min(db.length - 1, Math.floor(p * db.length))];
      $('tile-stats').textContent = `${tileLog.length} tiles (${hits} from tile cache${empty ? `, ${empty} empty` : ''}) · DuckDB ` +
        (db.length ? `median ${fmt.format(q(0.5))} ms, p95 ${fmt.format(q(0.95))} ms` : '–') +
        ` · ${fmt0.format(feats)} features encoded`;
    });
  }

  // Popup for a POI, building or H3 cell under the click; true if there is one.
  function layerPopup(e, show) {
    const ids = ['ml-pois', 'ml-bpoints', 'ml-bcells', 'ml-h3'].filter((id) => state.map.getLayer(id) && state.map.getLayoutProperty(id, 'visibility') === 'visible');
    if (!ids.length) return false;
    const f = state.map.queryRenderedFeatures(e.point, { layers: ids })[0];
    if (!f) return false;
    if (!show) return true;
    const p = f.properties;
    let html;
    if (f.layer.id === 'ml-pois') html = `<h3>${esc(p.name || '(no name)')}</h3><div class="sub">${esc(String(p.category).replace(/_/g, ' '))}</div>`;
    else if (f.layer.id === 'ml-bpoints') html = `<h3>Building</h3><div class="sub">boundary-cell point · ${fmtSmart(p.area_m2)} m² footprint</div>`;
    else if (f.layer.id === 'ml-bcells') html = `<h3>${fmt0.format(p.buildings)} buildings</h3><div class="sub">interior H3 cell, pre-summed · ${fmtSmart(p.area_m2)} m² footprint</div>`;
    else html = `<h3>H3 cell (${esc(p.cell_type)})</h3><div class="sub">${esc(p.h3_cell)} · village ${esc(p.village_id)}</div>`;
    new maplibregl.Popup({ closeButton: true, maxWidth: '280px' }).setLngLat(e.lngLat).setHTML(html).addTo(state.map);
    return true;
  }

  // ---------- misc ----------
  function showError(msg) { $('error').textContent = msg; $('error').hidden = false; }
  function hideError() { $('error').hidden = true; }
  function esc(s) { return String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])); }

  function setMode(mode) {
    state.mode = mode;
    $('tab-admin').setAttribute('aria-selected', String(mode === 'admin'));
    $('tab-draw').setAttribute('aria-selected', String(mode === 'draw'));
    $('area-admin').hidden = mode !== 'admin';
    $('area-draw').hidden = mode !== 'draw';
    if (mode !== 'draw' && state.draw && isDrawing()) state.draw.setMode('static');
  }

  function wire() {
    $('sel-province').addEventListener('change', onProvince);
    $('sel-regency').addEventListener('change', onRegency);
    $('tab-admin').addEventListener('click', () => setMode('admin'));
    $('tab-draw').addEventListener('click', () => setMode('draw'));
    $('btn-draw').addEventListener('click', () => {
      if (!state.draw) { showError('Drawing tools failed to load.'); return; }
      state.draw.clear(); state.drawn = null;
      state.draw.setMode('polygon');
      $('draw-status').textContent = 'Drawing… click to add points.';
    });
    $('btn-clear').addEventListener('click', () => {
      if (state.draw) { state.draw.clear(); state.draw.setMode('static'); }
      state.drawn = null; $('draw-status').textContent = '';
    });
    $('cat-search').addEventListener('input', searchCategories);
    $('cat-search').addEventListener('keydown', (e) => { if (e.key === 'Escape') { e.target.value = ''; searchCategories(); } });
    $('cat-results').addEventListener('click', (e) => {
      const li = e.target.closest('li[data-c]');
      if (!li) return;
      const c = li.dataset.c;
      state.selected.has(c) ? state.selected.delete(c) : state.selected.add(c);
      renderSelected(); searchCategories();
    });
    ['pop-on', 'nl-on', 'lc-on', 'bld-on'].forEach((id) => $(id).addEventListener('change', updateViewOptions));
    $('sel-view').addEventListener('change', () => { updateViewOptions(); render(false); });
    $('per-km2').addEventListener('change', () => render(false));
    document.querySelectorAll('input[name=method]').forEach((r) => r.addEventListener('change', () => render(false)));
    $('btn-run').addEventListener('click', run);
    $('top').addEventListener('click', (e) => {
      const tr = e.target.closest('tr[data-id]');
      if (!tr || !state.result) return;
      const f = state.result.features.find((x) => x.id === tr.dataset.id);
      if (f) { fitTo([f]); setHover(f.id); }
    });
    $('diag-toggle').addEventListener('click', () => {
      const open = $('diag-body').hidden;
      $('diag-body').hidden = !open;
      $('diag-toggle').setAttribute('aria-expanded', String(open));
    });
  }

  init();
})();
