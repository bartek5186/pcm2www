---
name: pcm2www
description: Use when working on the pcm2www Go application that imports PC-Market XML exports, stores staging data in a Gorm-backed database, primes a WooCommerce product cache, links products by EAN, plans and executes WooCommerce sync tasks, or extends the PCM-to-Woo sync flow.
---

# PCM2WWW

## When to use this skill

Use this skill for tasks in this repository involving:

- PC-Market import flow
- WooCommerce cache integration
- EAN linking between staging data and Woo cache
- task planning (planner) and task execution (worker)
- config, syncer, CLI, or DB schema changes
- diagnosing why data is or is not moving through the pipeline

## Current reality of the app

Trust the code over the README, but the README status table is accurate as of 2026-03-17.

Implemented and active:

- config loading from `~/.config/pcm2www/config.json`
- DB open + Gorm migrations for `sqlite`, `postgres`, and `mysql`
- long-running syncer that starts integrations from the config registry
- importer for `exp_wyk_*.xml` (dedup by SHA256 + transmisja_id, charset normalization, batch upserts)
- staging upserts into `st_products` and `st_stocks`
- WooCommerce product cache prime (full paginated load) and incremental sweep (by date_modified_gmt)
- Woo-to-staging linking by EAN (`st_products.kod` → `woo_product_caches.ean`, digits-only match)
- diagnostics in `link_issues` (missing EAN, missing in shop, duplicate EAN, missing in magazine)
- task planner (`planner.go`): compares staging vs cache, generates `woo_tasks` for EAN/stock/price updates
- task worker (`worker.go`): N parallel workers (default 3, config `workers`), each runs a tight loop:
  - **batch kinds** (`price.update`, `stock.update`): claim up to 20 tasks → batch GET (`?include=`) → policy check per product → batch POST (`/products/batch`) → verify per product → sync cache
  - **sequential kinds** (`ean.update`, `availability.update`): claim 1 → GET → policy check → PUT → verify → sync cache
  - `ean.update`: sets EAN on product (skips if product already has any EAN, or EAN taken)
  - `stock.update`: updates stock quantity (skips if manage_stock=false, already matches)
  - `price.update`: updates regular_price + hurt_price + tax_class (skips if sale_price active, or already matches); `tax_class` mapped from `vat_id` via `vatIDToTaxClass()` in planner: 2300→"2300", 800→"800", 500→"500", 0/-1→"zero-rate", other→"" (standard)
  - `availability.update`: sets manage_stock + stock_status + backorders based on cena_detal (see availability logic below)
- retry/requeue logic on worker failure
- CLI mode on non-Windows, systray app on Windows

Not implemented or only scaffolded:

- creating new products in WooCommerce (no create path in planner or worker)
- handling other PCM export types such as `exp_dok_*`
- fetching orders from WooCommerce (tick() method in woocommerce.go is scaffolded/logging only)

## Runtime model

On non-Windows builds, the app entrypoint is `main-cli.go`. On Windows without the `dev` tag, the entrypoint is `main.go`.

At startup the app:

1. Resolves the app data directory via `os.UserConfigDir()` → `~/.config/pcm2www/` on Linux-like systems.
2. Loads or creates `config.json`.
3. Opens the configured database and runs migrations.
4. Builds integrations from `config.integrations`.
5. Starts each integration in its own goroutine and gives it `*gorm.DB` through context.

Important runtime files:

- config: `~/.config/pcm2www/config.json`
- log: `~/.config/pcm2www/app.log`
- default sqlite DB: `~/.config/pcm2www/pcm2www.db`

The repository-level `.env` is currently not part of the runtime path in code. Do not assume env-driven config unless you add it explicitly.

## Code map

- `README.md`: high-level intent and current status table
- `main-cli.go`: CLI entrypoint, command loop, manual DB reset
- `main.go`: Windows systray entrypoint
- `internal/config/config.go`: config schema, default config creation, integration unmarshalling
- `internal/syncer/syncer.go`: lifecycle management for integrations
- `internal/integrations/registry.go`: integration registry
- `internal/integrations/importer/importer.go`: file discovery, dedup, XML parsing, staging upserts; triggers linker + planner
- `internal/integrations/importer/linker.go`: EAN-based matching and `link_issues`
- `internal/integrations/importer/planner.go`: compares staging vs cache, enqueues `woo_tasks` idempotently
- `internal/integrations/woocommerce/woocommerce.go`: Woo integration lifecycle; spawns cache sweeper + worker
- `internal/integrations/woocommerce/cache.go`: Woo cache prime and sweep logic
- `internal/integrations/woocommerce/worker.go`: task queue consumer; claim → fetch → PUT → verify → sync cache
- `internal/integrations/woocommerce/custom_fields.go`: custom field read/write helpers (e.g. hurt_price)
- `internal/db/models.go`: staging/cache/task/link tables
- `internal/db/migrate.go`: migration flow and defensive `link_issues` index handling
- `internal/db/task_payloads.go`: payload structs for WooEANUpdate, WooStockUpdate, WooPriceUpdate

Useful repo data:

- `imports/`: sample PCM XML files for local inspection
- `reports/`: generated CSV diagnostics, useful as artifacts, not source code

## How data moves today

The current working flow is:

1. Importer scans `watch_dir` for `exp_wyk_*.xml`, computes SHA256, checks `import_files` for dedup.
2. XML is parsed with charset normalization (ISO-8859-2, Windows-1250, etc.).
3. Product rows are upserted into `st_products`, stock rows into `st_stocks`.
4. Importer triggers `LinkProductsByEAN()`.
5. Linker matches `st_products.kod` (digits-only) against `woo_product_caches.ean` (digits-only).
6. Matched Woo cache rows get `towar_id` filled in; mismatches go to `link_issues`.
7. Importer triggers `PlanWooTasksForImports()`.
8. Planner compares staging vs cache for each linked product, enqueues `woo_tasks` (ean/stock/price).
9. Worker runs in background, claims tasks atomically, hits Woo REST API, verifies result, syncs cache.
10. Cache sweeper runs independently every `sweep_interval_minutes`, fetches recently modified Woo products.

`kod` is effectively treated as the source EAN during matching. If a task changes that assumption, update both importer and linker logic deliberately.

## Working rules for AI

Start most tasks by reading:

1. `README.md`
2. the entrypoint relevant to the platform
3. `internal/syncer/syncer.go`
4. the integration package you are about to change

When changing config behavior:

- update `internal/config/config.go`
- update `config.json.example`
- update `README.md` if user-facing behavior changed
- check whether default config generation also needs the new field

When changing importer behavior:

- verify against real sample files in `imports/`
- keep dedup logic intact unless the task is explicitly about reprocessing semantics
- preserve charset handling unless you have a replacement proven against PCM exports
- batch writes through Gorm upserts instead of row-by-row inserts
- after import, linker and planner are always triggered — keep that chain intact

When changing linking behavior:

- treat `link_issues` as a full rebuild table (cleared and rebuilt each run)
- keep diagnostics readable; this table is the main operator-facing explanation layer
- be explicit about duplicate EAN and missing-product semantics

When changing planner behavior:

- planner is idempotent: pending/running tasks are skipped; done/error tasks are requeued
- planner only operates on linked products (towar_id filled); unlinked = skip
- planner only operates on unambiguous 1:1 matches; >1 woo entry per towar_id = skip
- **prev_stock guard**: `st_stocks.stan_prev` stores the previous PCM stock value (NULL on first import). If `stan == stan_prev` (PCM didn't change since last export), the planner skips `stock.update` even if the Woo cache shows a different value. This prevents overwriting stock reductions caused by online sales. If PCM stock changed (e.g. delivery, inventory correction), the task is generated with an absolute set value from PCM.
- **availability logic**: `planAvailabilityUpdateTask()` is always called for every linked product. If `cena_detal == 0` → desired state is `manage_stock=false, stock_status=outofstock`. If `cena_detal > 0` → desired state is `manage_stock=true, backorders=notify`. Task is skipped only if cache already matches the desired state. `stock.update` and `price.update` are both skipped when `cena_detal=0`.

When changing worker behavior:

- worker uses atomic claim (status: pending → running in single UPDATE)
- every PUT to Woo is followed by a GET to verify the change was applied
- cache is synced from the verified GET response, not from the request payload
- failed tasks are requeued (not lost); context-cancelled tasks are also requeued

When changing Woo cache behavior:

- separate read-side cache logic from write-side task processing
- sweep uses `kvs` table to store last seen `date_modified_gmt`; don't break that state
- cache prime is paginated (100/page), ordered by modified desc

When changing DB schema:

- edit `internal/db/models.go`
- update `internal/db/migrate.go` if indexes or migration sequencing matter
- think about existing unique constraints before changing conflict clauses

## Known pitfalls

- `LoadOrCreate()` generates a default config with `woocommerce`, but not with the `importer` integration. For full local flow, compare against `config.json.example`.
- `syncer` manages integration lifecycles and emits heartbeat; it is not the business sync engine.
- `LinkProductsByEAN()` matches digits-only EANs. Formatting differences are intentionally normalized.
- `WooProductCache.TowarID` is filled by the linker, not by Woo cache fetch.
- Woo cache sweep relies on `date_modified_gmt` ordering and stores last seen timestamp in `kvs`.
- Non-Windows and Windows builds do not use the same entrypoint file. Be careful with build tags.
- `main-cli.go` prints `resetdb!` in help text, but the actual command branch is `resetdb`.
- Planner does NOT create new products in Woo — only updates to existing linked products.
- Worker skips `ean.update` if the product in Woo already has ANY EAN (conservative policy).
- Worker skips `price.update` if `sale_price > 0` (does not override active promotions).
- `st_stocks.stan_prev` is NULL for the first import of each warehouse row; planner treats NULL as "no history" and uses absolute set. Only on the second and subsequent imports does the prev_stock guard activate.
- `stock_status` and `backorders` are always included in Woo API requests via `ensureProductFields()` regardless of the user's `fields` config string — do not remove them from the required list in `custom_fields.go`.
- When `cena_detal=0`, planner skips both `stock.update` and `price.update` and only generates `availability.update`. Do not add price=0 writes to Woo — that would make products free.
- `availability.update` task key encodes the desired state (`available` or `unavailable`) — changing price from 0 to non-zero generates a new task key, triggering a fresh task rather than a requeue.

## Preferred validation

Default verification:

- `go test ./...`

When touching startup/config/integration wiring:

- check that the config can still be loaded
- confirm the expected integrations are present in `config.integrations`
- if safe, run the CLI locally and inspect `~/.config/pcm2www/app.log`

When touching importer logic:

- inspect a real sample from `imports/`
- verify `import_files`, `st_products`, and `st_stocks` behavior
- verify linker output in `link_issues`
- verify planner output in `woo_tasks`

When touching worker logic:

- check task status transitions (pending → running → done/skipped/error)
- verify cache is updated after successful task
- check retry behavior on failure

When touching Woo cache logic:

- prefer tests or narrowly scoped dry runs
- verify sweep timestamp is correctly persisted in `kvs`

## Change strategy

Use these patterns:

- importer/parsing work: `internal/integrations/importer/importer.go`
- matching and reconciliation work: `internal/integrations/importer/linker.go`
- task planning work: `internal/integrations/importer/planner.go`
- Woo write/execute work: `internal/integrations/woocommerce/worker.go`
- Woo read/cache work: `internal/integrations/woocommerce/cache.go`
- custom field handling: `internal/integrations/woocommerce/custom_fields.go`
- new task types: `internal/db/models.go` + `internal/db/task_payloads.go` + `worker.go` + `planner.go`
- config or lifecycle work: `internal/config/config.go`, `internal/syncer/syncer.go`, relevant entrypoint

If a request sounds like "synchronizacja z Woo działa?", verify whether the user means:

- cache read from Woo (yes, works)
- EAN linking between local and Woo data (yes, works)
- write-back of stock/EAN/price from PCM to Woo (yes, works — worker active)
- creating new products in Woo (no, not implemented)
