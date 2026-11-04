package clone

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// BuiltinAllowedTables are extension tables that hold configuration or content
// and no personal data. Everything else outside core is exported schema-only.
var BuiltinAllowedTables = []string{
	"swag_cms_extensions_*",
	"swag_language_pack_*",
	"swag_customized_products_template*",
	"swag_social_shopping_sales_channel",
}

// requiredCoreTables guards against a broken core detection (wrong path,
// unexpected vendor layout): without them the clone would be empty.
var requiredCoreTables = []string{"product", "order", "customer", "system_config", "sales_channel", "migration", "plugin"}

// Classification is the decision for every remote table.
type Classification struct {
	Core    []string // core tables, data exported (rules applied)
	Allowed []string // extension tables explicitly allowed
	Denied  []string // extension tables exported schema-only
	NoData  []string // tables exported schema-only for being transient/personal
}

// Classify sorts remote tables into exported and schema-only sets.
// Denied tables are kept as empty tables so plugins still boot.
func Classify(remote []string, core map[string]bool, allow []string, noData []string) (Classification, error) {
	for _, t := range requiredCoreTables {
		if !core[t] {
			return Classification{}, &Error{Step: StepProbe,
				Msg:  fmt.Sprintf("core table detection is incomplete: %q missing (found %d core tables)", t, len(core)),
				Hint: "core tables are read from the CREATE TABLE statements under vendor/shopware/ on the server: --path must be the project root (the directory with bin/console and composer.json) and vendor/shopware/core there must be a complete install (composer install on the server)"}
		}
	}
	var c Classification
	for _, t := range remote {
		switch {
		case matchAny(t, noData):
			c.NoData = append(c.NoData, t)
		case core[strings.ToLower(t)]:
			c.Core = append(c.Core, t)
		case matchAny(t, allow):
			c.Allowed = append(c.Allowed, t)
		default:
			c.Denied = append(c.Denied, t)
		}
	}
	sort.Strings(c.Core)
	sort.Strings(c.Allowed)
	sort.Strings(c.Denied)
	sort.Strings(c.NoData)
	return c, nil
}

// SchemaOnly returns every table whose rows are not exported.
func (c Classification) SchemaOnly() []string {
	out := append(append([]string(nil), c.NoData...), c.Denied...)
	sort.Strings(out)
	return out
}

func matchAny(table string, patterns []string) bool {
	t := strings.ToLower(table)
	for _, p := range patterns {
		if ok, _ := path.Match(strings.ToLower(p), t); ok {
			return true
		}
	}
	return false
}

// ParseCoreTables turns the remote core-table scan into a set.
func ParseCoreTables(out string) map[string]bool {
	set := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(strings.ToLower(l))
		if l != "" {
			set[l] = true
		}
	}
	return set
}
