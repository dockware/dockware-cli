package clone

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Kind describes how a column is anonymised. Every kind is expressed as one SQL
// expression, so the same rule set drives both strategies: shopware-cli uses it
// as a SELECT rewrite, the shadow strategy as INSERT … SELECT expression.
type Kind string

const (
	KindEmail     Kind = "email"
	KindFirstName Kind = "first_name"
	KindLastName  Kind = "last_name"
	KindCompany   Kind = "company"
	KindStreet    Kind = "street"
	KindZip       Kind = "zip"
	KindCity      Kind = "city"
	KindPhone     Kind = "phone"
	KindIP        Kind = "ip"
	KindUsername  Kind = "username"
	KindToken     Kind = "token"
	KindPassword  Kind = "password"
	KindEmptyJSON Kind = "empty_json_array"
	// KindNull clears the value: NULL for nullable columns, '' or '{}' otherwise.
	KindNull Kind = "null"
)

// KnownPasswordHash is the bcrypt hash of "shopware". Admin users, customers and
// integration secrets all get this value, so every account works locally with
// the dockware default password and no real hash leaves the server.
const KnownPasswordHash = "$2y$10$OLyjdGg5/EDY4Xb6mH2.eeODCPzWWOLXXKOtlXdPnfpwPkNun3HCW"

// Rule anonymises one column. Rules for columns that do not exist in the
// remote schema are skipped, so one rule set serves several Shopware versions.
type Rule struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	Kind   Kind   `json:"kind"`
}

// UnmarshalYAML reads a rule from the config file. An unquoted `kind: null`
// is a YAML null, which yaml.v3 never hands to the field's own unmarshaler
// (it zeroes the field instead), so the mapping is walked here and a null
// kind becomes KindNull. Unknown keys stay an error, as with KnownFields.
func (r *Rule) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: a rule is a mapping with table, column and kind", n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i], n.Content[i+1]
		var dst *string
		switch key.Value {
		case "table":
			dst = &r.Table
		case "column":
			dst = &r.Column
		case "kind":
			if val.ShortTag() == "!!null" {
				r.Kind = KindNull
				continue
			}
			dst = (*string)(&r.Kind)
		default:
			return fmt.Errorf("line %d: field %s not found in rule", key.Line, key.Value)
		}
		if err := val.Decode(dst); err != nil {
			return err
		}
	}
	return nil
}

// DefaultRules covers the shopware-cli defaults plus the gaps found in the
// shopware-cli 0.19.7 review (comments, birthdays, VAT ids, secondary address
// lines, payment custom fields, admin users, integration secrets, tokens).
func DefaultRules() []Rule {
	var rules []Rule
	add := func(table string, kind Kind, columns ...string) {
		for _, c := range columns {
			rules = append(rules, Rule{Table: table, Column: c, Kind: kind})
		}
	}

	for _, t := range []string{"customer", "order_customer"} {
		add(t, KindEmail, "email")
		add(t, KindFirstName, "first_name")
		add(t, KindLastName, "last_name")
		add(t, KindCompany, "company")
		add(t, KindIP, "remote_address")
		add(t, KindNull, "title", "vat_ids", "custom_fields")
	}
	add("customer", KindPassword, "password")
	add("customer", KindNull, "birthday", "legacy_password", "legacy_encoder", "hash", "affiliate_code", "campaign_code")

	for _, t := range []string{"customer_address", "order_address"} {
		add(t, KindFirstName, "first_name")
		add(t, KindLastName, "last_name")
		add(t, KindCompany, "company")
		add(t, KindStreet, "street")
		add(t, KindZip, "zipcode")
		add(t, KindCity, "city")
		add(t, KindPhone, "phone_number")
		add(t, KindNull, "title", "department", "additional_address_line1", "additional_address_line2", "vat_id", "hash", "custom_fields")
	}

	add("order", KindNull, "customer_comment", "internal_comment", "custom_fields")
	add("order", KindToken, "deep_link_code")
	add("order_delivery", KindEmptyJSON, "tracking_codes")
	add("order_transaction", KindNull, "custom_fields")
	add("order_transaction_capture", KindNull, "external_reference", "custom_fields")

	add("newsletter_recipient", KindEmail, "email")
	add("newsletter_recipient", KindFirstName, "first_name")
	add("newsletter_recipient", KindLastName, "last_name")
	add("newsletter_recipient", KindStreet, "street")
	add("newsletter_recipient", KindZip, "zip_code")
	add("newsletter_recipient", KindCity, "city")
	add("newsletter_recipient", KindToken, "hash")
	add("newsletter_recipient", KindNull, "title", "custom_fields")

	add("product_review", KindEmail, "email", "external_email")
	add("product_review", KindFirstName, "external_user")
	add("promotion_individual_code", KindNull, "payload")

	add("user", KindUsername, "username")
	add("user", KindEmail, "email")
	add("user", KindFirstName, "first_name")
	add("user", KindLastName, "last_name")
	add("user", KindPassword, "password")
	add("user", KindNull, "store_token", "custom_fields")
	add("user_access_key", KindPassword, "secret_access_key")
	add("integration", KindPassword, "secret_access_key")
	add("app", KindToken, "app_secret")

	return rules
}

// NoDataTables keeps only the schema of tables whose rows are transient,
// derived or personal and not needed to run a shop locally. Glob patterns allowed.
func NoDataTables() []string {
	return []string{
		// shopware-cli --clean set
		"cart", "customer_recovery", "dead_message", "enqueue", "messenger_messages",
		"import_export_log", "increment", "elasticsearch_index_task", "admin_elasticsearch_index_task",
		"log_entry", "message_queue_stats", "notification", "payment_token", "refresh_token",
		"version", "version_commit", "version_commit_data", "webhook_event_log",
		// gaps: sessions, recovery tokens, generated documents, import files
		"sales_channel_api_context", "user_recovery", "document", "import_export_file",
		"app_flow_event", "flow_execution", "scheduled_task_execution",
		// logs of any extension
		"*_log", "*_logs", "*_log_entry",
	}
}

// SecretConfigKeys drops system_config rows that hold credentials. The local
// shop falls back to the defaults; projects re-add sandbox keys via post_sql.
const SecretConfigKeys = `(password|secret|token|apikey|api_key|accesskey|access_key|privatekey|private_key|clientid|client_id|merchantid|webhook|license|shopid)`

// SystemConfigWhere is the WHERE clause applied to system_config.
func SystemConfigWhere() string {
	return "configuration_key NOT REGEXP '(?i)" + SecretConfigKeys + "' AND configuration_key NOT IN ('core.store.shopSecret','core.app.shopId')"
}

// Column is the schema metadata needed to render a rule.
type Column struct {
	Table     string
	Name      string
	Nullable  bool
	DataType  string
	Generated bool
}

// Schema maps table → column name → metadata.
type Schema map[string]map[string]Column

// HasID reports whether the table has a binary id column usable as seed.
func (s Schema) HasID(table string) bool {
	c, ok := s[table]["id"]
	return ok && (c.DataType == "binary" || c.DataType == "varbinary")
}

// Applicable returns the rules whose table and column exist, sorted.
func Applicable(rules []Rule, schema Schema) []Rule {
	var out []Rule
	for _, r := range rules {
		cols, ok := schema[r.Table]
		if !ok {
			continue
		}
		c, ok := cols[r.Column]
		if !ok || c.Generated {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Table != out[j].Table {
			return out[i].Table < out[j].Table
		}
		return out[i].Column < out[j].Column
	})
	return out
}

var (
	firstNames = []string{"Anna", "Ben", "Clara", "David", "Emma", "Felix", "Greta", "Hannes", "Ida", "Jonas", "Lena", "Max", "Nora", "Paul", "Sophie", "Tom"}
	lastNames  = []string{"Muster", "Beispiel", "Schmidt", "Weber", "Fischer", "Wagner", "Becker", "Hoffmann", "Koch", "Richter", "Klein", "Wolf"}
	cities     = []string{"Berlin", "Hamburg", "München", "Köln", "Leipzig", "Bremen", "Dresden", "Freiburg"}
)

func elt(seed string, values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = "'" + v + "'"
	}
	return fmt.Sprintf("ELT(1 + %s %% %d, %s)", seed, len(values), strings.Join(quoted, ","))
}

// Expression renders the SQL rewrite for a rule. Columns that are NULL or empty
// stay as they are, so optional fields keep their shape and no value is invented.
func Expression(r Rule, schema Schema) string {
	col := schema[r.Table][r.Column]
	q := "`" + r.Column + "`"

	hexID := "LOWER(HEX(`id`))"
	crc := "CRC32(`id`)"
	if !schema.HasID(r.Table) {
		hexID = "LOWER(REPLACE(UUID(),'-',''))"
		crc = "CRC32(UUID())"
	}

	var expr string
	switch r.Kind {
	case KindEmail:
		expr = "CONCAT('" + strings.ReplaceAll(r.Table, "_", "-") + "-', " + hexID + ", '@example.com')"
	case KindFirstName:
		expr = elt(crc, firstNames)
	case KindLastName:
		expr = elt("("+crc+" DIV 16)", lastNames)
	case KindCompany:
		// UUIDv7 ids share their time prefix: take the random tail
		expr = "CONCAT('Firma ', UPPER(RIGHT(" + hexID + ", 6)))"
	case KindStreet:
		expr = "CONCAT('Musterstraße ', 1 + " + crc + " % 200)"
	case KindZip:
		expr = "LPAD(" + crc + " % 100000, 5, '0')"
	case KindCity:
		expr = elt("("+crc+" DIV 256)", cities)
	case KindPhone:
		expr = "CONCAT('+49 30 ', LPAD(" + crc + " % 10000000, 7, '0'))"
	case KindIP:
		expr = "'127.0.0.1'"
	case KindUsername:
		// full id: user.username is unique and UUIDv7 prefixes collide
		expr = "CONCAT('user-', " + hexID + ")"
	case KindToken:
		expr = "LOWER(REPLACE(UUID(),'-',''))"
	case KindPassword:
		expr = "'" + KnownPasswordHash + "'"
	case KindEmptyJSON:
		expr = "'[]'"
	case KindNull:
		return nullExpression(col)
	default:
		panic("unknown anonymisation kind " + string(r.Kind))
	}
	return "IF(" + q + " IS NULL OR " + q + " = '', " + q + ", " + expr + ")"
}

func nullExpression(c Column) string {
	if c.Nullable {
		return "NULL"
	}
	if c.DataType == "json" {
		return "'{}'"
	}
	return "''"
}

// RewriteMap returns table → column → SQL for the applicable rules.
func RewriteMap(rules []Rule, schema Schema) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, r := range Applicable(rules, schema) {
		if out[r.Table] == nil {
			out[r.Table] = map[string]string{}
		}
		out[r.Table][r.Column] = Expression(r, schema)
	}
	return out
}

// RulesHash fingerprints the effective rule set for the audit log.
func RulesHash(rewrites map[string]map[string]string, noData []string, where map[string]string) string {
	h := sha256.New()
	tables := make([]string, 0, len(rewrites))
	for t := range rewrites {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	for _, t := range tables {
		cols := make([]string, 0, len(rewrites[t]))
		for c := range rewrites[t] {
			cols = append(cols, c)
		}
		sort.Strings(cols)
		for _, c := range cols {
			fmt.Fprintf(h, "r %s.%s=%s\n", t, c, rewrites[t][c])
		}
	}
	nd := append([]string(nil), noData...)
	sort.Strings(nd)
	for _, t := range nd {
		fmt.Fprintf(h, "n %s\n", t)
	}
	wk := make([]string, 0, len(where))
	for t := range where {
		wk = append(wk, t)
	}
	sort.Strings(wk)
	for _, t := range wk {
		fmt.Fprintf(h, "w %s=%s\n", t, where[t])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
