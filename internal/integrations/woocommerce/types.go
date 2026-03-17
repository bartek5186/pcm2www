// internal/integrations/woocommerce/types.go
package woocommerce

import (
	"encoding/json"
	"fmt"
	"strings"
)

type wcMetaData struct {
	ID    int64  `json:"id,omitempty"`
	Key   string `json:"key"`
	Value any    `json:"value"`
}

type wcProduct struct {
	ID              int64                      `json:"id"`
	Name            string                     `json:"name"`
	SKU             string                     `json:"sku"`
	GlobalUniqueID  string                     `json:"global_unique_id"`
	EAN             string                     `json:"ean"`
	Status          string                     `json:"status"`        // "publish","draft","trash"
	RegularPrice    string                     `json:"regular_price"` // string w Woo
	SalePrice       string                     `json:"sale_price"`    // string
	HurtPrice       string                     `json:"hurt_price"`
	ManageStock     bool                       `json:"manage_stock"`
	StockQuantity   float64                    `json:"stock_quantity"`
	Type            string                     `json:"type"` // "simple","variable", etc.
	MetaData        []wcMetaData               `json:"meta_data"`
	DateModifiedGMT string                     `json:"date_modified_gmt"`
	ExtraFields     map[string]json.RawMessage `json:"-"`
}

func (p wcProduct) cacheEAN() string {
	if s := strings.TrimSpace(p.GlobalUniqueID); s != "" {
		return s
	}
	return strings.TrimSpace(p.EAN)
}

func (p *wcProduct) UnmarshalJSON(data []byte) error {
	type alias wcProduct
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	for _, key := range []string{
		"id",
		"name",
		"sku",
		"global_unique_id",
		"ean",
		"status",
		"regular_price",
		"sale_price",
		"hurt_price",
		"manage_stock",
		"stock_quantity",
		"type",
		"meta_data",
		"date_modified_gmt",
	} {
		delete(raw, key)
	}

	*p = wcProduct(decoded)
	p.ExtraFields = raw
	return nil
}

func (p wcProduct) topLevelValue(key string) string {
	switch strings.TrimSpace(key) {
	case "name":
		return strings.TrimSpace(p.Name)
	case "sku":
		return strings.TrimSpace(p.SKU)
	case "global_unique_id":
		return strings.TrimSpace(p.GlobalUniqueID)
	case "ean":
		return strings.TrimSpace(p.EAN)
	case "status":
		return strings.TrimSpace(p.Status)
	case "regular_price":
		return strings.TrimSpace(p.RegularPrice)
	case "sale_price":
		return strings.TrimSpace(p.SalePrice)
	case "hurt_price":
		return strings.TrimSpace(p.HurtPrice)
	case "type":
		return strings.TrimSpace(p.Type)
	}

	raw, ok := p.ExtraFields[strings.TrimSpace(key)]
	if !ok || len(raw) == 0 {
		return ""
	}

	var value any
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return ""
	}
	if s := strings.TrimSpace(fmt.Sprint(value)); s != "" && s != "<nil>" {
		return s
	}
	return ""
}

func (p wcProduct) metaValue(key string) string {
	for _, meta := range p.MetaData {
		if meta.Key != strings.TrimSpace(key) || meta.Value == nil {
			continue
		}
		if s := strings.TrimSpace(fmt.Sprint(meta.Value)); s != "" && s != "<nil>" {
			return s
		}
	}
	return ""
}
