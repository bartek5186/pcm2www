// internal/integrations/woocommerce/types.go
package woocommerce

import (
	"bytes"
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
	ID                int64                      `json:"id"`
	Name              string                     `json:"name"`
	SKU               string                     `json:"sku"`
	GlobalUniqueID    string                     `json:"global_unique_id"`
	EAN               string                     `json:"ean"`
	Status            string                     `json:"status"`        // "publish","draft","trash"
	RegularPrice      string                     `json:"regular_price"` // string lub liczba w API, normalizowane do string
	SalePrice         string                     `json:"sale_price"`
	HurtPrice         string                     `json:"hurt_price"`
	TaxClass          string                     `json:"tax_class"`
	ManageStock       bool                       `json:"manage_stock"`
	StockQuantity     float64                    `json:"stock_quantity"`
	StockStatus       string                     `json:"stock_status"`       // instock / outofstock / onbackorder
	Backorders        string                     `json:"backorders"`         // no / notify / yes
	CatalogVisibility string                     `json:"catalog_visibility"` // visible / hidden / catalog / search
	Type              string                     `json:"type"`               // "simple","variable", etc.
	MetaData          []wcMetaData               `json:"meta_data"`
	DateModifiedGMT   string                     `json:"date_modified_gmt"`
	ExtraFields       map[string]json.RawMessage `json:"-"`
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
	// Woo extensions can return prices as JSON numbers instead of strings.
	// Shadow only these fields; keep normal type checking for the other data.
	fields := struct {
		*alias
		RegularPrice json.RawMessage `json:"regular_price"`
		SalePrice    json.RawMessage `json:"sale_price"`
		HurtPrice    json.RawMessage `json:"hurt_price"`
	}{alias: &decoded}
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, field := range []struct {
		name string
		raw  json.RawMessage
		dest *string
	}{
		{"regular_price", fields.RegularPrice, &decoded.RegularPrice},
		{"sale_price", fields.SalePrice, &decoded.SalePrice},
		{"hurt_price", fields.HurtPrice, &decoded.HurtPrice},
	} {
		value, err := decodeWooPrice(field.raw)
		if err != nil {
			return fmt.Errorf("decode %s: %w", field.name, err)
		}
		*field.dest = value
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
		"tax_class",
		"manage_stock",
		"stock_quantity",
		"stock_status",
		"backorders",
		"catalog_visibility",
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

func decodeWooPrice(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", nil
	}
	if raw[0] == '"' {
		var value string
		err := json.Unmarshal(raw, &value)
		return value, err
	}
	// Preserve the decimal representation without a float64 round trip.
	var value json.Number
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("expected a string, number or null: %w", err)
	}
	return value.String(), nil
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
