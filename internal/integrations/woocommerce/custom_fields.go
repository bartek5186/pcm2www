package woocommerce

import "strings"

type CustomFieldConfig struct {
	Code          string `json:"code"`
	ReadTopLevel  string `json:"read_top_level,omitempty"`
	ReadMetaKey   string `json:"read_meta_key,omitempty"`
	WriteTopLevel string `json:"write_top_level,omitempty"`
	WriteMetaKey  string `json:"write_meta_key,omitempty"`
}

func defaultCustomFieldConfigs() []CustomFieldConfig {
	return []CustomFieldConfig{
		{
			Code:          "hurt_price",
			ReadTopLevel:  "hurt_price",
			ReadMetaKey:   "_hurt_price",
			WriteTopLevel: "hurt_price",
			WriteMetaKey:  "_hurt_price",
		},
	}
}

func mergeCustomFieldConfigs(configured []CustomFieldConfig) []CustomFieldConfig {
	merged := make(map[string]CustomFieldConfig)
	order := make([]string, 0, len(configured)+1)

	upsert := func(cfg CustomFieldConfig, overlay bool) {
		cfg = normalizeCustomFieldConfig(cfg)
		if cfg.Code == "" {
			return
		}
		if _, ok := merged[cfg.Code]; !ok {
			order = append(order, cfg.Code)
			merged[cfg.Code] = cfg
			return
		}
		if !overlay {
			return
		}
		base := merged[cfg.Code]
		if cfg.ReadTopLevel != "" {
			base.ReadTopLevel = cfg.ReadTopLevel
		}
		if cfg.ReadMetaKey != "" {
			base.ReadMetaKey = cfg.ReadMetaKey
		}
		if cfg.WriteTopLevel != "" {
			base.WriteTopLevel = cfg.WriteTopLevel
		}
		if cfg.WriteMetaKey != "" {
			base.WriteMetaKey = cfg.WriteMetaKey
		}
		merged[cfg.Code] = base
	}

	for _, cfg := range defaultCustomFieldConfigs() {
		upsert(cfg, false)
	}
	for _, cfg := range configured {
		upsert(cfg, true)
	}

	out := make([]CustomFieldConfig, 0, len(order))
	for _, code := range order {
		out = append(out, merged[code])
	}
	return out
}

func normalizeCustomFieldConfig(cfg CustomFieldConfig) CustomFieldConfig {
	cfg.Code = strings.TrimSpace(cfg.Code)
	cfg.ReadTopLevel = strings.TrimSpace(cfg.ReadTopLevel)
	cfg.ReadMetaKey = strings.TrimSpace(cfg.ReadMetaKey)
	cfg.WriteTopLevel = strings.TrimSpace(cfg.WriteTopLevel)
	cfg.WriteMetaKey = strings.TrimSpace(cfg.WriteMetaKey)
	return cfg
}

func (w *Woo) effectiveCustomFieldConfigs() []CustomFieldConfig {
	return mergeCustomFieldConfigs(w.cfg.CustomFields)
}

func (w *Woo) productFields() string {
	return ensureProductFields(w.cfg.Cache.Fields, w.effectiveCustomFieldConfigs())
}

func ensureProductFields(fields string, customFields []CustomFieldConfig) string {
	required := []string{
		"id",
		"sku",
		"name",
		"regular_price",
		"sale_price",
		"stock_quantity",
		"manage_stock",
		"stock_status",
		"backorders",
		"status",
		"date_modified_gmt",
		"type",
		"global_unique_id",
		"ean",
	}

	needMeta := false
	for _, cfg := range customFields {
		if cfg.ReadTopLevel != "" {
			required = append(required, cfg.ReadTopLevel)
		}
		if cfg.ReadMetaKey != "" || cfg.WriteMetaKey != "" {
			needMeta = true
		}
	}
	if needMeta {
		required = append(required, "meta_data")
	}

	seen := make(map[string]struct{}, len(required))
	out := make([]string, 0, len(required))

	for _, field := range strings.Split(fields, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if _, ok := seen[field]; ok {
			continue
		}
		seen[field] = struct{}{}
		out = append(out, field)
	}

	for _, field := range required {
		if _, ok := seen[field]; ok {
			continue
		}
		out = append(out, field)
	}

	return strings.Join(out, ",")
}

func (w *Woo) customFieldConfig(code string) (CustomFieldConfig, bool) {
	code = strings.TrimSpace(code)
	for _, cfg := range w.effectiveCustomFieldConfigs() {
		if cfg.Code == code {
			return cfg, true
		}
	}
	return CustomFieldConfig{}, false
}

func (w *Woo) customFieldValue(product wcProduct, code string) string {
	cfg, ok := w.customFieldConfig(code)
	if !ok {
		return ""
	}

	for _, field := range []string{cfg.ReadTopLevel, cfg.WriteTopLevel} {
		if field == "" {
			continue
		}
		if value := product.topLevelValue(field); value != "" {
			return value
		}
	}
	for _, key := range []string{cfg.ReadMetaKey, cfg.WriteMetaKey} {
		if key == "" {
			continue
		}
		if value := product.metaValue(key); value != "" {
			return value
		}
	}
	return ""
}

func (w *Woo) applyCustomFieldPayload(body map[string]any, code, value string) {
	cfg, ok := w.customFieldConfig(code)
	if !ok {
		return
	}

	if cfg.WriteTopLevel != "" {
		body[cfg.WriteTopLevel] = value
	}
	if cfg.WriteMetaKey == "" {
		return
	}

	row := map[string]any{
		"key":   cfg.WriteMetaKey,
		"value": value,
	}
	if raw, ok := body["meta_data"]; ok {
		if items, castOK := raw.([]map[string]any); castOK {
			body["meta_data"] = append(items, row)
			return
		}
	}
	body["meta_data"] = []map[string]any{row}
}
