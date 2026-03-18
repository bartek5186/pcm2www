package db

const (
	WooTaskKindEANUpdate          = "ean.update"
	WooTaskKindStockUpdate        = "stock.update"
	WooTaskKindPriceUpdate        = "price.update"
	WooTaskKindAvailabilityUpdate = "availability.update"
)

type WooEANUpdatePayload struct {
	ImportID    uint   `json:"import_id"`
	WooID       uint   `json:"woo_id"`
	TowarID     int64  `json:"towar_id"`
	SKU         string `json:"sku"`
	ProductName string `json:"product_name"`
	SourceKod   string `json:"source_kod"`
	CurrentEAN  string `json:"current_ean"`
	DesiredEAN  string `json:"desired_ean"`
}

type WooStockUpdatePayload struct {
	ImportID      uint    `json:"import_id"`
	WooID         uint    `json:"woo_id"`
	TowarID       int64   `json:"towar_id"`
	SKU           string  `json:"sku"`
	ProductName   string  `json:"product_name"`
	CurrentStock  float64 `json:"current_stock"`
	DesiredStock  float64 `json:"desired_stock"`
	StockManaged  bool    `json:"stock_managed"`
	SourceStock   float64 `json:"source_stock"`
	SourceReserve float64 `json:"source_reserve"`
}

// WooAvailabilityPayload steruje manage_stock / stock_status / backorders.
// Unavailable=true (cena_detal=0): manage_stock=false, stock_status=outofstock.
// Unavailable=false (cena_detal>0): manage_stock=true, backorders=notify.
type WooAvailabilityPayload struct {
	ImportID    uint   `json:"import_id"`
	WooID       uint   `json:"woo_id"`
	TowarID     int64  `json:"towar_id"`
	SKU         string `json:"sku"`
	ProductName string `json:"product_name"`
	Unavailable bool   `json:"unavailable"`
}

type WooPriceUpdatePayload struct {
	ImportID        uint    `json:"import_id"`
	WooID           uint    `json:"woo_id"`
	TowarID         int64   `json:"towar_id"`
	SKU             string  `json:"sku"`
	ProductName     string  `json:"product_name"`
	CurrentRegular  float64 `json:"current_regular"`
	DesiredRegular  float64 `json:"desired_regular"`
	CurrentSale     float64 `json:"current_sale"`
	CurrentHurt     float64 `json:"current_hurt"`
	DesiredHurt     float64 `json:"desired_hurt"`
	CurrentTaxClass string  `json:"current_tax_class"`
	DesiredTaxClass string  `json:"desired_tax_class"`
}
