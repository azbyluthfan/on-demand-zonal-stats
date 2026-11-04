COMPOSE := docker compose -f deploy/docker-compose.yml --env-file $(if $(wildcard .env),.env,.env.example)
PREP    := $(COMPOSE) --profile prep run --rm prep

.PHONY: help app-up app-down api-local test test-race lint vet build tidy e2e perf-baseline \
        prep-image prep-fixture prep-diy prep-upload golden-raster up down logs spike spike-local

help: ## Show targets
	@grep -E '^[a-z0-9-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-14s %s\n", $$1, $$2}'

# ---- demo ---------------------------------------------------------------
app-up: ## Demo: RustFS + seed + api1 (:8081) + api2 (:8082) + load balancer (:8080)
	$(COMPOSE) up -d --build

app-down: ## Stop the demo (keeps the RustFS volume)
	$(COMPOSE) down

api-local: ## One API on the host (:8080) reading a local release directory
	RELEASE_BASE=$${RELEASE_BASE:-sample/releases/diy-2026-11-a} DEV_INSTALL_EXTENSIONS=1 LISTEN_ADDR=:8080 go run ./cmd/api

# ---- checks -------------------------------------------------------------
test: ## Unit, golden, invariant and contract tests
	go test -count=1 ./...

test-race: ## Tests with the race detector
	go test -race -count=1 ./...

lint: ## gofmt + go vet + staticcheck
	@test -z "$$(gofmt -l . | tee /dev/stderr)"
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...

vet:
	go vet ./...

build:
	go build ./...

tidy:
	@echo "duckdb-go is pinned to v2.10505.0 (DuckDB 1.5.5) for the cog extension; see docs/spikes.md"
	go mod tidy

e2e: ## Playwright smoke test + README screenshots (needs app-up); NO_SHOTS=1 skips screenshots
	docker run --rm --network zonalstats_default -v "$(CURDIR)":/work -w /tmp/e2e -e NO_SHOTS=$(NO_SHOTS) \
	  mcr.microsoft.com/playwright:v1.64.1-noble sh -c \
	  'npm init -y >/dev/null && npm install --silent playwright@1.64.1 && cp /work/e2e/smoke.mjs . && node smoke.mjs'

perf-baseline: ## k6 baseline against both replicas (needs app-up); SOAK=5m
	docker run --rm --network zonalstats_default -v "$(CURDIR)/deploy/k6":/k6 -e SOAK=$${SOAK:-5m} \
	  grafana/k6 run --summary-export=/k6/summary.json /k6/baseline.js

# ---- data releases ------------------------------------------------------
prep-image:
	$(COMPOSE) --profile prep build prep

prep-fixture: prep-image ## Build the synthetic fixture release (no downloads)
	$(PREP) build -config configs/fixture.json

prep-diy: prep-image ## Build the DI Yogyakarta release (needs data/raw; see docs/prep.md)
	$(PREP) build -config configs/diy-2026-11.json

prep-upload: up ## Upload a built release: make prep-upload RELEASE=diy-2026-11-a
	$(PREP) upload -dir out/releases/$(RELEASE)

# Regenerate testdata/golden/*_population_2020.csv with exactextract and
# rasterstats (amd64 container: exactextract ships no linux/arm64 wheel).
golden-raster: ## Regenerate raster golden CSVs (needs out/releases/{fixture-1,diy-2026-11-a})
	mkdir -p data/work
	for r in fixture-1 diy-2026-11-a; do rm -f data/work/villages_$$r.geojson; \
	  go run ./cmd/duckq "COPY (SELECT village_id, geometry FROM read_parquet('out/releases/$$r/boundaries/villages.parquet')) TO 'data/work/villages_$$r.geojson' WITH (FORMAT GDAL, DRIVER 'GeoJSON')"; done
	docker run --rm --platform linux/amd64 -v "$(CURDIR)":/work -w /work python:3.12-slim sh -c '\
	  apt-get update -qq && apt-get install -y -qq libexpat1 >/dev/null && \
	  pip install -q --root-user-action=ignore --only-binary=:all: exactextract rasterstats rasterio fiona shapely pandas && \
	  python -I scripts/golden_raster.py data/work/villages_fixture-1.geojson out/releases/fixture-1/rasters/population_2020.tif testdata/golden/fixture_population_2020.csv && \
	  python -I scripts/golden_raster.py data/work/villages_diy-2026-11-a.geojson out/releases/diy-2026-11-a/rasters/population_2020.tif testdata/golden/diy_population_2020.csv'

# ---- infrastructure / verification spike --------------------------------------------
up: ## Start RustFS only
	$(COMPOSE) up -d rustfs

down:
	$(COMPOSE) --profile spike --profile prep down

logs:
	$(COMPOSE) logs -f

spike: ## Storage/extension verification spike in Docker (RustFS + COG fixture)
	$(COMPOSE) --profile spike up --build --abort-on-container-exit --exit-code-from spike spike

spike-local:
	go run ./cmd/spike -install-only
	S3_ENDPOINT=localhost:9000 go run ./cmd/spike
