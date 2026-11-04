# Bundled sample release

`releases/diy-2026-11-a/` is the DI Yogyakarta release built by `make prep-diy` (see `docs/prep.md`). `docker compose up` uploads it to RustFS (`seed` service), so the demo runs with no downloads or prep.

| Data | Source | License |
|---|---|---|
| Village boundaries (438 desa/kelurahan) | [HDX COD-AB Indonesia](https://data.humdata.org/dataset/cod-ab-idn), BPS / WFP / OCHA ROAP, boundaries 2020 | CC BY-IGO |
| POIs (Overture Places 2026-10-21.0, DIY bbox) | [Overture Maps Foundation](https://overturemaps.org) | CDLA-Permissive-2.0 (some sources Apache-2.0) |
| Population 2020, constrained, 100 m (cropped) | [WorldPop](https://www.worldpop.org), University of Southampton | CC BY 4.0 |

Derived files: Parquet/COG conversions, H3 classification and QC by this project (the code is MIT-licensed; the data keeps the licenses above). Checksums and QC results are in `manifest.json`. To rebuild it, run `make prep-diy`, then copy `out/releases/diy-2026-11-a` here.
