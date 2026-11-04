package clone

import (
	"strings"
	"testing"
	"time"
)

// windowNow is a fixed clock for the order window tests: 2026-11-03 15:04:05 UTC.
var windowNow = time.Date(2026, time.November, 3, 15, 4, 5, 0, time.UTC)

// orderSchema builds a probe schema; a column name ending in "?" is nullable.
// Columns named id or *_id are binary(16), order_date_time is datetime,
// everything else varchar.
func orderSchema(tables map[string][]string) Schema {
	s := Schema{}
	for t, cols := range tables {
		s[t] = map[string]Column{}
		for _, c := range cols {
			nullable := strings.HasSuffix(c, "?")
			c = strings.TrimSuffix(c, "?")
			dt := "varchar"
			switch {
			case c == "id" || strings.HasSuffix(c, "_id"):
				dt = "binary"
			case c == "order_date_time":
				dt = "datetime"
			}
			s[t][c] = Column{Table: t, Name: c, Nullable: nullable, DataType: dt}
		}
	}
	return s
}

func TestOrderWindowWhereOff(t *testing.T) {
	p := &Probe{Schema: orderSchema(map[string][]string{"order": {"id", "order_date_time"}, "order_line_item": {"id", "order_id"}})}
	if w := OrderWindowWhere(p, 0, windowNow); w != nil {
		t.Errorf("days=0 must not filter, got %v", w)
	}
	noDate := &Probe{Schema: orderSchema(map[string][]string{"order": {"id"}, "order_line_item": {"id", "order_id"}})}
	if w := OrderWindowWhere(noDate, 30, windowNow); w != nil {
		t.Errorf("without order_date_time nothing can be windowed, got %v", w)
	}
}

func TestOrderWindowWhereUsesOneUTCDayCutoff(t *testing.T) {
	p := &Probe{Schema: orderSchema(map[string][]string{"order": {"id", "order_date_time"}, "order_line_item": {"id", "order_id"}})}
	// 01:30 on the 4th in UTC+2 is still the 3rd in UTC; 30 days back from midnight UTC
	local := time.Date(2026, time.November, 4, 1, 30, 0, 0, time.FixedZone("CEST", 2*60*60))
	w := OrderWindowWhere(p, 30, local)
	cutoff := "'2026-10-04 00:00:00'"
	if w["order"] != "order_date_time >= "+cutoff {
		t.Errorf("order = %q, want the day-truncated UTC literal", w["order"])
	}
	// the same literal is embedded in the child table, never re-evaluated by MySQL
	if !strings.Contains(w["order_line_item"], cutoff) || strings.Contains(w["order_line_item"], "UTC_TIMESTAMP") {
		t.Errorf("order_line_item = %q, want the literal cutoff", w["order_line_item"])
	}
}

func TestOrderWindowWhereFollowsTheOrderChain(t *testing.T) {
	p := &Probe{Schema: orderSchema(map[string][]string{
		"order":                                     {"id", "version_id", "order_date_time"},
		"order_line_item":                           {"id", "order_id"},
		"order_delivery":                            {"id", "order_id"},
		"order_delivery_position":                   {"id", "order_delivery_id", "order_line_item_id"},
		"order_transaction":                         {"id", "order_id"},
		"order_transaction_capture":                 {"id", "order_transaction_id"},
		"order_transaction_capture_refund":          {"id", "capture_id"},
		"order_transaction_capture_refund_position": {"id", "refund_id", "order_line_item_id?"},
		"document":                                  {"id", "order_id"},
		"acme_note":                                 {"id", "order_id?", "note"},
		"acme_counter":                              {"id", "order_id", "count"},
		"state_machine_history":                     {"id", "entity_name", "referenced_id"},
		"product":                                   {"id", "product_number"},
	})}
	p.Schema["acme_counter"]["order_id"] = Column{Table: "acme_counter", Name: "order_id", DataType: "int"}
	w := OrderWindowWhere(p, 30, windowNow)

	root := "order_date_time >= '2026-10-04 00:00:00'"
	if w["order"] != root {
		t.Fatalf("order = %q", w["order"])
	}
	keptOrders := "`order_id` IN (SELECT `order`.`id` FROM `order` WHERE " + root + ")"
	for _, tbl := range []string{"order_line_item", "order_delivery", "order_transaction", "document"} {
		if w[tbl] != keptOrders {
			t.Errorf("%s = %q, want %q", tbl, w[tbl], keptOrders)
		}
	}
	// chain tables are filtered through their parent's condition
	chain := map[string]string{
		"order_delivery_position":                   "`order_delivery_id` IN (SELECT `order_delivery`.`id` FROM `order_delivery` WHERE " + w["order_delivery"] + ")",
		"order_transaction_capture":                 "`order_transaction_id` IN (SELECT `order_transaction`.`id` FROM `order_transaction` WHERE " + w["order_transaction"] + ")",
		"order_transaction_capture_refund":          "`capture_id` IN (SELECT `order_transaction_capture`.`id` FROM `order_transaction_capture` WHERE " + w["order_transaction_capture"] + ")",
		"order_transaction_capture_refund_position": "`refund_id` IN (SELECT `order_transaction_capture_refund`.`id` FROM `order_transaction_capture_refund` WHERE " + w["order_transaction_capture_refund"] + ")",
	}
	for tbl, want := range chain {
		if w[tbl] != want {
			t.Errorf("%s = %q, want %q", tbl, w[tbl], want)
		}
	}
	// optional references keep their NULL rows; non-binary order_id is not a reference
	if w["acme_note"] != "(`order_id` IS NULL OR "+keptOrders+")" {
		t.Errorf("acme_note = %q", w["acme_note"])
	}
	for _, tbl := range []string{"acme_counter", "product", "order_tag"} {
		if _, ok := w[tbl]; ok {
			t.Errorf("%s must not be filtered: %q", tbl, w[tbl])
		}
	}
	// history of other entities stays, order entities follow their rows
	h := w["state_machine_history"]
	if !strings.HasPrefix(h, "`entity_name` NOT IN ('order','order_delivery','order_transaction') OR ") ||
		!strings.Contains(h, "(`entity_name` = 'order_delivery' AND `referenced_id` IN (SELECT `order_delivery`.`id` FROM `order_delivery` WHERE "+w["order_delivery"]+"))") {
		t.Errorf("state_machine_history = %q", h)
	}
}

func TestOrderWindowWhereKeepsHistoryWithoutReferencedID(t *testing.T) {
	// Shopware < 6.5.7: state_machine_history only has the JSON entity_id
	p := &Probe{Schema: orderSchema(map[string][]string{
		"order":                 {"id", "order_date_time"},
		"state_machine_history": {"id", "entity_name", "entity_id"},
	})}
	p.Schema["state_machine_history"]["entity_id"] = Column{Table: "state_machine_history", Name: "entity_id", DataType: "json"}
	if w := OrderWindowWhere(p, 7, windowNow); w["state_machine_history"] != "" {
		t.Errorf("history without referenced_id must stay unfiltered: %q", w["state_machine_history"])
	}
}

func TestAddWhereKeepsExistingCondition(t *testing.T) {
	p := &Probe{Schema: orderSchema(map[string][]string{"system_config": {"id", "configuration_key"}, "order": {"id", "order_date_time"}})}
	plan := NewDumpPlan(p, Classification{}, nil, nil)
	plan.AddWhere("system_config", "sales_channel_id IS NULL")
	plan.AddWhere("order", "id IS NOT NULL")
	if plan.Where["system_config"] != "("+SystemConfigWhere()+") AND (sales_channel_id IS NULL)" {
		t.Errorf("system_config = %q", plan.Where["system_config"])
	}
	if plan.Where["order"] != "id IS NOT NULL" {
		t.Errorf("order = %q", plan.Where["order"])
	}
}

func TestShadowSQLAppliesWhereAgainstTheLiveSchema(t *testing.T) {
	p := &Probe{Database: "shop", Tables: []string{"order", "order_line_item"}, Schema: orderSchema(map[string][]string{
		"order":           {"id", "order_date_time"},
		"order_line_item": {"id", "order_id"},
	})}
	plan := DumpPlan{Where: OrderWindowWhere(p, 30, windowNow)}
	sql := ShadowSQL(p, Classification{}, plan, "shop_dwc_shadow", true)
	want := "INSERT INTO `shop_dwc_shadow`.`order_line_item` (`id`,`order_id`) SELECT `id`,`order_id` FROM `shop`.`order_line_item` WHERE " + plan.Where["order_line_item"] + ";"
	if !strings.Contains(sql, want) {
		t.Errorf("shadow copy lacks the window:\n%s", sql)
	}
}
