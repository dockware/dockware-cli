package clone

import (
	"sort"
	"strings"
	"time"
)

// orderParents are the tables whose kept rows other order tables point to,
// via a <table>_id column (checked after the generic order_id rule).
var orderParents = []string{"order_delivery", "order_line_item", "order_transaction"}

// orderHistoryEntities are the state_machine_history entity names whose
// referenced rows are windowed; all other history rows are kept.
var orderHistoryEntities = []string{"order", "order_delivery", "order_transaction"}

// OrderWindowWhere restricts the export to orders placed within the last days
// before now and to the rows that hang off them (addresses, line items,
// deliveries, transactions, captures, refunds, documents, tags, state history
// and any extension table with a binary order_id), so a shop with years of
// history clones in minutes. The cutoff is one literal (midnight UTC, days
// days ago) embedded in every condition: the queries of the child tables run
// after the order query, and a cutoff evaluated per query would keep orders
// without their rows. Every version row of a kept order is kept, as the ids
// are compared without version. Conditions use unqualified table names, which
// both strategies run with the live database as default schema. Tables or
// columns missing from the probe are left unfiltered; days <= 0 or a schema
// without order.order_date_time means no window (nil).
func OrderWindowWhere(p *Probe, days int, now time.Time) map[string]string {
	if days <= 0 || p.Schema["order"]["order_date_time"].Name == "" {
		return nil
	}
	y, m, d := now.UTC().Date()
	cutoff := time.Date(y, m, d, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -days).Format("2006-01-02 15:04:05")
	where := map[string]string{
		"order": "order_date_time >= '" + cutoff + "'",
	}
	kept := func(table string) string {
		return "(SELECT `" + table + "`.`id` FROM `" + table + "` WHERE " + where[table] + ")"
	}
	// ref restricts table to rows whose binary column points into the kept
	// rows of parent; NULL references (optional links) are kept.
	ref := func(table, column, parent string) {
		col, ok := p.Schema[table][column]
		if !ok || where[table] != "" || where[parent] == "" || !isBinary(col.DataType) {
			return
		}
		cond := "`" + column + "` IN " + kept(parent)
		if col.Nullable {
			cond = "(`" + column + "` IS NULL OR " + cond + ")"
		}
		where[table] = cond
	}
	tables := make([]string, 0, len(p.Schema))
	for t := range p.Schema {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	for _, t := range tables {
		if t != "order" {
			ref(t, "order_id", "order")
		}
	}
	ref("order_transaction_capture", "order_transaction_id", "order_transaction")
	ref("order_transaction_capture_refund", "capture_id", "order_transaction_capture")
	ref("order_transaction_capture_refund_position", "refund_id", "order_transaction_capture_refund")
	for _, t := range tables {
		for _, parent := range orderParents {
			ref(t, parent+"_id", parent)
		}
	}

	// Shopware 6.5.7+ stores the referenced row in referenced_id; older
	// schemas only have the JSON entity_id and keep their full history.
	hist := p.Schema["state_machine_history"]
	if hist["entity_name"].Name != "" && isBinary(hist["referenced_id"].DataType) {
		var names, branches []string
		for _, t := range orderHistoryEntities {
			if where[t] != "" {
				names = append(names, "'"+t+"'")
				branches = append(branches, "(`entity_name` = '"+t+"' AND `referenced_id` IN "+kept(t)+")")
			}
		}
		where["state_machine_history"] = "`entity_name` NOT IN (" + strings.Join(names, ",") + ") OR " + strings.Join(branches, " OR ")
	}
	return where
}

func isBinary(dataType string) bool { return dataType == "binary" || dataType == "varbinary" }
