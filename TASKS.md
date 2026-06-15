Zadania:

- [x] Dodać opcję configu `integrations.importer.price_mode`, czy do WooCommerce wysyłać ceny brutto czy netto.
  - Domyślnie: `gross`, czyli cena brutto z PC-Market bez przeliczania.
  - Opcjonalnie: `net`, czyli przeliczenie ceny brutto na netto według `vat_id`.
- [x] Rozszerzyć test realnych XML o checkpointy po drodze i walidację stanu końcowego.
  - Sprawdzane są `st_products`, `st_stocks`, `stan`, `stan_prev`, `rezerwacja`, ceny, VAT i statusy.
  - Checkpoint domyślnie co 10 plików, zmieniany przez `PCM2WWW_IMPORT_XML_CHECKPOINT_EVERY`.
- [x] Testować krokowe pojawianie się plików tak jak u workera/importera.
  - Test kopiuje XML po kolei do `t.TempDir()` i po każdym pliku uruchamia `scanOnce`.
  - Fixture pozostają nietknięte w `imports/incoming_test` albo `imports`.
- [x] Zasymulować uszkodzony XML i zabezpieczyć brak częściowych staging rows.
  - Uszkodzony import dostaje `status=2` i `last_error`.
  - Kolejny poprawny XML nadal importuje się poprawnie.
- [x] Zasymulować nieudaną aktualizację w WooCommerce.
  - Test workerowego batch POST ustawia task na `error`.
  - Cache nie udaje, że nieudana aktualizacja ceny się powiodła.

Weryfikacja:

```bash
env GOCACHE=/tmp/pcm2www-go-build-cache go test ./...
env GOCACHE=/tmp/pcm2www-go-build-cache PCM2WWW_IMPORT_XML_FIXTURE_TESTS=1 go test ./internal/integrations/importer -run TestImportRealXMLSequence -count=1 -v
```
