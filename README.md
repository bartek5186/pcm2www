# Konfiguracja integracji PCM2WWW z WooCommerce

Ten plik konfiguracyjny opisuje integrację systemu **PC-Market 7 (PCM)** poprzez narzędzie **pcm2www** z platformą **WooCommerce**.  
Integrator działa cyklicznie, pobiera dane z katalogu eksportów PC-Market (`exp_*`) oraz synchronizuje je z WooCommerce przy użyciu REST API.

Integrator posiada narzędzie CLI, tak samo jak narzędzie Desktopowe (Windows)
Plik konfiguracyjny wrzucamy w ~/.config/pcm2www/config.json


---

## Struktura konfiguracji

```json
{
  "integrations": {
    "woocommerce": {
      "base_url": "https://new...",
      "username": "bartek5186",
      "consumer_key": "ck_xxx",
      "consumer_secret": "GGoO .... .... .... ....",
      "poll_sec": 10,
      "cache": {
        "prime_on_start": true,
        "sweep_interval_minutes": 360,
        "sweep_stock_only_minutes": 120,
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

- **integrations.woocommerce** – ustawienia połączenia z WooCommerce
- **integrations.importer** – ustawienia importu plików z PC-Market
- **auto_start, sync_interval_seconds** – parametry globalne

## Parametry globalne

- **auto_start** – integrator startuje automatycznie po uruchomieniu aplikacji.  
- **sync_interval_seconds** – co ile sekund przetwarzane są zadania synchronizacji w kolejce (np. wysyłka zmian do WooCommerce).  
  → W tym przypadku co **10 sekund**.

---

## Integracja WooCommerce

### Połączenie z API

- **base_url** – adres sklepu WooCommerce (REST API).  
- **username** – nazwa użytkownika integracyjnego WooCommerce.  
- **consumer_key** i **consumer_secret** – klucze API wygenerowane w WooCommerce (używane do autoryzacji).  
- **poll_sec** – częstotliwość sprawdzania kolejki zadań dla WooCommerce, tutaj co **10 sekund**.

### Konfiguracja cache

Sekcja `cache` określa sposób buforowania danych produktów z WooCommerce:

- **prime_on_start** – przy pierwszym uruchomieniu pobierany jest pełny stan produktów z Woo (pełna inicjalizacja cache).  
- **sweep_interval_minutes** – pełne odświeżanie cache produktów co **360 minut (6h)**.  
- **sweep_stock_only_minutes** – odświeżanie tylko stanów magazynowych co **120 minut (2h)**.  
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

- **watch_dir** – katalog, w którym PCM umieszcza eksporty (np. exp_wyk_*.xml, exp_dok_*.xml).  
  W tej konfiguracji: `~/pcm2www/imports`.  
- **poll_sec** – co ile sekund sprawdzany jest katalog importu, tutaj co **5 sekund**.

---

## Przepływ danych

1. **PC-Market 7** generuje pliki eksportu (exp_wyk, exp_dok, itd.) do katalogu `~/pcm2www/imports`.  
2. **Importer** monitoruje katalog i wczytuje nowe pliki XML (co 5 sekund).  
3. Dane są zapisywane do lokalnej bazy/cache integratora.  
4. **WooCommerce worker** co 10 sekund sprawdza różnice i wysyła zmiany do WooCommerce REST API.  
5. **Cache WooCommerce** jest odświeżany:
   - pełny stan co 6h,  
   - stany magazynowe co 2h,  
   - dodatkowo pełna synchronizacja przy starcie (`prime_on_start=true`).  

---

## Podsumowanie

- Integrator działa w trybie **ciągłej synchronizacji**.  
- Zmiany w PCM (towary, stany, ceny) są widoczne w WooCommerce niemal w czasie rzeczywistym.  
- Harmonogram zapewnia równowagę pomiędzy **aktualnością danych** a **wydajnością**:
  - pliki z PCM → WooCommerce: co 5s  
  - zadania do WooCommerce: co 10s  
  - cache pełny: co 6h  
  - cache stanów: co 2h  

Dzięki temu integracja jest szybka, a jednocześnie minimalizuje obciążenie WooCommerce.
