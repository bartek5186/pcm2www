// internal/integrations/woocommerce/types.go
package woocommerce

type wcProduct struct {
	ID           int64   `json:"id"`
	Name         string  `json:"name"`
	SKU          string  `json:"sku"`
	EAN          string  `json:"ean"`
	Status       string  `json:"status"`        // "publish","draft","trash"
	RegularPrice string  `json:"regular_price"` // string w Woo
	SalePrice    string  `json:"sale_price"`    // string
	HurtPrice    string  `json:"hurt_price"`
	ManageStock  bool    `json:"manage_stock"`
	StockQty     float64 `json:"stock_quantity"`
	Type         string  `json:"type"` // "simple","variable", etc.
	DateModified string  `json:"date_modified_gmt"`
}
