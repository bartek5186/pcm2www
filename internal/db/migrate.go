package db

// Migrate tworzy/aktualizuje schemat bazy.
// Wołaj przy starcie aplikacji – jeśli baza nie istnieje, zostanie utworzona.
func (h *Handle) Migrate() error {
	return h.DB.AutoMigrate(
		&ImportFile{},
		&StProduct{},
		&StStock{},
		&WooProductCache{},
		&WooTask{},
		&KV{},
	)
}
