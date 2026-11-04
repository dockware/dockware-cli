package clone

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func coreSet(extra ...string) map[string]bool {
	set := map[string]bool{}
	for _, t := range append(append([]string(nil), requiredCoreTables...), extra...) {
		set[t] = true
	}
	return set
}

func TestClassifyDefaultDenyAndPrecedence(t *testing.T) {
	remote := []string{"product", "order", "customer", "system_config", "sales_channel", "migration", "plugin",
		"log_entry", "acme_api_log", "acme_loyalty_member", "swag_cms_extensions_quickview", "Acme_Product_Benefit"}
	c, err := Classify(remote, coreSet("log_entry"), append(BuiltinAllowedTables, "acme_product_*"), NoDataTables())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"denied":  {"acme_loyalty_member"},
		"allowed": {"Acme_Product_Benefit", "swag_cms_extensions_quickview"},
		// no-data wins over core: log_entry is core but must not carry rows
		"nodata": {"acme_api_log", "log_entry"},
	}
	got := map[string][]string{"denied": c.Denied, "allowed": c.Allowed, "nodata": c.NoData}
	for k, w := range want {
		if strings.Join(got[k], ",") != strings.Join(w, ",") {
			t.Errorf("%s = %v, want %v", k, got[k], w)
		}
	}
	if strings.Join(c.SchemaOnly(), ",") != "acme_api_log,acme_loyalty_member,log_entry" {
		t.Errorf("SchemaOnly = %v", c.SchemaOnly())
	}
}

func TestClassifyRejectsIncompleteCoreDetection(t *testing.T) {
	core := coreSet()
	delete(core, "customer")
	if _, err := Classify([]string{"customer"}, core, nil, nil); err == nil {
		t.Fatal("expected error: a missing core table would export customer as schema-only and hide the broken scan")
	}
}

func TestProjectRuleReplacesDefault(t *testing.T) {
	rules := mergeRules(DefaultRules(), []Rule{{Table: "customer", Column: "email", Kind: KindNull}, {Table: "acme_x", Column: "mail", Kind: KindEmail}})
	schema := Schema{
		"customer": {"id": {Name: "id", DataType: "binary"}, "email": {Name: "email", DataType: "varchar"}},
		"acme_x":   {"id": {Name: "id", DataType: "binary"}, "mail": {Name: "mail", DataType: "varchar", Nullable: true}},
	}
	m := RewriteMap(rules, schema)
	if m["customer"]["email"] != "''" {
		t.Errorf("customer.email = %q, want project rule (null kind on NOT NULL column)", m["customer"]["email"])
	}
	if !strings.Contains(m["acme_x"]["mail"], "@example.com") {
		t.Errorf("acme_x.mail rule missing: %q", m["acme_x"]["mail"])
	}
	if len(m) != 2 {
		t.Errorf("rules for absent tables/columns must be skipped, got tables %v", m)
	}
}

func TestDomainMapKeepsPathsAndUniqueness(t *testing.T) {
	domains := []Domain{
		{URL: "https://stage.example.com"},
		{URL: "https://stage.example.com/en"},
		{URL: "https://shop.example.ch"},
		{URL: "http://stage.example.com"}, // same local URL as the https entry, must stay unmapped
		{URL: ""},
		{URL: "default.headless0"}, // headless channel placeholder, no host
	}
	got := DomainMap(domains, "https://stage.example.com", 8001)
	want := map[string]string{
		"https://stage.example.com":    "http://localhost:8001",
		"https://stage.example.com/en": "http://localhost:8001/en",
		"https://shop.example.ch":      "http://shop-example-ch.localhost:8001",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s -> %s, want %s", k, got[k], v)
		}
	}
}

// A developer checkout must stay git-clean: Shopware writes var/ (tracked in
// some projects) and `nuxt dev` removes .output, which must not be a volume.
func TestComposeKeepsShopwareWritesOutOfCheckout(t *testing.T) {
	l := &Local{Name: "x", CodeDir: "/src", Port: 8001, FrontPort: 3001, Frontend: &FrontendConfig{Path: "front", Image: "node:20", Install: "true", Command: "true", Port: 3000}}
	out, err := l.Compose()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "var:/var/www/html/var") {
		t.Errorf("local checkout needs a var volume:\n%s", out)
	}
	if strings.Contains(string(out), "/.output") {
		t.Errorf(".output must not be a volume mountpoint:\n%s", out)
	}
	l.OwnsCode = true
	out, _ = l.Compose()
	if strings.Contains(string(out), "var:/var/www/html/var") {
		t.Errorf("synced code owns var/, no volume expected:\n%s", out)
	}
}

func TestBuildEnvNeverReusesRemoteSecrets(t *testing.T) {
	p := &Probe{
		EnvKeys: []string{"APP_SECRET", "DATABASE_URL", "STRIPE_SECRET_KEY", "REDIS_CACHE_HOST", "REDIS_PASSWORD", "REDIS_DSN_CACHE", "REDIS_DSN_CACHE_HTTP", "SHOPWARE_CDN_STRATEGY_DEFAULT", "ACME_ERP_URL", "SHOPWARE_ADMIN_ES_ENABLED"},
		SafeEnv: map[string]string{"SHOPWARE_CDN_STRATEGY_DEFAULT": "id"},
	}
	l := &Local{Port: 8001, AppEnv: "prod"}
	env, unset, reserved := BuildEnv(p, l, map[string]string{"ACME_ERP_URL": "http://mock"})
	if env["DATABASE_URL"] != "mysql://root:root@127.0.0.1:3306/shopware" || env["APP_URL"] != "http://localhost:8001" {
		t.Errorf("local connection values wrong: %v", env)
	}
	if env["STRIPE_SECRET_KEY"] != "" || env["REDIS_PASSWORD"] != "" {
		t.Errorf("secrets must be empty: %q %q", env["STRIPE_SECRET_KEY"], env["REDIS_PASSWORD"])
	}
	if env["REDIS_CACHE_HOST"] != "redis" || env["SHOPWARE_CDN_STRATEGY_DEFAULT"] != "id" || env["ACME_ERP_URL"] != "http://mock" {
		t.Errorf("mapped/copied/override values wrong: %v", env)
	}
	// real shop: the server's SHOPWARE_ADMIN_ES_ENABLED=1 looked safe to copy and made the indexer log "No alive nodes found"
	if env["SHOPWARE_ADMIN_ES_ENABLED"] != "0" || env["SHOPWARE_ADMIN_ES_REFRESH_INDICES"] != "0" {
		t.Errorf("admin search must be off locally: %q %q", env["SHOPWARE_ADMIN_ES_ENABLED"], env["SHOPWARE_ADMIN_ES_REFRESH_INDICES"])
	}
	// the suffix is not always URL/DSN: Shopware's http_cache and cache.yaml use REDIS_DSN_CACHE(_HTTP)
	if env["REDIS_DSN_CACHE"] != "redis://redis:6379" || env["REDIS_DSN_CACHE_HTTP"] != "redis://redis:6379" {
		t.Errorf("redis DSN keys must point at the compose redis: %q %q", env["REDIS_DSN_CACHE"], env["REDIS_DSN_CACHE_HTTP"])
	}
	if strings.Join(unset, ",") != "STRIPE_SECRET_KEY" {
		t.Errorf("unset = %v, want only STRIPE_SECRET_KEY (override removes ACME_ERP_URL)", unset)
	}
	if len(env["APP_SECRET"]) != 64 {
		t.Errorf("APP_SECRET must be freshly generated")
	}
	if len(reserved) != 0 {
		t.Errorf("reserved = %v, want none", reserved)
	}
}

func TestBuildEnvPointsRedisSchemedSecretsAtLocalRedis(t *testing.T) {
	// real shop: shopware.redis.connections.cart.dsn reads CART_DSN; the key looks
	// like a credential, so the probe reports only the scheme of its value
	p := &Probe{
		EnvKeys:    []string{"CART_DSN", "SMOXY_URL", "STRIPE_SECRET_KEY"},
		EnvSchemes: map[string]string{"CART_DSN": "redis", "SMOXY_URL": "https"},
	}
	l := &Local{Port: 8001, AppEnv: "prod"}
	env, unset, _ := BuildEnv(p, l, nil)
	if env["CART_DSN"] != "redis://redis:6379" {
		t.Errorf("CART_DSN = %q, want the compose redis", env["CART_DSN"])
	}
	if env["SMOXY_URL"] != "" {
		t.Errorf("SMOXY_URL = %q, want empty (https is no local service)", env["SMOXY_URL"])
	}
	if strings.Join(unset, ",") != "SMOXY_URL,STRIPE_SECRET_KEY" {
		t.Errorf("unset = %v, want SMOXY_URL,STRIPE_SECRET_KEY (CART_DSN is mapped)", unset)
	}
	if !NeedsRedis(p) {
		t.Error("a redis-schemed DSN without REDIS* keys must still start redis")
	}
	if NeedsRedis(&Probe{EnvKeys: []string{"SMOXY_URL"}, EnvSchemes: map[string]string{"SMOXY_URL": "https"}}) {
		t.Error("an https URL must not start redis")
	}
}

func TestBuildEnvKeepsImageKeysOutOfTheContainer(t *testing.T) {
	// the dockware entrypoint switches node/PHP to NODE_VERSION/PHP_VERSION and
	// exits when the version is not installed; an empty value is a version too
	p := &Probe{
		EnvKeys: []string{"NODE_VERSION", "NVM_VERSION", "PHP_VERSION", "TZ", "HOME", "APP_URL", "ACME_ERP_URL"},
		SafeEnv: map[string]string{"NODE_VERSION": "v16.0.0", "NVM_VERSION": "v0.39.1", "PHP_VERSION": "8.1", "TZ": "Europe/Berlin"},
	}
	l := &Local{Port: 8001, AppEnv: "prod"}
	env, unset, reserved := BuildEnv(p, l, map[string]string{"NODE_VERSION": "20", "PHP_VERSION": "8.3", "ACME_ERP_URL": "http://mock"})
	for _, k := range []string{"NODE_VERSION", "NVM_VERSION", "PHP_VERSION", "TZ", "HOME"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s must not be emitted at all (got %q)", k, env[k])
		}
	}
	if strings.Join(unset, ",") != "" {
		t.Errorf("unset = %v, want none (image keys are neither copied nor set empty)", unset)
	}
	if strings.Join(reserved, ",") != "NODE_VERSION,PHP_VERSION" {
		t.Errorf("reserved = %v, want the ignored overrides NODE_VERSION,PHP_VERSION", reserved)
	}
	if env["ACME_ERP_URL"] != "http://mock" || env["APP_URL"] != "http://localhost:8001" {
		t.Errorf("other overrides and known keys must still apply: %v", env)
	}
}

func TestBuildEnvAppDebugFollowsAppEnv(t *testing.T) {
	// the server's APP_DEBUG=1 doubled the local TTFB, so it is never copied
	p := &Probe{EnvKeys: []string{"APP_DEBUG"}}
	for appEnv, want := range map[string]string{"prod": "0", "dev": "1"} {
		env, unset, _ := BuildEnv(p, &Local{Port: 8001, AppEnv: appEnv}, nil)
		if env["APP_DEBUG"] != want || len(unset) != 0 {
			t.Errorf("APP_ENV=%s: APP_DEBUG = %q (unset %v), want %q", appEnv, env["APP_DEBUG"], unset, want)
		}
	}
	env, _, _ := BuildEnv(p, &Local{Port: 8001, AppEnv: "prod"}, map[string]string{"APP_DEBUG": "1"})
	if env["APP_DEBUG"] != "1" {
		t.Errorf("config override must win: APP_DEBUG = %q", env["APP_DEBUG"])
	}
}

func TestFrontendEnvPlaceholders(t *testing.T) {
	l := &Local{Port: 8001, FrontPort: 3001, Frontend: &FrontendConfig{Env: map[string]string{
		"SERVER": "${SHOP_URL_INTERNAL}/store-api",
		"CLIENT": "${SHOP_URL}/store-api",
		"KEY":    "${ACCESS_KEY:Shop EU}",
		"FIRST":  "${ACCESS_KEY}",
		"SELF":   "${FRONTEND_URL}",
	}}}
	env, err := l.FrontendEnv(map[string]string{"Headless": "K1", "Shop EU": "K2"}, []string{"Headless", "Shop EU"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"SERVER": "http://shop/store-api", "CLIENT": "http://localhost:8001/store-api", "KEY": "K2", "FIRST": "K1", "SELF": "http://localhost:3001"}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	l.Frontend.Env = map[string]string{"KEY": "${ACCESS_KEY:Missing}"}
	if _, err := l.FrontendEnv(map[string]string{}, nil); err == nil {
		t.Error("unknown sales channel name must fail instead of starting the frontend without a key")
	}
}

// importFixtureDump is a shopware-cli style dump: a multi-line INSERT whose
// rows carry a ';' and a lock keyword, a table with two statements, the
// FOREIGN_KEY_CHECKS trailer after the last data and a view section.
const importFixtureDump = "SET NAMES utf8mb4;\nSET FOREIGN_KEY_CHECKS = 0;\n\n" +
	"-- Structure for table `a`\nDROP TABLE IF EXISTS `a`;\nCREATE TABLE `a` (\n  `id` int NOT NULL,\n  `name` varchar(16),\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB;\n\n" +
	"-- Data for table `a` -- 2 rows\nLOCK TABLES `a` WRITE;\nINSERT INTO `a` (`id`, `name`) VALUES\n(1, 'x;'),\n(2, 'LOCK TABLES');\nUNLOCK TABLES;\n\n" +
	"-- Structure for table `b`\nDROP TABLE IF EXISTS `b`;\nCREATE TABLE `b` (\n  `id` int NOT NULL\n) ENGINE=InnoDB;\n\n" +
	"-- Data for table `b` -- 4 rows\nLOCK TABLES `b` WRITE;\nINSERT INTO `b` (`id`) VALUES\n(1),\n(2);\nINSERT INTO `b` (`id`) VALUES\n(3),\n(4);\nUNLOCK TABLES;\nSET FOREIGN_KEY_CHECKS = 1;\n\n" +
	"--\n-- Structure for view `v`\n--\n\nDROP VIEW IF EXISTS `v`;\nCREATE VIEW `v` AS SELECT `id` FROM `a`;\n"

// importFixtureTriggerDump ends with a schema-only table (the default
// no-data list makes that the normal case), the trailer and a trigger in
// shopware-cli's DELIMITER form.
const importFixtureTriggerDump = "SET NAMES utf8mb4;\nSET FOREIGN_KEY_CHECKS = 0;\n\n" +
	"--\n-- Structure for table `a`\n--\n\nDROP TABLE IF EXISTS `a`;\nCREATE TABLE `a` (\n  `id` int NOT NULL\n) ENGINE=InnoDB;\n\n" +
	"--\n-- Data for table `a` -- 1 rows\nLOCK TABLES `a` WRITE;\nINSERT INTO `a` (`id`) VALUES\n(1);\nUNLOCK TABLES;\n\n" +
	"--\n-- Structure for table `log`\n--\n\nDROP TABLE IF EXISTS `log`;\nCREATE TABLE `log` (\n  `id` int NOT NULL\n) ENGINE=InnoDB;\nSET FOREIGN_KEY_CHECKS = 1;\n\n" +
	"--\n-- Trigger `t_ins`\n--\n\nDROP TRIGGER IF EXISTS `t_ins`;\nDELIMITER //\n" +
	"CREATE TRIGGER `t_ins` AFTER INSERT ON `a` FOR EACH ROW BEGIN\n  INSERT INTO `log` (`id`) VALUES (NEW.`id`);\nEND//\nDELIMITER ;\n"

// splitDump runs importSplitter on dump with max=1 (every statement its own
// part) and returns the work dir and a reader of its files.
func splitDump(t *testing.T, dump string) (string, func(string) string) {
	t.Helper()
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("awk not installed")
	}
	dir := t.TempDir()
	cmd := exec.Command("awk", "-v", "dir="+dir, "-v", "max=1", importSplitter)
	cmd.Stdin = strings.NewReader(dump)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("awk: %v\n%s", err, out)
	}
	return dir, func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
}

// A part that ends mid-statement or mixes tables, a dropped row or a lock
// statement in a part all break the parallel data phase; a view section
// switching back to schema.sql would drag the triggers after it along.
func TestImportSplitterSeparatesSchemaDataAndTrailer(t *testing.T) {
	dir, read := splitDump(t, importFixtureDump)
	schema := read("schema.sql")
	for _, want := range []string{"CREATE TABLE `a`", "CREATE TABLE `b`"} {
		if !strings.Contains(schema, want) {
			t.Errorf("schema.sql lacks %s:\n%s", want, schema)
		}
	}
	if strings.Contains(schema, "VIEW") {
		t.Errorf("schema.sql must not hold the view:\n%s", schema)
	}
	if strings.Contains(schema, "INSERT") {
		t.Errorf("schema.sql must hold no data:\n%s", schema)
	}
	parts, _ := filepath.Glob(filepath.Join(dir, "*.part.sql"))
	if len(parts) != 3 {
		t.Fatalf("parts = %v, want 3 (a, then b split at max=1 between its statements)", parts)
	}
	const preamble = "SET NAMES utf8mb4;\nSET FOREIGN_KEY_CHECKS = 0;\nSET UNIQUE_CHECKS = 0;\n"
	rows := map[string]int{}
	for _, p := range parts {
		part := read(filepath.Base(p))
		if !strings.HasPrefix(part, preamble) {
			t.Errorf("%s lacks the session preamble:\n%s", p, part)
		}
		if strings.Contains(part, "INSERT INTO `a`") == strings.Contains(part, "INSERT INTO `b`") {
			t.Errorf("%s must hold exactly one table:\n%s", p, part)
		}
		lines := strings.Split(strings.TrimRight(part, "\n"), "\n")
		if last := lines[len(lines)-1]; !strings.HasSuffix(last, ";") {
			t.Errorf("%s ends mid-statement: %q", p, last)
		}
		for _, line := range lines {
			if strings.HasPrefix(line, "LOCK TABLES") || strings.HasPrefix(line, "UNLOCK TABLES") {
				t.Errorf("%s keeps a lock statement: %q", p, line)
			}
			if strings.HasPrefix(line, "(") {
				rows[line]++
			}
		}
	}
	want := []string{"(1, 'x;'),", "(2, 'LOCK TABLES');", "(1),", "(2);", "(3),", "(4);"}
	for _, row := range want {
		if rows[row] != 1 {
			t.Errorf("row %q written %d times, want once", row, rows[row])
		}
	}
	if len(rows) != len(want) {
		t.Errorf("rows in parts = %v, want exactly %v", rows, want)
	}
	post := read("post.sql")
	if !strings.HasPrefix(post, preamble) {
		t.Errorf("post.sql lacks the session preamble:\n%s", post)
	}
	if !strings.Contains(post, "SET FOREIGN_KEY_CHECKS = 1;") || strings.Contains(post, "INSERT") {
		t.Errorf("post.sql must hold the trailer and no data:\n%s", post)
	}
	if i := strings.Index(post, "CREATE VIEW `v`"); i < 0 || i < strings.Index(post, "SET FOREIGN_KEY_CHECKS = 1;") {
		t.Errorf("post.sql must hold the view after the trailer:\n%s", post)
	}
}

// A trigger in schema.sql fires on every row of the parallel load; a serial
// import of the dump creates it only after the data.
func TestImportSplitterSendsTriggersAfterSchemaOnlyTableToPost(t *testing.T) {
	dir, read := splitDump(t, importFixtureTriggerDump)
	schema := read("schema.sql")
	if !strings.Contains(schema, "CREATE TABLE `log`") {
		t.Errorf("schema.sql lacks the schema-only table:\n%s", schema)
	}
	if strings.Contains(schema, "TRIGGER") || strings.Contains(schema, "DELIMITER") {
		t.Errorf("schema.sql must not hold the trigger:\n%s", schema)
	}
	if parts, _ := filepath.Glob(filepath.Join(dir, "*.part.sql")); len(parts) != 1 {
		t.Errorf("parts = %v, want 1", parts)
	}
	post := read("post.sql")
	if !strings.HasPrefix(post, "SET NAMES utf8mb4;\nSET FOREIGN_KEY_CHECKS = 0;\nSET UNIQUE_CHECKS = 0;\n") {
		t.Errorf("post.sql lacks the session preamble:\n%s", post)
	}
	trigger := "DROP TRIGGER IF EXISTS `t_ins`;\nDELIMITER //\nCREATE TRIGGER `t_ins` AFTER INSERT ON `a` FOR EACH ROW BEGIN\n  INSERT INTO `log` (`id`) VALUES (NEW.`id`);\nEND//\nDELIMITER ;\n"
	if !strings.Contains(post, trigger) {
		t.Errorf("post.sql must hold the whole trigger section:\n%s", post)
	}
	if strings.Contains(post, "INSERT INTO `a`") {
		t.Errorf("post.sql must hold no data:\n%s", post)
	}
}
