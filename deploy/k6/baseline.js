// Performance baseline (results in docs/caching.md); measures this project only.
// Runs on the compose network so both replicas are hit round-robin:
//   make perf-baseline            (SOAK=5m by default)
import http from 'k6/http';
import { check } from 'k6';

const TARGETS = (__ENV.TARGETS || 'http://api1:8080,http://api2:8080').split(',');
const DUR = __ENV.SOAK || '5m';
let n = 0;
const url = () => TARGETS[(n++) % TARGETS.length] + '/v1/aggregate';

const poi = (id, cats) => ({ id, type: 'poi_count', categories: cats });
const pop = (method) => ({ id: 'pop_' + method, type: 'raster_sum', layer: 'population_2020', method });
const body = (area, metrics) => JSON.stringify({ area, metrics });
const REGENCIES = ['3401', '3402', '3403', '3404', '3471'];
const CATS = [['cafe', 'coffee_shop'], ['school', 'elementary_school'], ['muslim_place_of_worship'], ['restaurant'], ['convenience_store']];
const pick = (a) => a[Math.floor(Math.random() * a.length)];

function post(payload, extraHeaders) {
  const res = http.post(url(), payload, { headers: Object.assign({ 'Content-Type': 'application/json' }, extraHeaders || {}) });
  check(res, { 'status 200': (r) => r.status === 200 });
}

export const options = {
  scenarios: {
    // 1. One regency (Sleman, 86 villages), POI count, warm cache: p95 < 300 ms.
    regency_poi_warm: { executor: 'constant-vus', vus: 1, duration: '30s', exec: 'regencyPoiWarm', startTime: '0s' },
    // 2. One regency, fractional population, cold cache (no-cache): p95 < 3 s.
    regency_pop_cold: { executor: 'constant-vus', vus: 1, duration: '30s', exec: 'regencyPopCold', startTime: '35s' },
    // 3. One province (438 villages), both metrics, warm cache: p95 < 1 s.
    province_both_warm: { executor: 'constant-vus', vus: 1, duration: '30s', exec: 'provinceBothWarm', startTime: '70s' },
    // 4. 20 concurrent users, mixed requests: no errors (memory watched with docker stats).
    mixed_20_users: { executor: 'constant-vus', vus: 20, duration: DUR, exec: 'mixed', startTime: '105s' },
  },
  thresholds: {
    'http_req_duration{scenario:regency_poi_warm}': ['p(95)<300'],
    'http_req_duration{scenario:regency_pop_cold}': ['p(95)<3000'],
    'http_req_duration{scenario:province_both_warm}': ['p(95)<1000'],
    'http_req_failed': ['rate==0'],
  },
  summaryTrendStats: ['min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

export function regencyPoiWarm() { post(body({ regency_code: '3404' }, [poi('cafes', ['cafe', 'coffee_shop'])])); }
export function regencyPopCold() { post(body({ regency_code: '3404' }, [pop('fractional')]), { 'Cache-Control': 'no-cache' }); }
export function provinceBothWarm() {
  post(body({ province_code: '34' }, [poi('cafes', ['cafe', 'coffee_shop']), pop('fractional'), pop('center')]));
}
export function mixed() {
  const r = Math.random();
  if (r < 0.15) {
    post(body({ province_code: '34' }, [poi('c', pick(CATS)), pop('fractional')]));
  } else if (r < 0.25) { // cold path
    post(body({ regency_code: pick(REGENCIES) }, [pop('fractional'), pop('center')]), { 'Cache-Control': 'no-cache' });
  } else if (r < 0.45) { // random drawn box in DIY
    const x = 110.05 + Math.random() * 0.7, y = -8.15 + Math.random() * 0.55, w = 0.01 + Math.random() * 0.08;
    post(body({ geometry: { type: 'Polygon', coordinates: [[[x, y], [x + w, y], [x + w, y + w], [x, y + w], [x, y]]] } },
      [poi('c', pick(CATS)), pop('fractional')]));
  } else {
    post(body({ regency_code: pick(REGENCIES) }, [poi('c', pick(CATS)), pop(Math.random() < 0.5 ? 'fractional' : 'center')]));
  }
}
