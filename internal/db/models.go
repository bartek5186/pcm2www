// internal/db/models.go
package db

import "time"

// import_files
type ImportFile struct {
	ImportID     uint   `gorm:"primaryKey;column:import_id"`
	Filename     string `gorm:"uniqueIndex"`
	FileTimeUTC  string
	TransmisjaID string `gorm:"uniqueIndex"`
	SHA256       string `gorm:"uniqueIndex"`
	SizeBytes    int64
	Status       int       `gorm:"index"` // 0=pending, 1=done, 2=error
	LastError    string    `gorm:"type:text"`
	ReceivedAt   time.Time `gorm:"autoCreateTime"`
	ProcessedAt  *time.Time
}

// st_products (staging)
type StProduct struct {
	ImportID         uint   `gorm:"primaryKey"`
	TowarID          int64  `gorm:"primaryKey"`
	Kod              string `gorm:"index"`
	Nazwa            string
	Opis1            string
	VatID            int64
	KategoriaID      int64
	GrupaID          int64
	JmID             int64
	CenaDetal        float64
	CenaHurtowa      float64
	CenaNocna        float64
	CenaDodatkowa    float64
	CenaDetPrzedProm float64
	NajCena30Det     float64
	AktywnyWSI       bool
	DoUsuniecia      bool
	DataAktualizacji string
	FolderZdjec      string
	PlikZdjecia      string
}

// st_stock (staging)
type StStock struct {
	ImportID   uint  `gorm:"primaryKey"`
	TowarID    int64 `gorm:"primaryKey"`
	MagazynID  int64 `gorm:"primaryKey"`
	Stan       float64
	Rezerwacja float64
}

// woo_products_cache
type WooProductCache struct {
	WooID        uint   `gorm:"primaryKey"`
	TowarID      *int64 `gorm:"index"`
	Kod          string `gorm:"index"` // SKU
	Ean          string `gorm:"index"`
	Name         string
	PriceRegular float64
	PriceSale    float64
	HurtPrice    float64
	StockQty     float64
	StockManaged bool
	Status       string // publish/draft/trash
	Type         string
	DateModified string
}

// woo_tasks
type WooTask struct {
	TaskID      uint   `gorm:"primaryKey;column:task_id"`
	ImportID    uint   `gorm:"index"`
	Kind        string `gorm:"index"` // np. product.update, stock.update
	PayloadJSON string `gorm:"type:text"`
	DependsOn   *uint
	Status      string    `gorm:"index;default:pending"` // pending/done/error
	LastError   string    `gorm:"type:text"`
	CreatedAt   time.Time `gorm:"autoCreateTime"`
}

// internal/db/models.go
type KV struct {
	K string `gorm:"primaryKey"`
	V string
}
