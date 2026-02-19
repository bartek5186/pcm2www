# Konfiguracja integracji PCM2WWW z WooCommerce

Ten plik konfiguracyjny opisuje integrację systemu **PC-Market 7 (PCM)** poprzez narzędzie **pcm2www** z platformą **WooCommerce**.  
Integrator działa cyklicznie, pobiera dane z katalogu eksportów PC-Market (`exp_*`) oraz synchronizuje je z WooCommerce przy użyciu REST API.

> **Status implementacji (2026-02-19):** funkcje oznaczone jako **[NIEGOTOWE]** lub **[CZĘŚCIOWO GOTOWE]** nie są jeszcze ukończone.

## Funkcjonalności

- 🚀 **Automatyczna synchronizacja** produktów i stanów magazynowych **[NIEGOTOWE: brak aktywnego workera wysyłki do WooCommerce]**  
- 🔄 **Obsługa cache** – pełne i częściowe odświeżanie danych z WooCommerce  
- 🗂️ **Import plików PCM** (`exp_wyk`, `exp_dok`, itp.) z katalogu wymiany **[CZĘŚCIOWO GOTOWE: obecnie przetwarzane są głównie pliki `exp_wyk_*.xml`]**  
- 🛒 **Integracja przez REST API** WooCommerce (create, update, stock update) **[NIEGOTOWE: create/update/stock update nie są jeszcze uruchomione]**  
- ⚙️ **Elastyczna konfiguracja** poprzez plik JSON
- 📡 **Ciągła praca w tle** – monitoring katalogu i cykliczne taski **[CZĘŚCIOWO GOTOWE: monitoring działa, taski wysyłki do Woo nie są jeszcze aktywne]**

---
Integrator posiada narzędzie CLI, tak samo jak narzędzie Desktopowe (Windows)
Plik konfiguracyjny wrzucamy w ~/.config/pcm2www/config.json


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
        "fields": "id,sku,name,regular_price,sale_price,stock_quantity,manage_stock,status,hurt_price,ean,date_modified_gmt,type"
      }
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
- **sync_interval_seconds** – globalny interwał pętli syncera (heartbeat). **[CZĘŚCIOWO GOTOWE: docelowe przetwarzanie kolejki wysyłek do WooCommerce nie jest jeszcze aktywne]**  
  → W tym przypadku co **10 sekund**.

---

## Integracja WooCommerce

### Połączenie z API

- **base_url** – adres sklepu WooCommerce (REST API).  
- **consumer_key** i **consumer_secret** – klucze API wygenerowane w WooCommerce (używane do autoryzacji).  
- **poll_sec** – interwał pętli integracji WooCommerce (aktualnie logowanie/ping), tutaj co **10 sekund**. **[NIEGOTOWE: kolejka `woo_tasks` nie jest jeszcze aktywnie przetwarzana]**

### Konfiguracja cache

Sekcja `cache` określa sposób buforowania danych produktów z WooCommerce:

- **prime_on_start** – przy pierwszym uruchomieniu pobierany jest pełny stan produktów z Woo (pełna inicjalizacja cache).  
- **sweep_interval_minutes** – pełne odświeżanie cache produktów co **360 minut (6h)**.  
- **fields** – lista pól produktów pobieranych z WooCommerce:  
  - id, sku – identyfikatory  
  - name – nazwa produktu  
  - regular_price, sale_price – ceny  
  - stock_quantity, manage_stock – stany magazynowe  
  - status – status produktu (np. publish / draft)  
  - hurt_price, ean – pola rozszerzone (dane hurtowe i kod EAN)  
  - date_modified_gmt – data ostatniej modyfikacji  
  - type – typ produktu (np. simple, variable)  

---

## Importer (PCM → Woo)

Sekcja `importer` odpowiada za pobieranie danych z PC-Market:

- **watch_dir** – katalog, w którym PCM umieszcza eksporty (np. exp_wyk_*.xml, exp_dok_*.xml). **[CZĘŚCIOWO GOTOWE: obecnie implementacja koncentruje się na `exp_wyk_*.xml`]**  
  W tej konfiguracji: `~/pcm2www/imports`.  
- **poll_sec** – co ile sekund sprawdzany jest katalog importu, tutaj co **5 sekund**.

---

## Przepływ danych

1. **PC-Market 7** generuje pliki eksportu (exp_wyk, exp_dok, itd.) do katalogu `~/pcm2www/imports`.  
2. **Importer** monitoruje katalog i wczytuje nowe pliki XML (co 5 sekund).  
3. Dane są zapisywane do lokalnej bazy/cache integratora.  
4. **WooCommerce worker** co 10 sekund sprawdza różnice i wysyła zmiany do WooCommerce REST API. **[NIEGOTOWE: worker nie jest obecnie uruchomiony]**  
5. **Cache WooCommerce** jest odświeżany:
   - pełny/paginowany odczyt produktów przy starcie (`prime_on_start=true`),  
   - odświeżanie przyrostowe co `sweep_interval_minutes`,  
   - osobny harmonogram stanów magazynowych co 2h **[NIEGOTOWE]**.  

---

## Podsumowanie

- Integrator działa w trybie **ciągłej pracy** (monitoring importu + cache Woo).  
- Synchronizacja zmian PCM → WooCommerce jest **[NIEGOTOWA]** (brak aktywnego workera wysyłki).  
- Aktualny harmonogram:
  - import plików z katalogu wymiany: wg `importer.poll_sec` (np. 5s),  
  - pętla integracji Woo: wg `woocommerce.poll_sec` (np. 10s, obecnie tryb dev/ping),  
  - odświeżanie cache Woo: wg `cache.sweep_interval_minutes` (np. 360 min).
