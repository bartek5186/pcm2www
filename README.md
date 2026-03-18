# Integracja PCM2WWW z WooCommerce

Ten plik opisuje integrację systemu **PC-Market 7 (PCM)** poprzez narzędzie **pcm2www** z platformą **WooCommerce**.
Integrator działa cyklicznie, pobiera dane z katalogu eksportów PC-Market (`exp_wyk_*.xml`) oraz synchronizuje je z WooCommerce przy użyciu REST API.

> **Status implementacji (2026-03-17):** funkcje oznaczone jako **[NIEGOTOWE]** nie są jeszcze ukończone.

## Funkcjonalności

- **Automatyczna synchronizacja** stanów magazynowych, EAN i cen do WooCommerce (aktywna)
- **Obsługa cache** – pełne i przyrostowe odświeżanie danych z WooCommerce
- **Import plików PCM** – aktualnie obsługiwany format: `exp_wyk_*.xml` **[inne typy: NIEGOTOWE]**
- **Integracja przez REST API** WooCommerce: update stanu, EAN, ceny (aktywne); tworzenie nowych produktów **[NIEGOTOWE]**
- **Elastyczna konfiguracja** poprzez plik JSON
- **Ciągła praca w tle** – monitoring katalogu, kolejka tasków, worker wysyłki do Woo

---

Integrator posiada narzędzie CLI (Linux/Mac) oraz aplikację z systray (Windows).
Plik konfiguracyjny: `~/.config/pcm2www/config.json`

---

## Struktura konfiguracji

```json
{
  "database": {
    "driver": "sqlite",
    "path": "~/.config/pcm2www/pcm2www.db",
    "dsn": ""
  },
  "integrations": {
    "woocommerce": {
      "base_url": "https://new...",
      "consumer_key": "ck_xxx",
      "consumer_secret": "GGoO .... .... .... ....",
      "poll_sec": 10,
      "cache": {
        "prime_on_start": true,
        "sweep_interval_minutes": 360,
        "fields": "id,sku,name,regular_price,sale_price,stock_quantity,manage_stock,status,global_unique_id,date_modified_gmt,type"
      },
      "custom_fields": [
        {
          "code": "hurt_price",
          "read_top_level": "hurt_price",
          "read_meta_key": "_hurt_price",
          "write_top_level": "hurt_price",
          "write_meta_key": "_hurt_price"
        }
      ]
    },
    "importer": {
      "watch_dir": "~/pcm2www/imports",
      "poll_sec": 5
    }
  },
  "auto_start": true,
  "sync_interval_seconds": 10
}
```

Integracja składa się z trzech głównych sekcji:

- **database** – wybór silnika bazy danych (`sqlite` / `postgres` / `mysql`)
- **integrations.woocommerce** – ustawienia połączenia z WooCommerce
- **integrations.importer** – ustawienia importu plików z PC-Market
- **auto_start, sync_interval_seconds** – parametry globalne

## Baza danych

Sekcja `database` pozwala przełączać backend danych:

- `driver: "sqlite"` – lokalny plik bazy (`path`), domyślny tryb.
- `driver: "postgres"` – połączenie po `dsn`.
- `driver: "mysql"` – połączenie po `dsn`.

Przykłady:

```json
{
  "database": {
    "driver": "postgres",
    "dsn": "host=127.0.0.1 user=pcm password=pcm dbname=pcm2www port=5432 sslmode=disable TimeZone=UTC"
  }
}
```

```json
{
  "database": {
    "driver": "mysql",
    "dsn": "pcm:pcm@tcp(127.0.0.1:3306)/pcm2www?parseTime=true&loc=UTC"
  }
}
```

> Zmiana `database.*` wymaga restartu aplikacji (reload configu nie przełącza aktywnego połączenia DB w locie).

## Parametry globalne

- **auto_start** – integrator startuje automatycznie po uruchomieniu aplikacji.
- **sync_interval_seconds** – globalny interwał heartbeat syncera, tutaj co **10 sekund**.

---

## Integracja WooCommerce

### Połączenie z API

- **base_url** – adres sklepu WooCommerce (REST API).
- **consumer_key** i **consumer_secret** – klucze API wygenerowane w WooCommerce.
- **poll_sec** – interwał pętli integracji WooCommerce (heartbeat), tutaj co **10 sekund**.

### Konfiguracja cache

Sekcja `cache` określa sposób buforowania danych produktów z WooCommerce:

- **prime_on_start** – przy starcie pobierany jest pełny stan produktów z Woo (paginowany, 100/stronę).
- **sweep_interval_minutes** – przyrostowe odświeżanie cache co **360 minut (6h)** – tylko produkty zmienione od ostatniego sweep (timestamp w tabeli `kvs`).
- **fields** – lista pól produktów pobieranych z WooCommerce:
  - id, sku – identyfikatory
  - name – nazwa produktu
  - regular_price, sale_price – ceny
  - stock_quantity, manage_stock – stany magazynowe
  - stock_status – `instock` / `outofstock` / `onbackorder`
  - backorders – `no` / `notify` / `yes`
  - status – status produktu (np. publish / draft)
  - global_unique_id – pole Woo "GTIN, UPC, EAN, lub ISBN"
  - date_modified_gmt – data ostatniej modyfikacji
  - type – typ produktu (np. simple, variable)

> `stock_status` i `backorders` są zawsze dołączane do zapytań API niezależnie od wartości `fields` w konfiguracji.

### Pola customowe

- **custom_fields** – lista mapowań dla customowych pól Woo/meta.
- Dla każdego pola można wskazać:
  - `read_top_level` – nazwę pola top-level zwracanego przez REST API
  - `read_meta_key` – klucz w `meta_data`
  - `write_top_level` – nazwę pola top-level używanego przy `PUT`
  - `write_meta_key` – klucz meta używany przy `PUT`
- Domyślny przykład: `hurt_price`, korzysta z meta `_hurt_price`.

### Worker wysyłki (kolejka `woo_tasks`)

Worker działa w tle i przetwarza kolejkę zadań atomicznie (claim → execute → verify → sync cache). Obsługiwane typy tasków:

| Kind | Opis | Polityki skip |
|---|---|---|
| `ean.update` | Ustawienie EAN produktu w Woo | Skip jeśli produkt już ma jakikolwiek EAN; skip jeśli EAN zajęty przez inny produkt |
| `stock.update` | Aktualizacja stanu magazynowego | Skip jeśli `cena_detal=0`; skip jeśli `manage_stock=false`; skip jeśli stan już się zgadza; skip jeśli PCM nie zmienił stanu od poprzedniego importu |
| `price.update` | Aktualizacja ceny regularnej, hurtowej i klasy podatkowej (`tax_class`) | Skip jeśli `cena_detal=0`; skip jeśli aktywna `sale_price > 0`; skip jeśli cena i klasa podatkowa już się zgadzają |
| `availability.update` | Zarządzanie dostępnością produktu w sklepie | Skip jeśli stan w Woo już jest zgodny z oczekiwanym |

**Logika dostępności (cena_detal z PCM):**

| cena_detal | Akcja w Woo |
|---|---|
| `= 0` | `manage_stock=false` + `stock_status=outofstock` (produkt niedostępny, brak śledzenia) |
| `> 0` | `manage_stock=true` + `backorders=notify` (śledź stan, zamówienia oczekujące z powiadomieniem) |

#### Ochrona przed nadpisaniem sprzedaży online

`st_stocks` przechowuje kolumnę `stan_prev` — poprzednią wartość stanu PCM przed ostatnim upsertem (NULL przy pierwszym imporcie produktu). Planner porównuje `stan` z `stan_prev`: jeśli są równe, PCM nie zmienił stanu od ostatniego eksportu, więc różnica w cache Woo prawdopodobnie wynika ze sprzedaży w sklepie — task `stock.update` nie jest generowany. Jeśli PCM zmienił stan (np. pracownik zrobił korektę lub przyjął dostawę), delta ≠ 0 i task jest generowany z wartością absolutną z PCM.

Każdy task jest weryfikowany po aktualizacji (GET po PUT). Nieudane taski są requeue'owane.
**Tworzenie nowych produktów w Woo jest [NIEGOTOWE].**

#### Stawki podatkowe

Podczas `price.update` ustawiana jest klasa podatkowa produktu na podstawie `vat_id` z PCM (`vatIDToTaxClass` w plannerze). Mapowanie:

| vat_id (PCM) | Klasa podatkowa w Woo (`tax_class`) |
|---|---|
| `2300` | `"2300"` (23%) |
| `800` | `"800"` (8%) |
| `500` | `"500"` (5%) |
| `0` | `"zero-rate"` (0%) |
| `-1` | `"zero-rate"` (ZW) |
| inny / brak | `""` (standard rate — fallback 23%) |

Pole `TaxClass` jest trzymane w `WooProductCache` i synchronizowane przez `syncCacheFromVerifiedProduct`.

---

## Importer (PCM → Woo)

Sekcja `importer` odpowiada za pobieranie danych z PC-Market:

- **watch_dir** – katalog, w którym PCM umieszcza eksporty. W tej konfiguracji: `~/pcm2www/imports`.
- **poll_sec** – co ile sekund sprawdzany jest katalog importu, tutaj co **5 sekund**.

Aktualnie parsowany format: `exp_wyk_*.xml`. Inne typy eksportów PCM (`exp_dok_*` itp.) są **[NIEGOTOWE]**.

Dedulikacja pliku odbywa się przez SHA256, nazwę pliku i `transmisja_id`. Obsługiwane kodowania: ISO-8859-2, Windows-1250 i inne.

---

## Przepływ danych

```
PC-Market 7
    └─ generuje exp_wyk_*.xml do watch_dir
           ↓ co poll_sec sekund
    [Importer] – SHA256 dedup, parsowanie XML, batch upsert
    ├─ st_products (staging produktów)
    └─ st_stocks (stany wg magazynów)
           ↓ po każdym imporcie
    [Linker] – dopasowanie EAN: st_products.kod ↔ woo_product_caches.ean
    └─ link_issues (diagnostyki: brak EAN, duplikaty, brak w sklepie)
           ↓
    [Planner] – porównanie staging vs cache, generowanie woo_tasks
    ├─ ean.update (jeśli EAN produktu niezgodny lub brak w Woo)
    ├─ stock.update (jeśli stan się różni AND PCM zmienił stan od ostatniego importu)
    └─ price.update (jeśli cena różni się i brak aktywnej promocji)
           ↓
    [Worker] – claim → fetch → verify → PUT → verify → sync cache
    └─ woo_product_caches (aktualizowany po weryfikacji)
           ↓
    WooCommerce REST API
```

Cache Woo odświeżany jest niezależnie:
- pełny paginowany odczyt przy starcie (`prime_on_start=true`),
- przyrostowe odświeżanie co `sweep_interval_minutes`.

---

## Podsumowanie statusu implementacji

| Funkcja | Status |
|---|---|
| Import `exp_wyk_*.xml` | Działa |
| Dedup plików (SHA256, transmisja_id) | Działa |
| Staging `st_products`, `st_stocks` | Działa |
| Cache WooCommerce (prime + sweep) | Działa |
| Linkowanie EAN (PCM ↔ Woo) | Działa |
| Planowanie tasków (planner) | Działa |
| Worker `stock.update` do Woo | Działa (batch 20) |
| Worker `ean.update` do Woo | Działa (sekwencyjnie) |
| Worker `price.update` do Woo | Działa (batch 20) |
| Worker `availability.update` do Woo | Działa (sekwencyjnie) |
| Równoległe workery (`workers` w config) | Działa (domyślnie 3) |
| Synchronizacja klasy podatkowej (`tax_class`) | Działa |
| Tworzenie nowych produktów w Woo | NIEGOTOWE |
| Import innych typów eksportów PCM | NIEGOTOWE |
| Pobieranie zamówień z Woo | NIEGOTOWE |
