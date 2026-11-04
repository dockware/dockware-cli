// Package clone copies a running Shopware shop into a local dockware
// environment with anonymised data, without loading the source server.
package clone

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Options configure one clone run.
type Options struct {
	Target          string
	Root            string
	SSHPort         int
	Identity        string
	SSHOptions      []string
	Strategy        string // stream | shadow
	ShadowDB        string
	Limits          []string
	CodeDir         string   // existing local checkout; empty = rsync from the server
	Exclude         []string // extra rsync exclude patterns for the code sync
	MaxCodeSizeMB   *int     // code sync limit in MB, -1 = no limit; nil = config max_code_size_mb, else defaultMaxCodeSizeMB
	OrdersSinceDays *int     // keep only orders of the last N days, 0 = all; nil = config orders_since_days
	SkipSearchIndex *bool    // transfer the product search index schema-only and rebuild it locally; nil = config skip_search_index
	PHP             string   // PHP major.minor of the local shop; empty = config or server
	Node            string   // node major of the local shop; empty = config, checkout, server
	ComposerAuth    string   // auth.json merged on top of FindComposerAuth's other sources
	ConfigPath      string   // default <CodeDir>/.dockware-clone.yml
	Name            string
	OutDir          string
	Port            int
	FrontendPort    int
	SWCLIVersion    string
	AppEnv          string
	Image           string
	VerifySample    int
	DumpOnly        bool
	NoFrontend      bool
	Log             io.Writer
}

// defaultMaxCodeSizeMB caps the code sync, so a project root full of backups
// and uploads is noticed before it lands on the laptop.
const defaultMaxCodeSizeMB = 300

// searchIndexTables hold the product search index; Shopware rebuilds it from
// the product data (dal:refresh:index --only=product.indexer).
var searchIndexTables = []string{"product_search_keyword", "product_keyword_dictionary"}

// syncWriter serialises the shared log: the dump streams shopware-cli
// progress from its own goroutine while the local steps write tool output.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// Run executes probe → dump, overlapped with the local environment → import
// → verification → smoke tests. Every error it returns is an *Error naming
// the step that failed.
func Run(ctx context.Context, o Options) (err error) {
	log := &syncWriter{w: o.Log}
	step := StepConfig
	classify := func(err error) error {
		if err == nil {
			return nil
		}
		return Fail(step, "clone failed during "+step, err, "")
	}
	defer func() { err = classify(err) }()

	r, err := ParseTarget(o.Target)
	if err != nil {
		return Fail(StepConfig, "invalid ssh target "+o.Target, err, "")
	}
	if o.Strategy != "stream" && o.Strategy != "shadow" {
		return &Error{Step: StepConfig, Msg: fmt.Sprintf("unknown strategy %q", o.Strategy), Hint: "pass --strategy stream (default) or --strategy shadow"}
	}
	if o.PHP != "" && !configPHPVersion.MatchString(o.PHP) {
		return &Error{Step: StepConfig, Msg: fmt.Sprintf("invalid PHP version %q", o.PHP), Hint: "pass --php as major.minor, e.g. 8.3"}
	}
	if o.Node != "" && !configNodeVersion.MatchString(o.Node) {
		return &Error{Step: StepConfig, Msg: fmt.Sprintf("invalid node version %q", o.Node), Hint: "pass --node as major version, e.g. 22"}
	}
	if o.SSHPort != 0 {
		r.Port = o.SSHPort
	}
	r.Identity, r.Root = o.Identity, o.Root
	for _, opt := range o.SSHOptions {
		r.Extra = append(r.Extra, "-o", opt)
	}
	if o.Name == "" {
		o.Name = "dwc-" + slug(r.Target)
	}
	if o.OutDir == "" {
		o.OutDir = o.Name
	}
	if o.OutDir, err = filepath.Abs(o.OutDir); err != nil {
		return Fail(StepConfig, "cannot resolve the output directory", err, "")
	}
	if o.CodeDir != "" {
		if o.CodeDir, err = filepath.Abs(o.CodeDir); err != nil {
			return Fail(StepConfig, "cannot resolve the code directory", err, "")
		}
		if insideDir(o.OutDir, o.CodeDir) {
			return &Error{Step: StepConfig, Msg: fmt.Sprintf("output directory %s is inside the checkout %s", o.OutDir, o.CodeDir), Hint: "pass --out <dir> outside the checkout"}
		}
	}
	if err := os.MkdirAll(o.OutDir, 0o700); err != nil {
		return Fail(StepConfig, "cannot create output directory "+o.OutDir, err, "")
	}
	audit := newAudit(o.Target, o.Root, o.Strategy)
	defer func() {
		err = classify(err)
		if err != nil {
			audit.Error = err.Error()
			var ce *Error
			if errors.As(err, &ce) {
				audit.ErrorStep, audit.ErrorHint = ce.Step, ce.Hint
			}
		}
		if werr := audit.write(o.OutDir); werr != nil && err == nil {
			err = Fail(step, "cannot write the audit log", werr, "")
		}
		fmt.Fprintf(log, "audit: %s\n", filepath.Join(o.OutDir, "clone.log.json"))
	}()
	record := func(name string, took time.Duration) {
		audit.Phases[name] = took
		fmt.Fprintf(log, "  %s: %s\n", name, took.Round(100*time.Millisecond))
	}
	phase := func(name string, start time.Time) { record(name, time.Since(start)) }
	warn := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		audit.Warnings = append(audit.Warnings, msg)
		fmt.Fprintln(log, "WARN "+msg)
	}

	// project config and effective settings: an explicitly passed flag wins
	// over the config, also when it switches a config value off
	if o.ConfigPath == "" && o.CodeDir != "" {
		if p := filepath.Join(o.CodeDir, ".dockware-clone.yml"); fileExists(p) {
			o.ConfigPath = p
		}
	}
	cfg, err := LoadProjectConfig(o.ConfigPath)
	if err != nil {
		return err
	}
	exclude := append(append([]string(nil), cfg.Exclude...), o.Exclude...)
	maxMB := cfg.MaxCodeSizeMB
	if maxMB == 0 {
		maxMB = defaultMaxCodeSizeMB
	}
	if o.MaxCodeSizeMB != nil {
		if *o.MaxCodeSizeMB < -1 || *o.MaxCodeSizeMB == 0 {
			return &Error{Step: StepConfig, Msg: fmt.Sprintf("--max-code-size must be -1 (no limit) or a size in MB, got %d", *o.MaxCodeSizeMB),
				Hint: "pass the limit in MB, e.g. --max-code-size 500, or -1 for no limit"}
		}
		maxMB = *o.MaxCodeSizeMB
	}
	var maxBytes int64
	if maxMB > 0 {
		maxBytes = int64(maxMB) * 1000 * 1000
	}
	days := cfg.OrdersSinceDays
	if o.OrdersSinceDays != nil {
		if *o.OrdersSinceDays < 0 {
			return &Error{Step: StepConfig, Msg: fmt.Sprintf("--orders-since-days must be 0 (all orders) or more, got %d", *o.OrdersSinceDays),
				Hint: "pass the number of days whose orders are kept, or 0 for all orders"}
		}
		days = *o.OrdersSinceDays
	}
	skipIndex := cfg.SkipSearchIndex
	if o.SkipSearchIndex != nil {
		skipIndex = *o.SkipSearchIndex
	}
	audit.OrdersSinceDays, audit.SkipSearchIndex, audit.CodeExclude = days, skipIndex, exclude

	// docker has to work before the server is touched; the stream strategy
	// needs it even for --dump-only, as shopware-cli is extracted from its image
	if !o.DumpOnly || o.Strategy == "stream" {
		step = StepDocker
		if err := DockerAvailable(ctx); err != nil {
			return err
		}
	}

	// one ssh connection for the whole run; registered before the dump and
	// vendor joins, so it is closed after them (defers run last-in first-out)
	closeSSH, err := r.ShareConnection()
	if err != nil {
		return Fail(StepSSH, "cannot create the ssh control socket directory", err, "")
	}
	defer closeSSH()

	// 1. probe
	step = StepProbe
	fmt.Fprintf(log, "==> probing %s:%s\n", r.Target, r.Root)
	t := time.Now()
	p, err := r.RunProbe(ctx)
	if err != nil {
		return err
	}
	phase("probe", t)
	audit.ShopwareVersion, audit.PHPVersion, audit.MySQLVersion = p.ShopwareVersion, p.PHPVersion, p.MySQLVersion
	fmt.Fprintf(log, "  Shopware %s, PHP %s, MySQL %s, %d tables, %d core tables known\n",
		p.ShopwareVersion, p.PHPVersion, p.MySQLVersion, len(p.Tables), len(p.CoreTables))

	// 2. classify and plan
	allow := append(append([]string(nil), BuiltinAllowedTables...), cfg.AllowTables...)
	noData := append(NoDataTables(), cfg.NoData...)
	if skipIndex {
		noData = append(noData, searchIndexTables...)
	}
	c, err := Classify(p.Tables, p.CoreTables, allow, noData)
	if err != nil {
		return err
	}
	rules := mergeRules(DefaultRules(), cfg.Rules)
	plan := NewDumpPlan(p, c, rules, o.Limits)
	if days > 0 {
		if where := OrderWindowWhere(p, days, time.Now()); where == nil {
			warn("orders_since_days ignored: the source has no order.order_date_time column; all orders are exported")
			audit.OrdersSinceDays = 0
		} else {
			for _, tbl := range sortedKeys(where) {
				plan.AddWhere(tbl, where[tbl])
			}
			fmt.Fprintf(log, "  orders: last %d days (%d tables filtered)\n", days, len(where))
		}
	}
	if skipIndex {
		fmt.Fprintln(log, "  search index: schema-only, rebuilt locally")
	}
	audit.Tables = c
	audit.RulesHash = RulesHash(plan.Rewrites, plan.SchemaOnly, plan.Where)
	for _, cols := range plan.Rewrites {
		audit.Rewrites += len(cols)
	}
	if len(c.Denied) > 0 {
		audit.DeniedRows = map[string]int64{}
		for _, tbl := range c.Denied {
			audit.DeniedRows[tbl] = p.TableRows[tbl]
		}
		fmt.Fprintf(log, "  schema-only (not allowed, add to allow_tables if needed): %s\n", formatRows(audit.DeniedRows))
	}
	fmt.Fprintf(log, "  %d core, %d allowed, %d denied, %d no-data tables; %d rewritten columns; rules %s\n",
		len(c.Core), len(c.Allowed), len(c.Denied), len(c.NoData), audit.Rewrites, audit.RulesHash)

	// 3. dump
	dumpStep := StepDump
	if o.Strategy == "shadow" {
		dumpStep = StepShadow
	} else {
		audit.ShopwareCLIVersion = o.SWCLIVersion
	}
	runDump := func(ctx context.Context) (*DumpResult, error) {
		if o.Strategy == "shadow" {
			return ShadowDump(ctx, r, p, c, plan, o.ShadowDB, filepath.Join(o.OutDir, "dump.sql.gz"), log)
		}
		return StreamDump(ctx, r, p, plan, o.SWCLIVersion, filepath.Join(o.OutDir, "dump.sql.zst"), log)
	}
	recordDump := func(d *DumpResult, took time.Duration) {
		audit.Dump = d
		record("dump", took)
		fmt.Fprintf(log, "  %s, %.1f MB\n", filepath.Base(d.File), float64(d.Bytes)/1e6)
	}
	fmt.Fprintf(log, "==> dumping (%s)\n", o.Strategy)
	if o.DumpOnly {
		step = dumpStep
		t = time.Now()
		dump, err := runDump(ctx)
		if err != nil {
			return err
		}
		recordDump(dump, time.Since(t))
		return nil
	}

	// the dump runs on the server while the local environment is prepared
	// (code, container, PHP extensions): the local steps only need the probe
	// and the plan, and the dump is the longest step. The goroutine touches
	// nothing but its own locals, log and cancelLocal; its result is recorded
	// after the join. A failure on either side cancels the other
	localCtx, cancelLocal := context.WithCancel(ctx)
	defer cancelLocal()
	dumpCtx, cancelDump := context.WithCancel(ctx)
	var (
		dump     *DumpResult
		dumpErr  error
		dumpTook time.Duration
		dumpDone = make(chan struct{})
	)
	go func() {
		s := time.Now()
		dump, dumpErr = runDump(dumpCtx)
		dumpTook = time.Since(s)
		// close before the cancel: a local step that then fails on the
		// cancel finds dumpDone closed and reports the dump error
		close(dumpDone)
		if dumpErr != nil {
			cancelLocal()
		}
	}()
	dumpFailed := func() error { return Fail(dumpStep, "clone failed during "+dumpStep, dumpErr, "") }
	// localFail reports a failed local step; a dump that failed first is the
	// cause, the step's "context canceled" only the effect
	localFail := func(err error) error {
		select {
		case <-dumpDone:
			if dumpErr != nil {
				return dumpFailed()
			}
		default:
		}
		return err
	}
	// a failure of the local path cancels the dump and waits for it, so the
	// ssh process is gone and the dump's own cleanup (remote temp dir,
	// shadow schema) has run before Run returns; a dump that already
	// finished still lands in the audit
	defer func() {
		cancelDump()
		<-dumpDone
		if dumpErr == nil && audit.Dump == nil {
			recordDump(dump, dumpTook)
		}
	}()

	// 4. code
	php, phpSource := p.PHPVersion, "server"
	if cfg.PHP != "" {
		php, phpSource = cfg.PHP, "config"
	}
	if o.PHP != "" {
		php, phpSource = o.PHP, "--php"
	}
	l := &Local{Name: o.Name, Dir: o.OutDir, Image: o.Image, AppEnv: o.AppEnv, PHPVersion: php}
	t = time.Now()
	if o.CodeDir != "" {
		l.CodeDir = o.CodeDir
		if len(exclude) > 0 {
			warn("exclude patterns only apply to the code sync from the server; --code uses the checkout as it is")
		}
		if remoteHash, err := r.Output(localCtx, "cd "+ShellQuote(r.Root)+" && php -r 'echo hash_file(\"sha256\", \"composer.lock\");'"); err == nil {
			if local, err := fileSHA256(filepath.Join(l.CodeDir, "composer.lock")); err == nil && local != strings.TrimSpace(remoteHash) {
				warn("local composer.lock differs from the server: plugin versions and migrations may not match the cloned database")
			}
		}
	} else {
		step = StepRsync
		l.CodeDir, l.OwnsCode = filepath.Join(o.OutDir, "code"), true
		fmt.Fprintln(log, "==> syncing code")
		if err := SyncCode(localCtx, r, l.CodeDir, CodeSyncOptions{Exclude: exclude, MaxBytes: maxBytes}, log); err != nil {
			return localFail(err)
		}
	}
	phase("code", t)
	node, nodeSource := o.Node, "--node"
	if node == "" {
		node, nodeSource = cfg.Node, "config"
	}
	// an explicit version also decides the frontend image, before any detection
	l.NodePinned = node != ""
	if node == "" {
		node, nodeSource = DetectNodeVersion(l.CodeDir), "checkout"
	}
	if node == "" {
		node, nodeSource = p.NodeVersion, "server"
	}
	l.NodeVersion = node
	audit.NodeVersion = node
	if node == "" {
		fmt.Fprintf(log, "  PHP %s (%s), Node: container default\n", php, phpSource)
	} else {
		fmt.Fprintf(log, "  PHP %s (%s), Node %s (%s)\n", php, phpSource, node, nodeSource)
	}

	step = StepComposer
	auth, err := FindComposerAuth(o.ComposerAuth, l.CodeDir)
	if err != nil {
		return localFail(err)
	}
	if auth.Source == "" {
		fmt.Fprintln(log, "  composer auth: none found")
		if p.HasComposerAuth {
			warn("the server uses composer credentials (auth.json) for private repositories; composer install will likely fail without local credentials (--composer-auth)")
		}
	} else {
		fmt.Fprintf(log, "  composer auth: %s\n", auth.Source)
	}

	// 5. local environment
	step = StepDocker
	l.Port = FreePort(o.Port)
	var unset, reserved []string
	l.Env, unset, reserved = BuildEnv(p, l, cfg.Env)
	audit.UnsetEnv = unset
	if len(unset) > 0 {
		fmt.Fprintf(log, "  env keys set empty (secrets or unknown, override via env: in the config): %s\n", strings.Join(unset, ", "))
	}
	for _, k := range reserved {
		warn("env: %s is controlled by the dockware image and ignored; use --php/--node", k)
	}
	l.Redis = NeedsRedis(p)
	if ValidMediaURL(p.MediaURL) {
		l.MediaURL = p.MediaURL
	} else {
		warn("no usable media url on the source (%q): media missing locally will 404", p.MediaURL)
	}
	if cfg.Frontend != nil && !o.NoFrontend {
		l.Frontend = cfg.Frontend
		l.FrontPort = FreePort(o.FrontendPort)
	}
	if err := l.WriteFiles(); err != nil {
		return localFail(err)
	}
	fmt.Fprintf(log, "==> starting %s (%s, PHP %s, port %d)\n", l.container(), l.Image, l.PHPVersion, l.Port)
	t = time.Now()
	if err := l.Up(localCtx); err != nil {
		return localFail(err)
	}
	phase("container", t)
	if err := l.SeedVar(localCtx, log); err != nil {
		return localFail(err)
	}
	// the server's PHP extensions, before composer checks the platform and
	// the shop runs code that needs them (zstd cache compression, imagick)
	t = time.Now()
	missing, err := l.EnsurePHPExtensions(localCtx, p.PHPExtensions, log)
	if err != nil {
		return localFail(err)
	}
	for _, ext := range missing {
		warn("php extension %s exists on the server but not locally: install it in the image, code paths using it will fail", ext)
	}
	phase("php-extensions", t)

	// 6. vendor and import in parallel; the import waits for the dump
	fmt.Fprintln(log, "==> installing vendor and importing database")
	t = time.Now()
	vendorCtx, cancelVendor := context.WithCancel(localCtx)
	var (
		vendorErr     error
		vendorTook    time.Duration
		nodeInstalled string
		vendorDone    = make(chan struct{})
	)
	go func() {
		defer close(vendorDone)
		s := time.Now()
		// both carry the tail of their own output in the error; UseNode only
		// logs a warning when the image has no node and none was requested
		nodeInstalled, vendorErr = l.UseNode(vendorCtx, log)
		if vendorErr == nil {
			vendorErr = l.InstallVendor(vendorCtx, auth, io.Discard)
		}
		vendorTook = time.Since(s)
	}()
	// a failed dump or import stops the vendor install instead of waiting minutes for it
	defer func() {
		cancelVendor()
		<-vendorDone
	}()
	<-dumpDone
	if dumpErr != nil {
		return dumpFailed()
	}
	recordDump(dump, dumpTook)
	step = StepImport
	importStart := time.Now()
	importErr := l.Import(ctx, dump, log)
	importTook := time.Since(importStart)
	if importErr != nil {
		// no waiting minutes for composer after a failed import
		cancelVendor()
	}
	<-vendorDone
	audit.Phases["vendor"], audit.Phases["import"] = vendorTook, importTook
	// a cancel fails both; it must not read as an import or composer failure
	if err := ctx.Err(); err != nil {
		return err
	}
	if importErr != nil {
		return Fail(StepImport, "database import failed", importErr, "")
	}
	if vendorErr != nil {
		return Fail(StepComposer, "vendor install failed", vendorErr, "")
	}
	audit.NodeVersion = strings.TrimSpace(nodeInstalled)
	if audit.NodeVersion != "" {
		fmt.Fprintf(log, "  node %s\n", audit.NodeVersion)
	}
	phase("vendor+import", t)

	// 7. verification before anything is added locally
	step = StepVerify
	fmt.Fprintln(log, "==> verifying anonymisation")
	t = time.Now()
	checks, err := Verify(ctx, r, l, p, c, rules, o.VerifySample)
	if err != nil {
		return err
	}
	audit.Checks = checks
	phase("verify", t)
	failedVerify := printChecks(log, checks)

	// 8. make it run locally
	step = StepPostImport
	fmt.Fprintln(log, "==> preparing shop")
	t = time.Now()
	domains := DomainMap(p.Domains, p.AppURL, l.Port)
	if err := l.PostImport(ctx, p, domains, cfg.PostSQLPath(), log); err != nil {
		return err
	}
	phase("post-import", t)
	if skipIndex {
		fmt.Fprintln(log, "==> rebuilding search index")
		t = time.Now()
		if out, err := l.Console(ctx, "dal:refresh:index", "--only=product.indexer"); err != nil {
			return Fail(StepPostImport, "search index rebuild failed", err, out)
		}
		phase("search-index", t)
	}
	keys, order, err := l.AccessKeys(ctx)
	if err != nil {
		return err
	}

	// 9. frontend
	if l.Frontend != nil {
		step = StepFrontend
		env, err := l.FrontendEnv(keys, order)
		if err != nil {
			return err
		}
		fmt.Fprintf(log, "==> starting frontend %s (%s) on port %d\n", l.Frontend.Path, l.FrontendImage(), l.FrontPort)
		if err := l.StartFrontend(ctx, env); err != nil {
			return err
		}
	}

	// 10. smoke tests
	step = StepSmoke
	fmt.Fprintln(log, "==> smoke tests")
	t = time.Now()
	smoke := Smoke(ctx, l, p, domains, keys, order)
	audit.Checks = append(audit.Checks, smoke...)
	phase("smoke", t)
	failedSmoke := printChecks(log, smoke)

	compose := filepath.Join(o.OutDir, "docker-compose.yml")
	fmt.Fprintf(log, "\nShop:     %s\nAdmin:    %s/admin (admin / shopware)\n", l.ShopURL(), l.ShopURL())
	for _, from := range sortedKeys(domains) {
		fmt.Fprintf(log, "Domain:   %s -> %s\n", from, domains[from])
	}
	for _, name := range order {
		fmt.Fprintf(log, "Access:   %-30s %s\n", name, keys[name])
	}
	if l.Frontend != nil {
		fmt.Fprintf(log, "Frontend: http://localhost:%d\n", l.FrontPort)
	}
	fmt.Fprintf(log, "Compose:  docker compose -f %s ...\n", compose)
	if failedVerify > 0 {
		return &Error{Step: StepVerify, Msg: fmt.Sprintf("%d checks failed, see above", failedVerify+failedSmoke),
			Hint: "the listed columns still carry server values: add rules for them in .dockware-clone.yml and do not share this clone"}
	}
	if failedSmoke > 0 {
		return &Error{Step: StepSmoke, Msg: fmt.Sprintf("%d checks failed, see above", failedSmoke),
			Hint: "the shop is up but not healthy: see the failed checks above and `docker compose -f " + compose + " logs shop`"}
	}
	return nil
}

func printChecks(log io.Writer, checks []Check) int {
	failed := 0
	for _, c := range checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
			failed++
		}
		if c.Detail != "" {
			fmt.Fprintf(log, "  %s %s: %s\n", mark, c.Name, c.Detail)
		} else {
			fmt.Fprintf(log, "  %s %s\n", mark, c.Name)
		}
	}
	return failed
}

// mergeRules appends project rules; a project rule replaces a default for the same column.
func mergeRules(defaults, extra []Rule) []Rule {
	idx := map[string]int{}
	out := append([]Rule(nil), defaults...)
	for i, r := range out {
		idx[r.Table+"."+r.Column] = i
	}
	for _, r := range extra {
		if i, ok := idx[r.Table+"."+r.Column]; ok {
			out[i] = r
			continue
		}
		idx[r.Table+"."+r.Column] = len(out)
		out = append(out, r)
	}
	return out
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	return strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// insideDir reports whether path equals dir or lies below it, following
// symlinks in the longest existing prefix of each path.
func insideDir(path, dir string) bool {
	rel, err := filepath.Rel(resolveExisting(dir), resolveExisting(path))
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveExisting evaluates symlinks in the longest existing prefix of path.
func resolveExisting(path string) string {
	rest := ""
	for p := path; ; {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return path
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

func fileSHA256(p string) (string, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func formatRows(m map[string]int64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s(~%d)", k, m[k])
	}
	return strings.Join(parts, ", ")
}

// httpClient resolves every *.localhost name to 127.0.0.1, like browsers do.
var httpClient = &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err == nil && (host == "localhost" || strings.HasSuffix(host, ".localhost")) {
			addr = net.JoinHostPort("127.0.0.1", port)
		}
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, addr)
	}},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func httpCheck(ctx context.Context, name, method, u string, headers map[string]string, body string, want int) (Check, []byte) {
	req, err := http.NewRequestWithContext(ctx, method, u, strings.NewReader(body))
	if err != nil {
		return Check{Name: name, Detail: err.Error()}, nil
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return Check{Name: name, Detail: err.Error()}, nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return Check{Name: name, OK: resp.StatusCode == want, Detail: fmt.Sprintf("%s %s → %d, %d bytes", method, u, resp.StatusCode, len(data))}, data
}

// Smoke exercises storefront, Store API (headless), Admin API and frontend.
func Smoke(ctx context.Context, l *Local, p *Probe, domains map[string]string, keys map[string]string, order []string) []Check {
	var checks []Check
	if p.HasStorefront {
		for _, d := range p.Domains {
			if d.TypeID != SalesChannelTypeStorefront || domains[d.URL] == "" {
				continue
			}
			c, _ := httpCheck(ctx, "storefront "+d.Name, http.MethodGet, domains[d.URL]+"/", nil, "", http.StatusOK)
			checks = append(checks, c)
			break
		}
	}
	for _, name := range order {
		c, _ := httpCheck(ctx, "store-api "+name, http.MethodPost, l.ShopURL()+"/store-api/product", map[string]string{
			"sw-access-key": keys[name], "Content-Type": "application/json", "Accept": "application/json",
		}, `{"limit":1}`, http.StatusOK)
		checks = append(checks, c)
	}
	c, _ := httpCheck(ctx, "admin api login", http.MethodPost, l.ShopURL()+"/api/oauth/token", map[string]string{"Content-Type": "application/json"},
		`{"grant_type":"password","client_id":"administration","scopes":"write","username":"admin","password":"shopware"}`, http.StatusOK)
	checks = append(checks, c)

	if l.Frontend != nil {
		u := "http://localhost:" + fmt.Sprint(l.FrontPort) + "/"
		// dev servers compile on first request
		deadline := time.Now().Add(10 * time.Minute)
		var last Check
		for time.Now().Before(deadline) {
			last, _ = httpCheck(ctx, "frontend SSR "+l.Frontend.Path, http.MethodGet, u, nil, "", http.StatusOK)
			if last.OK {
				break
			}
			select {
			case <-ctx.Done():
				return append(checks, last)
			case <-time.After(5 * time.Second):
			}
		}
		checks = append(checks, last)
	}
	return checks
}
