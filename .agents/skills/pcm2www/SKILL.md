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

Trust the code over the README, but the README status table is accurate as of 2026-09-03.

Implemented and active:

- config loading from `~/.config/pcm2www/config.json`
- DB open + Gorm migrations for `sqlite`, `postgres`, and `mysql`
- long-running syncer that starts integrations from the config registry
- importer for `exp_wyk_*.xml` only (ZIP is ignored; file-stability + post-parse SHA guard, dedup by SHA256 or non-empty transmisja_id, charset normalization, duplicate-key rejection, transactional batch upserts)
- staging upserts into `st_products` and `st_stocks`
- WooCommerce product cache prime (full paginated reconciliation; unseen rows are removed only after an individual 404 confirmation) and overlapping incremental sweep (by date_modified_gmt, `status=any`)
- Woo-to-staging linking by EAN (`st_products.kod` → `woo_product_caches.ean`, digits-only match)
- diagnostics in `link_issues` (missing EAN, missing in shop, duplicate EAN on either PCM or Woo side, missing in magazine)
- task planner (`planner.go`): compares staging vs cache, generates `woo_tasks` for stock/price/availability updates on products linked by EAN
- task worker (`worker.go`): N parallel workers (default 3, config `workers`), each runs a tight loop:
  - **batch kinds** (`price.update`, `stock.update`, `availability.update`): claim up to 20 tasks → batch GET (`?include=`) → policy check per product → batch POST (`/products/batch`) → a separate verification GET → sync cache
  - legacy `ean.update` tasks are marked `skipped` without calling Woo; EAN is never written by the app
  - `stock.update`: updates stock quantity (skips if manage_stock=false, already matches)
  - `price.update`: updates regular_price + hurt_price + tax_class (skips if sale_price active, or already matches); `tax_class` mapped from `vat_id` via `vatIDToTaxClass()` in planner: 2300→"2300", 800→"800", 500→"500", 0/-1→"zero-rate", other→"" (standard)
  - `availability.update`: sets manage_stock + stock_status + backorders + catalog_visibility based on cena_detal; when reactivating a product it can also restore current PCM stock
- retries transient worker failures (timeout, HTTP 429/5xx) up to 5 attempts with backoff; interrupted `running` tasks are recovered on startup
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
- `internal/db/task_payloads.go`: task-kind constants and payload structs for Woo tasks

Useful repo data:

- `imports/`: sample PCM XML files for local inspection
- `reports/`: generated CSV diagnostics, useful as artifacts, not source code

## How data moves today

The current working flow is:

1. Importer scans `watch_dir` for `exp_wyk_*.xml`, computes SHA256, checks `import_files` for dedup.
2. XML is parsed with charset normalization (ISO-8859-2, Windows-1250, etc.).
3. Product rows are upserted into `st_products`, stock rows into `st_stocks`.
4. Woo marks its cache ready only after a successful prime (or immediately when prime is disabled); importer waits on that runtime signal before linking and planning. There is no timer-based startup guess.
5. Linker matches `st_products.kod` (digits-only) against `woo_product_caches.ean` (digits-only).
6. Matched Woo cache rows get `towar_id` filled in; mismatches go to `link_issues`.
7. Importer triggers `PlanWooTasksForImports()`.
8. Planner compares staging vs cache for each linked product, enqueues `woo_tasks` (stock/price/availability).
9. Worker runs in background, claims tasks atomically, hits Woo REST API, verifies result, syncs cache.
10. Cache sweeper runs independently every `sweep_interval_minutes`, fetches recently modified Woo products.

File identity is SHA256 or a non-empty `transmisja_id`; filename is metadata and may be reused for a different export. The import status `done` and staging changes commit in one transaction. `towar_id` is the staging product identity, so a changed `kod`/EAN updates the same PCM product row. Exact duplicate `towar_id` or `(towar_id, magazyn_id)` keys within one XML fail the import atomically.

`kod` is treated as the source EAN during matching. EAN is the only link key: if no unique match exists in Woo, the product remains unlinked and is not updated. Do not add SKU fallback, create missing Woo products, or plan EAN writes unless the user explicitly changes this policy.

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
- accept only `exp_wyk_*.xml`; do not advertise ZIP support without implementing and testing extraction
- never use filename alone for deduplication
- batch writes through Gorm upserts instead of row-by-row inserts
- after import, linker and planner are always triggered — keep that chain intact

When changing linking behavior:

- treat `link_issues` as a full rebuild table (cleared and rebuilt each run)
- keep diagnostics readable; this table is the main operator-facing explanation layer
- be explicit about duplicate EAN and missing-product semantics
- reject ambiguous EANs on both sides: `duplicate_ean_source` for PCM and `duplicate_ean_shop` for Woo; neither case may produce a link

When changing planner behavior:

- planner is idempotent for an identical task key; a newer desired state supersedes older pending tasks for the same Woo product and operation
- planner only operates on linked products (towar_id filled); unlinked = skip
- planner only operates on unambiguous 1:1 matches; >1 woo entry per towar_id = skip
- **prev_stock guard**: `st_stocks.stan_prev` stores the previous PCM stock value (NULL on first import). If `stan == stan_prev` (PCM didn't change since last export), the planner skips `stock.update` even if the Woo cache shows a different value. This prevents overwriting stock reductions caused by online sales. If PCM stock changed (e.g. delivery, inventory correction), the task is generated with an absolute set value from PCM.
- **availability logic**: `planAvailabilityUpdateTask()` is always called for every linked product. If `do_usuniecia=true` or `cena_detal == 0` → desired state is `manage_stock=false, stock_status=outofstock, catalog_visibility=hidden`. Otherwise the desired state is `manage_stock=true, backorders=notify, catalog_visibility=visible`; when cache has `manage_stock=false`, include current PCM stock in the availability task so reactivation restores it atomically. `stock.update` and `price.update` are skipped for every unavailable source.
- **PCM activity policy**: `do_usuniecia=true` hides the linked Woo product and suppresses stock/price tasks; it never deletes the Woo product. `aktywny_w_SI` is stored but does not gate sync because all 941 occurrences in the available real fixtures are `N`, including normal products. Do not give it behavioral meaning without confirmed PC-Market semantics and updated fixtures/tests.

When changing worker behavior:

- worker uses atomic claim (status: pending → running in single UPDATE)
- every PUT to Woo is followed by a GET to verify the change was applied
- cache is synced from the verified GET response, not from the request payload
- immediately before any Woo write, validate that the cached `woo_id` is still linked to the task's `towar_id` and that current normalized EANs match; otherwise mark the task `superseded` without an API call
- task-state persistence errors are fatal to the Woo integration; do not log-and-ignore them
- transient retries honor `Retry-After`, add jitter, and share a circuit breaker across workers
- transient failures (timeouts, HTTP 429/5xx) are retried with backoff up to 5 attempts; permanent failures end as `error`
- context-cancelled tasks return to `pending`, and startup recovers tasks left in `running`
- serialize writes per Woo product and skip a task if a newer task for the same product and kind exists

When changing Woo cache behavior:

- separate read-side cache logic from write-side task processing
- sweep uses `kvs` table to store last seen `date_modified_gmt`; don't break that state
- cache prime is paginated (100/page), ordered by modified desc
- only after a complete successful prime, individually GET unseen Woo IDs and delete only explicit 404s; never prune after a partial/failed fetch or an ambiguous pagination miss
- incremental sweep cannot prove deletion and must not prune unseen rows

When changing DB schema:

- edit `internal/db/models.go`
- update `internal/db/migrate.go` if indexes or migration sequencing matter
- think about existing unique constraints before changing conflict clauses
- migrations must be repeatable and preserve valid data; deduplicate only rows that block a required unique index and use Gorm's dialect-aware migrator instead of raw engine-specific DDL

## Known pitfalls

- `LoadOrCreate()` generates a default config with `woocommerce`, but not with the `importer` integration. For full local flow, compare against `config.json.example`.
- `auto_start` starts all configured integrations when the process opens; it does not register OS startup or control polling. First-run config includes importer and Woo templates, remains `auto_start=false`, and rejects placeholders/invalid settings on manual start.
- `syncer` manages integration lifecycles and emits heartbeat; it is not the business sync engine.
- `LinkProductsByEAN()` matches digits-only EANs. Formatting differences are intentionally normalized.
- `WooProductCache.TowarID` is filled by the linker, not by Woo cache fetch.
- Woo cache sweep relies on `date_modified_gmt` ordering, overlaps the previous boundary by two seconds, requests `status=any`, and stores last seen timestamp in `kvs`.
- Syncer lifecycle operations are serialized. Config reload must fully build/validate the replacement before stopping the current run, preserve the old run on rejection, restart under the original parent context, and expose per-integration failures.
- A process-level `pcm2www.lock` is the local concurrency policy. Do not recover `running` tasks from a second local instance.
- schema changes are recorded in `schema_migrations`; file-backed SQLite is backed up before a pending schema migration and only the five newest migration backups are kept.
- Non-Windows and Windows builds do not use the same entrypoint file. Be careful with build tags.
- `main-cli.go` prints `resetdb!` in help text, but the actual command branch is `resetdb`.
- Planner does NOT create new products in Woo — only updates to existing linked products.
- EAN is link-only. Planner never creates `ean.update`; worker skips legacy queued EAN tasks without an API call.
- Worker skips `price.update` if `sale_price > 0` (does not override active promotions).
- `st_stocks.stan_prev` is NULL for the first import of each warehouse row; planner treats NULL as "no history" and uses absolute set. Only on the second and subsequent imports does the prev_stock guard activate.
- `stock_status`, `backorders`, `tax_class`, and `catalog_visibility` are always included in Woo API requests via `ensureProductFields()` regardless of the user's `fields` config string — do not remove them from the required list in `custom_fields.go`.
- When `cena_detal=0`, planner skips both `stock.update` and `price.update` and only generates `availability.update`. Do not add price=0 writes to Woo — that would make products free.
- `availability.update` task key encodes the desired state and, when needed, restored stock — changing price from 0 to non-zero generates a fresh desired-state task.

## Preferred validation

Default verification:

- `go test ./...`
- `./scripts/validate_xml_sequence.sh` for the local real-fixture reference-model test; it validates the complete staging state after every XML on an isolated in-memory DB and never opens the configured app database

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
- write-back of stock/price/availability from PCM to Woo for EAN-linked products (yes, works — worker active)
- creating new products in Woo (no, not implemented)
