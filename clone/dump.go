package clone

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DumpPlan is the effective export configuration shared by both strategies.
type DumpPlan struct {
	Rewrites   map[string]map[string]string
	SchemaOnly []string
	Where      map[string]string
	Limits     []string
}

// NewDumpPlan builds the plan from probe results and table classification.
func NewDumpPlan(p *Probe, c Classification, rules []Rule, limits []string) DumpPlan {
	where := map[string]string{}
	if _, ok := p.Schema["system_config"]; ok {
		where["system_config"] = SystemConfigWhere()
	}
	return DumpPlan{
		Rewrites:   RewriteMap(rules, p.Schema),
		SchemaOnly: c.SchemaOnly(),
		Where:      where,
		Limits:     limits,
	}
}

// AddWhere restricts table to rows matching cond (SQL without the WHERE
// keyword); an existing condition for the table is kept and ANDed.
func (d *DumpPlan) AddWhere(table, cond string) {
	if d.Where == nil {
		d.Where = map[string]string{}
	}
	if old := d.Where[table]; old != "" {
		cond = "(" + old + ") AND (" + cond + ")"
	}
	d.Where[table] = cond
}

// DumpResult describes the produced anonymised dump.
type DumpResult struct {
	File        string                   `json:"file"`
	Bytes       int64                    `json:"bytes"`
	Compression string                   `json:"compression"`
	Duration    time.Duration            `json:"duration_ns"`
	Phases      map[string]time.Duration `json:"phases_ns"`
}

func archName(remote string) (string, error) {
	switch remote {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	}
	return "", &Error{Step: StepDump, Msg: "no shopware-cli build for the server architecture " + remote,
		Hint: "use --strategy shadow, which only needs mysqldump on the server"}
}

// shopwareCLIBinary returns a cached static shopware-cli for linux/<arch>,
// extracted from the official image so no download from the server is needed.
func shopwareCLIBinary(ctx context.Context, version, arch string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", Fail(StepDump, "no user cache directory for the shopware-cli binary", err, "")
	}
	dst := filepath.Join(cache, "dockware-cli", fmt.Sprintf("shopware-cli-%s-linux-%s", version, arch))
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", Fail(StepDump, "cannot create "+filepath.Dir(dst), err, "")
	}
	image := "ghcr.io/shopware/shopware-cli:" + version
	id, err := exec.CommandContext(ctx, "docker", "create", "--platform", "linux/"+arch, image).Output()
	if err != nil {
		var ee *exec.ExitError
		out := ""
		if errors.As(err, &ee) {
			out = string(ee.Stderr)
		}
		return "", Fail(StepDocker, "cannot create a container from "+image+" to extract shopware-cli", err, out)
	}
	cid := strings.TrimSpace(string(id))
	defer exec.Command("docker", "rm", "-f", cid).Run()
	tmp := dst + ".tmp"
	if out, err := exec.CommandContext(ctx, "docker", "cp", cid+":/usr/local/bin/shopware-cli", tmp).CombinedOutput(); err != nil {
		return "", Fail(StepDocker, "cannot copy shopware-cli out of "+image, err, string(out))
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", Fail(StepDump, "cannot store the shopware-cli binary", err, "")
	}
	return dst, nil
}

type swConfig struct {
	CompatibilityDate string `yaml:"compatibility_date"`
	Dump              struct {
		Rewrite map[string]map[string]string `yaml:"rewrite,omitempty"`
		NoData  []string                     `yaml:"nodata,omitempty"`
		Where   map[string]string            `yaml:"where,omitempty"`
	} `yaml:"dump"`
}

// ShopwareCLIConfig renders the project config consumed by shopware-cli.
func (d DumpPlan) ShopwareCLIConfig(now time.Time) ([]byte, error) {
	var c swConfig
	c.CompatibilityDate = now.Format("2006-01-02")
	c.Dump.Rewrite = d.Rewrites
	c.Dump.NoData = d.SchemaOnly
	c.Dump.Where = d.Where
	return yaml.Marshal(c)
}

// StreamDump runs shopware-cli on the server. Rows are rewritten inside the
// SELECT, so only anonymised data is ever serialised; the zstd stream goes
// straight to the local file and nothing is written on the server.
func StreamDump(ctx context.Context, r Remote, p *Probe, plan DumpPlan, version, out string, log io.Writer) (*DumpResult, error) {
	res := &DumpResult{File: out, Compression: "zstd", Phases: map[string]time.Duration{}}
	start := time.Now()
	arch, err := archName(p.Arch)
	if err != nil {
		return nil, err
	}
	bin, err := shopwareCLIBinary(ctx, version, arch)
	if err != nil {
		return nil, err
	}
	dir, err := r.upload(ctx, bin, "swcli", p.hasGzip)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := r.cleanup(dir); err != nil {
			fmt.Fprintf(log, "WARN remote cleanup of %s failed: %v\n", dir, err)
		}
	}()
	cfg, err := plan.ShopwareCLIConfig(time.Now())
	if err != nil {
		return nil, Fail(StepDump, "cannot render the shopware-cli config", err, "")
	}
	if err := r.putFile(ctx, dir, "clone.yml", cfg); err != nil {
		return nil, err
	}
	res.Phases["upload"] = time.Since(start)

	args := []string{dir + "/swcli", "--project-config", dir + "/clone.yml", "project", "dump",
		"--anonymize", "--clean", "--skip-lock-tables", "--compression", "zstd", "--output", "-",
		"-n", "--no-update-hint", "--no-ai-hint"}
	for _, l := range plan.Limits {
		args = append(args, "--limit", l)
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = ShellQuote(a)
	}
	script := "cd " + ShellQuote(r.Root) + " && exec nice -n 10 " + strings.Join(quoted, " ")

	f, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, Fail(StepDump, "cannot write "+out, err, "")
	}
	defer f.Close()
	var errb bytes.Buffer
	dumpStart := time.Now()
	if err := r.Run(ctx, script, nil, f, io.MultiWriter(&errb, log)); err != nil {
		return nil, Fail(StepDump, "shopware-cli dump on the server failed", err, strings.TrimSpace(errb.String()))
	}
	res.Phases["dump"] = time.Since(dumpStart)
	if st, err := f.Stat(); err == nil {
		res.Bytes = st.Size()
	}
	res.Duration = time.Since(start)
	return res, nil
}

// ShadowSQL builds the server-side copy: CREATE TABLE … LIKE plus a single
// INSERT … SELECT per exported table with the rewrites applied in the SELECT.
// READ COMMITTED gives non-locking consistent reads on InnoDB with row binlog.
// The session's default schema is the live one, so unqualified table names in
// plan.Where (subqueries) resolve to live tables, as they do for shopware-cli.
func ShadowSQL(p *Probe, c Classification, plan DumpPlan, shadow string, createDB bool) string {
	var b strings.Builder
	live := "`" + p.Database + "`"
	sh := "`" + shadow + "`"
	b.WriteString("SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED;\nSET SESSION foreign_key_checks = 0, unique_checks = 0;\n")
	if createDB {
		fmt.Fprintf(&b, "CREATE DATABASE %s CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;\n", sh)
	}
	schemaOnly := map[string]bool{}
	for _, t := range plan.SchemaOnly {
		schemaOnly[t] = true
	}
	tables := append([]string(nil), p.Tables...)
	sort.Strings(tables)
	for _, t := range tables {
		qt := "`" + t + "`"
		fmt.Fprintf(&b, "CREATE TABLE %s.%s LIKE %s.%s;\n", sh, qt, live, qt)
		if schemaOnly[t] {
			continue
		}
		cols := make([]string, 0, len(p.Schema[t]))
		for name, col := range p.Schema[t] {
			if !col.Generated {
				cols = append(cols, name)
			}
		}
		sort.Strings(cols)
		ins := make([]string, len(cols))
		sel := make([]string, len(cols))
		for i, col := range cols {
			ins[i] = "`" + col + "`"
			if expr, ok := plan.Rewrites[t][col]; ok {
				sel[i] = expr + " AS `" + col + "`"
			} else {
				sel[i] = "`" + col + "`"
			}
		}
		where := ""
		if w, ok := plan.Where[t]; ok {
			where = " WHERE " + w
		}
		fmt.Fprintf(&b, "INSERT INTO %s.%s (%s) SELECT %s FROM %s.%s%s;\n", sh, qt, strings.Join(ins, ","), strings.Join(sel, ","), live, qt, where)
	}
	return b.String()
}

// shadowCreateAllowed reports whether the connected database user may create
// the shadow schema: a global CREATE privilege (probe) or a schema-level grant
// whose pattern covers the name (e.g. `shop\_%`). Returns the user for messages.
func shadowCreateAllowed(ctx context.Context, r Remote, p *Probe, shadow string) (user string, ok bool, err error) {
	out, err := r.MySQL(ctx, "SELECT CURRENT_USER(), COUNT(*) FROM information_schema.schema_privileges WHERE privilege_type = 'CREATE' AND grantee = "+granteeSQL+" AND "+sqlString(shadow)+" LIKE table_schema")
	if err != nil {
		return "", false, err
	}
	user, n, _ := strings.Cut(strings.TrimSpace(out), "\t")
	return user, p.CanCreateDB || n != "0", nil
}

// ShadowDump mirrors the live schema into a shadow schema on the same server,
// anonymising while copying, then streams mysqldump of the shadow and drops it.
func ShadowDump(ctx context.Context, r Remote, p *Probe, c Classification, plan DumpPlan, shadow, out string, log io.Writer) (*DumpResult, error) {
	if len(plan.Limits) > 0 {
		return nil, &Error{Step: StepShadow, Msg: "--limit is only supported by the stream strategy", Hint: "drop --limit or use --strategy stream"}
	}
	if p.BinlogFormat == "STATEMENT" {
		return nil, &Error{Step: StepShadow, Msg: "the server runs binlog_format=STATEMENT: INSERT … SELECT would lock the live rows",
			Hint: "use --strategy stream (default), which only reads"}
	}
	if shadow == "" {
		shadow = p.Database + "_dwc_shadow"
	}
	if shadow == p.Database {
		return nil, &Error{Step: StepShadow, Msg: "the shadow schema must differ from the live schema " + p.Database, Hint: "pass another --shadow-db name"}
	}
	res := &DumpResult{File: out, Compression: "gzip", Phases: map[string]time.Duration{}}
	start := time.Now()

	exists, err := r.MySQL(ctx, "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = "+sqlString(shadow))
	if err != nil {
		return nil, err
	}
	createDB := strings.TrimSpace(exists) == "0"
	if createDB {
		user, ok, err := shadowCreateAllowed(ctx, r, p, shadow)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, &Error{Step: StepShadow, Msg: "the database user " + user + " may not create the shadow schema " + shadow,
				Hint: "use --strategy stream (default, needs no server writes) or ask the hoster for CREATE on that schema"}
		}
	} else {
		n, err := r.MySQL(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = "+sqlString(shadow))
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(n) != "0" {
			return nil, &Error{Step: StepShadow, Msg: "the shadow schema " + shadow + " exists and is not empty; refusing to touch it",
				Hint: "drop its tables on the server (a previous run may have been interrupted) or pass another --shadow-db name"}
		}
	}

	drop := "DROP DATABASE IF EXISTS `" + shadow + "`"
	if !createDB {
		var parts []string
		for _, t := range p.Tables {
			parts = append(parts, "`"+shadow+"`.`"+t+"`")
		}
		drop = "SET foreign_key_checks = 0; DROP TABLE IF EXISTS " + strings.Join(parts, ",")
	}
	// registered before the copy, so the schema is removed on every later failure
	defer func() {
		if _, err := r.MySQL(context.Background(), drop); err != nil {
			fmt.Fprintf(log, "WARN dropping shadow %s failed: %v\n", shadow, err)
		}
	}()

	copyStart := time.Now()
	sql := ShadowSQL(p, c, plan, shadow, createDB)
	_, errb, err := r.run(ctx, r.dbEnvScript()+`exec nice -n 10 $M "$DBN"`, strings.NewReader(sql))
	if err != nil {
		return nil, r.dbError(StepShadow, "loading the anonymised copy into shadow schema "+shadow+" failed", err, errb)
	}
	res.Phases["copy"] = time.Since(copyStart)

	f, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, Fail(StepDump, "cannot write "+out, err, "")
	}
	defer f.Close()
	dumpStart := time.Now()
	// CREATE TABLE … LIKE drops foreign keys, so the schema comes from the live
	// database; triggers are added after the data so they do not fire on import.
	gtid := " --set-gtid-purged=OFF"
	if strings.Contains(strings.ToLower(p.MySQLVersion), "mariadb") {
		gtid = "" // MariaDB's mysqldump rejects the flag
	}
	dumpScript := r.dbEnvScript() + `D="mysqldump -h$DBH -P$DBP -u$DBU --single-transaction --skip-lock-tables --no-tablespaces --hex-blob` + gtid + ` --default-character-set=utf8mb4"
st=$(mktemp /tmp/dwc.XXXXXX) || ` + shExit(exitNoTemp) + `
{ nice -n 10 $D --no-data --skip-triggers "$DBN" && nice -n 10 $D --no-create-info --skip-triggers ` + ShellQuote(shadow) + ` && nice -n 10 $D --no-data --no-create-info --triggers "$DBN"; echo $? > "$st"; } | nice -n 10 gzip -1
rc=$(cat "$st"); rm -f "$st"; exit "$rc"`
	var dumpErr bytes.Buffer
	if err := r.Run(ctx, dumpScript, nil, f, &dumpErr); err != nil {
		if remoteExit(err) == exitNoTemp {
			return nil, &Error{Step: StepDump, Msg: "cannot create a temp file in /tmp on the server", Hint: "/tmp must be writable for the ssh user; check disk space and quota there", Err: err}
		}
		return nil, r.dbError(StepDump, "mysqldump of the shadow schema "+shadow+" failed", err, strings.TrimSpace(dumpErr.String()))
	}
	res.Phases["dump"] = time.Since(dumpStart)
	if st, err := f.Stat(); err == nil {
		res.Bytes = st.Size()
	}
	res.Duration = time.Since(start)
	return res, nil
}
