package clone

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Check is one verification result written to the audit log.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// leakKinds produce values that cannot plausibly equal a real value, so an
// equal remote value proves the rewrite did not happen.
var leakKinds = map[Kind]bool{KindEmail: true, KindCompany: true, KindPhone: true, KindZip: true, KindUsername: true, KindToken: true}

// localPattern is a SQL predicate on q (a column expression) that is true
// for every value the rule can generate.
func localPattern(r Rule, q string) string {
	in := func(values []string) string {
		quoted := make([]string, len(values))
		for i, v := range values {
			quoted[i] = sqlString(v)
		}
		return q + " IN (" + strings.Join(quoted, ",") + ")"
	}
	switch r.Kind {
	case KindEmail:
		return q + " REGEXP '^[a-z0-9-]+-[0-9a-f]{32}@example[.]com$'"
	case KindFirstName:
		return in(firstNames)
	case KindLastName:
		return in(lastNames)
	case KindCity:
		return in(cities)
	case KindCompany:
		return q + " REGEXP '^Firma [0-9A-F]{6}$'"
	case KindStreet:
		return q + " LIKE 'Musterstra%e %'"
	case KindZip:
		return q + " REGEXP '^[0-9]{5}$'"
	case KindPhone:
		return q + " REGEXP '^[+]49 30 [0-9]{7}$'"
	case KindIP:
		return q + " = '127.0.0.1'"
	case KindUsername:
		return q + " REGEXP '^user-[0-9a-f]{32}$'"
	case KindToken:
		return q + " REGEXP '^[0-9a-f]{32}$'"
	case KindPassword:
		return q + " = " + sqlString(KnownPasswordHash)
	case KindEmptyJSON:
		return q + " = '[]'"
	case KindNull:
		return q + " IN ('', '{}')"
	}
	return "0"
}

// Verify proves the imported data is anonymised. Remote queries only receive
// generated values and return counts, so no personal data is transferred.
func Verify(ctx context.Context, r Remote, l *Local, p *Probe, c Classification, rules []Rule, sample int) ([]Check, error) {
	var checks []Check
	exported := map[string]bool{}
	for _, t := range append(append([]string(nil), c.Core...), c.Allowed...) {
		exported[t] = true
	}

	// 1. table count: guards against shopware-cli silently dumping another database
	out, err := l.SQL(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE'")
	if err != nil {
		return nil, Fail(StepVerify, "counting the local tables failed", err, "")
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	checks = append(checks, Check{Name: "table count matches source", OK: n == len(p.Tables), Detail: fmt.Sprintf("local %d, remote %d", n, len(p.Tables))})

	// 2. every rewritten value has the generated shape
	applicable := Applicable(rules, p.Schema)
	var shape []string
	for _, rule := range applicable {
		if !exported[rule.Table] {
			continue
		}
		col := p.Schema[rule.Table][rule.Column]
		q := "`" + rule.Column + "`"
		present := q + " IS NOT NULL"
		textExpr := q
		if col.DataType == "json" {
			// JSON = 'string' compares as JSON values, never as text
			textExpr = "CAST(" + q + " AS CHAR)"
		}
		pattern := localPattern(rule, textExpr)
		if isText(col.DataType) {
			present += " AND " + q + " <> ''"
		} else if rule.Kind == KindNull {
			pattern = "0" // dates, numbers: only NULL is acceptable
		}
		shape = append(shape, fmt.Sprintf("SELECT '%s.%s', COUNT(*) FROM `%s` WHERE %s AND NOT (%s)",
			rule.Table, rule.Column, rule.Table, present, pattern))
	}
	bad, err := countRows(ctx, l, shape)
	if err != nil {
		return nil, Fail(StepVerify, "checking the rewritten columns failed", err, "")
	}
	checks = append(checks, Check{Name: fmt.Sprintf("%d rewritten columns have generated values", len(shape)), OK: len(bad) == 0, Detail: formatCounts(bad)})

	// 3. sampled generated values must not exist on the source for the same row
	var remote []string
	for _, rule := range applicable {
		if !exported[rule.Table] || !leakKinds[rule.Kind] || !p.Schema.HasID(rule.Table) {
			continue
		}
		q := "`" + rule.Column + "`"
		rows, err := l.SQL(ctx, fmt.Sprintf("SELECT LOWER(HEX(id)), %s FROM `%s` WHERE %s IS NOT NULL AND %s <> '' ORDER BY id LIMIT %d", q, rule.Table, q, q, sample))
		if err != nil {
			return nil, Fail(StepVerify, "sampling "+rule.Table+"."+rule.Column+" failed", err, "")
		}
		var pairs []string
		for _, line := range strings.Split(strings.TrimSpace(rows), "\n") {
			id, val, ok := strings.Cut(line, "\t")
			if !ok {
				continue
			}
			pairs = append(pairs, fmt.Sprintf("(UNHEX('%s'),%s)", id, sqlString(val)))
		}
		if len(pairs) > 0 {
			remote = append(remote, fmt.Sprintf("SELECT '%s.%s', COUNT(*) FROM `%s` WHERE (id, %s) IN (%s)", rule.Table, rule.Column, rule.Table, q, strings.Join(pairs, ",")))
		}
	}
	leaks := map[string]int{}
	if len(remote) > 0 {
		out, err := r.MySQL(ctx, strings.Join(remote, ";\n")+";")
		if err != nil {
			return nil, Fail(StepVerify, "comparing the sample with the source failed", err, "")
		}
		leaks = parseCounts(out)
	}
	checks = append(checks, Check{Name: fmt.Sprintf("%d sampled columns differ from source (≤%d rows each)", len(remote), sample), OK: len(leaks) == 0, Detail: formatCounts(leaks)})

	// 4. schema-only tables are empty
	var empty []string
	for _, t := range c.SchemaOnly() {
		empty = append(empty, fmt.Sprintf("SELECT '%s', COUNT(*) FROM `%s`", t, t))
	}
	filled, err := countRows(ctx, l, empty)
	if err != nil {
		return nil, Fail(StepVerify, "counting the rows of the schema-only tables failed", err, "")
	}
	checks = append(checks, Check{Name: fmt.Sprintf("%d schema-only tables are empty", len(empty)), OK: len(filled) == 0, Detail: formatCounts(filled)})

	// 5. no credentials in system_config
	if _, ok := p.Schema["system_config"]; ok {
		out, err := l.SQL(ctx, "SELECT COUNT(*) FROM system_config WHERE NOT ("+SystemConfigWhere()+")")
		if err != nil {
			return nil, Fail(StepVerify, "counting secret system_config keys failed", err, "")
		}
		cnt := strings.TrimSpace(out)
		checks = append(checks, Check{Name: "no secret system_config keys", OK: cnt == "0", Detail: cnt + " secret keys"})
	}

	// 6. e-mail addresses outside example.com in any exported text column
	// (informational: shop contact addresses are expected here)
	var scan []string
	for _, t := range sortedTables(exported) {
		cols := p.Schema[t]
		var names []string
		for name, col := range cols {
			if isText(col.DataType) && !col.Generated {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			scan = append(scan, fmt.Sprintf("SELECT '%s.%s', COUNT(*) FROM `%s` WHERE `%s` LIKE '%%@%%' AND `%s` REGEXP '[A-Za-z0-9._%%+-]+@(?!example[.]com)[A-Za-z0-9-]+[.][A-Za-z.]{2,}'", t, name, t, name, name))
		}
	}
	found, err := countRows(ctx, l, scan)
	if err != nil {
		return nil, Fail(StepVerify, "the e-mail scan failed", err, "")
	}
	checks = append(checks, Check{Name: "e-mail scan of exported text columns (review, not a failure)", OK: true, Detail: formatCounts(found)})
	return checks, nil
}

func sortedTables(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for t := range m {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// countBatch is the number of count queries per mysql call; countWorkers
// bounds the calls in flight, as the regex scans are CPU-bound in the
// container and the e-mail scan alone covers every text column.
const (
	countBatch   = 200
	countWorkers = 4
)

// countRows runs count queries in batches, countWorkers at a time, and
// returns the non-zero ones. Every query aliases its count to a unique name,
// so the merged result does not depend on the batch order.
func countRows(ctx context.Context, l *Local, queries []string) (map[string]int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		res      = map[string]int{}
		firstErr error
		jobs     = make(chan []string)
	)
	for i := 0; i < countWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for batch := range jobs {
				// long CMS/HTML values exceed MySQL's default regexp budget (32 steps);
				// the variable is global-only, which is fine on the throwaway local DB
				out, err := l.SQL(ctx, "SET GLOBAL regexp_time_limit = 100000;\n"+strings.Join(batch, ";\n")+";")
				mu.Lock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
						cancel() // the other batches stop instead of finishing their scans
					}
				} else {
					for k, v := range parseCounts(out) {
						res[k] = v
					}
				}
				mu.Unlock()
			}
		}()
	}
	for _, batch := range batches(queries, countBatch) {
		jobs <- batch
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return res, nil
}

// batches splits items into consecutive slices of at most size items.
func batches(items []string, size int) [][]string {
	var out [][]string
	for len(items) > size {
		out = append(out, items[:size])
		items = items[size:]
	}
	if len(items) > 0 {
		out = append(out, items)
	}
	return out
}

func parseCounts(out string) map[string]int {
	res := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		k, v, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		if n, _ := strconv.Atoi(v); n > 0 {
			res[k] = n
		}
	}
	return res
}

func formatCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%d", k, m[k])
	}
	return strings.Join(parts, ", ")
}
func isText(dataType string) bool {
	switch dataType {
	case "char", "varchar", "tinytext", "text", "mediumtext", "longtext", "json":
		return true
	}
	return false
}
