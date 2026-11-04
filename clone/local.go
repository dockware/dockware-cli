package clone

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	localDB       = "shopware"
	localMySQL    = "MYSQL_PWD=root mysql -h127.0.0.1 -uroot --default-character-set=utf8mb4"
	containerRoot = "/var/www/html"
)

// Local describes the generated dockware environment.
type Local struct {
	Name       string
	Dir        string // output directory with compose file, dump, logs
	CodeDir    string // project code mounted to /var/www/html
	OwnsCode   bool   // code was synced by the clone (safe to write .env)
	Port       int
	Image      string
	PHPVersion string
	AppEnv     string
	Env        map[string]string
	Redis      bool
	Frontend   *FrontendConfig
	FrontPort  int
	// MediaURL is where media and thumbnails missing locally are redirected to.
	MediaURL string
	// NodeVersion is the node major the shop container runs (via nvm); "" keeps the image default.
	NodeVersion string
	// NodePinned marks NodeVersion as requested via --node or the config, so it
	// also decides the frontend image instead of what the checkout pins.
	NodePinned bool
}

func (l *Local) container() string { return l.Name + "-shop" }

// ShopURL is the local storefront base URL.
func (l *Local) ShopURL() string { return "http://localhost:" + strconv.Itoa(l.Port) }

// FreePort returns the first free TCP port starting at from. It probes all
// interfaces, which is where Docker publishes ports.
func FreePort(from int) int {
	for p := from; p < from+200; p++ {
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(p))
		if err == nil {
			ln.Close()
			return p
		}
	}
	return from
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// containerEnvKeys are defined by the dockware image (its ENV) or the shell.
// The entrypoint acts on PHP_VERSION and NODE_VERSION at start and exits
// when that version is not installed; an empty value counts as a version
// too, so a server .env carrying such keys must not reach shop.env at all.
// The clone sets PHP_VERSION in the compose file and switches node itself
// (UseNode).
var containerEnvKeys = map[string]bool{
	"PATH": true, "TZ": true, "PHP_VERSION": true, "NODE_VERSION": true, "NVM_VERSION": true,
	"APACHE_DOCROOT": true, "SHOP_DOMAIN": true, "RECOVERY_MODE": true, "SSH_USER": true, "SSH_PWD": true,
	"XDEBUG_REMOTE_HOST": true, "XDEBUG_CONFIG": true, "PHP_IDE_CONFIG": true, "XDEBUG_ENABLED": true,
	"FILEBEAT_ENABLED": true, "SUPERVISOR_ENABLED": true, "TIDEWAYS_KEY": true,
	"SW_CURRENCY": true, "SW_API_ACCESS_KEY": true, "SW_TASKS_ENABLED": true,
	"BASH_ENV": true, "DEBIAN_FRONTEND": true,
	"HOME": true, "HOSTNAME": true, "USER": true, "SHELL": true, "PWD": true,
}

// BuildEnv derives the local environment from the remote .env key list.
// Known keys get local values, provably harmless values are copied, every
// other remote key is set empty and reported, so no remote secret is reused.
// Keys the image owns (containerEnvKeys) are left out, also when an override
// names them; those overrides are returned as reserved so the caller can warn.
func BuildEnv(p *Probe, l *Local, overrides map[string]string) (env map[string]string, unset, reserved []string) {
	// a copied APP_DEBUG=1 doubled the local TTFB: the debug container checks
	// every file on the macOS bind mount (home 2.05 → 1.26 s, category 3.7 → 2.5 s)
	debug := "0"
	if l.AppEnv == "dev" {
		debug = "1"
	}
	env = map[string]string{
		"APP_ENV":                      l.AppEnv,
		"APP_DEBUG":                    debug,
		"APP_SECRET":                   randomHex(32),
		"APP_URL":                      l.ShopURL(),
		"APP_URL_CHECK_DISABLED":       "1",
		"DATABASE_URL":                 "mysql://root:root@127.0.0.1:3306/" + localDB,
		"MAILER_DSN":                   "smtp://127.0.0.1:1025",
		"INSTANCE_ID":                  randomHex(16),
		"LOCK_DSN":                     "flock",
		"SHOPWARE_HTTP_CACHE_ENABLED":  "0",
		"SHOPWARE_ES_ENABLED":          "0",
		"SHOPWARE_ES_INDEXING_ENABLED": "0",
		// no search node locally; a copied admin ES flag makes every indexer run log "No alive nodes found in cluster"
		"SHOPWARE_ADMIN_ES_ENABLED":            "0",
		"SHOPWARE_ADMIN_ES_REFRESH_INDICES":    "0",
		"OPENSEARCH_URL":                       "http://localhost:9200",
		"MESSENGER_TRANSPORT_DSN":              "doctrine://default?auto_setup=1",
		"MESSENGER_TRANSPORT_LOW_PRIORITY_DSN": "doctrine://default?auto_setup=1&queue_name=low_priority",
		"MESSENGER_TRANSPORT_FAILURE_DSN":      "doctrine://default?queue_name=failed&auto_setup=1",
		"BLUE_GREEN_DEPLOYMENT":                "0",
		"COMPOSER_HOME":                        "/var/www/.composer",
		// install.lock is not copied; this skips the web installer redirect
		"SHOPWARE_SKIP_WEBINSTALLER": "1",
	}
	known := map[string]bool{}
	for k := range env {
		known[k] = true
	}
	for _, k := range p.EnvKeys {
		if known[k] || containerEnvKeys[k] {
			continue
		}
		switch {
		case strings.HasPrefix(k, "REDIS") && strings.HasSuffix(k, "HOST"):
			env[k] = "redis"
		case strings.HasPrefix(k, "REDIS") && strings.HasSuffix(k, "PORT"):
			env[k] = "6379"
		case strings.HasPrefix(k, "REDIS") && strings.Contains(k, "PASS"):
			env[k] = ""
		case strings.HasPrefix(k, "REDIS") && (strings.Contains(k, "URL") || strings.Contains(k, "DSN")):
			// REDIS_URL, REDIS_DSN_CACHE, REDIS_DSN_CACHE_HTTP, REDIS_URL_SESSION
			env[k] = "redis://redis:6379"
		case redisSchemes[p.EnvSchemes[k]]:
			// CART_DSN, SESSION_DSN, …: secret-looking keys whose server value is a redis URL
			env[k] = "redis://redis:6379"
		default:
			if v, ok := p.SafeEnv[k]; ok {
				env[k] = v
			} else {
				env[k] = ""
				unset = append(unset, k)
			}
		}
	}
	for k, v := range overrides {
		if containerEnvKeys[k] {
			reserved = append(reserved, k)
			continue
		}
		env[k] = v
		unset = remove(unset, k)
	}
	sort.Strings(unset)
	sort.Strings(reserved)
	return env, unset, reserved
}

func remove(list []string, v string) []string {
	out := list[:0]
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// redisSchemes are the URL schemes Shopware's RedisConnectionFactory accepts.
var redisSchemes = map[string]bool{"redis": true, "rediss": true, "valkey": true, "valkeys": true}

// NeedsRedis reports whether the remote config uses Redis: a REDIS* key or a
// DSN/URL key whose server value has a redis scheme.
func NeedsRedis(p *Probe) bool {
	for _, k := range p.EnvKeys {
		if strings.HasPrefix(k, "REDIS") {
			return true
		}
	}
	for _, s := range p.EnvSchemes {
		if redisSchemes[s] {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func dotenv(env map[string]string) string {
	var b strings.Builder
	b.WriteString("# generated by dockware clone, local values only\n")
	for _, k := range sortedKeys(env) {
		fmt.Fprintf(&b, "%s=%s\n", k, strconv.Quote(env[k]))
	}
	return b.String()
}

// apacheEnv passes the environment to PHP-FPM (which clears the process env).
func apacheEnv(env map[string]string) string {
	var b strings.Builder
	for _, k := range sortedKeys(env) {
		fmt.Fprintf(&b, "SetEnv %s %s\n", k, strconv.Quote(env[k]))
	}
	return b.String()
}

// phpCloneINI raises the image's OPcache limits for a shop checkout: its
// 10000 accelerated files are below the ~23k vendor files plus the
// container cache, revalidate_freq 2 keeps code edits visible within 2 s,
// and the realpath cache spares stat calls on the bind mount.
const phpCloneINI = `opcache.memory_consumption=512
opcache.interned_strings_buffer=64
opcache.max_accelerated_files=100000
opcache.revalidate_freq=2
realpath_cache_size=4096K
realpath_cache_ttl=600
`

// WriteFiles writes compose file, env files, Apache env include and php ini.
func (l *Local) WriteFiles() error {
	if err := writeFile(filepath.Join(l.Dir, "shop.env"), []byte(dockerEnvFile(l.Env)), 0o600); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(l.Dir, "apache-env.conf"), []byte(apacheEnv(l.Env)), 0o644); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(l.Dir, "php-clone.ini"), []byte(phpCloneINI), 0o644); err != nil {
		return err
	}
	if l.MediaURL != "" {
		if err := writeFile(filepath.Join(l.Dir, "apache-media.conf"), []byte(mediaFallback(l.MediaURL)), 0o644); err != nil {
			return err
		}
	}
	if l.OwnsCode {
		if err := writeFile(filepath.Join(l.CodeDir, ".env"), []byte(dotenv(l.Env)), 0o600); err != nil {
			return err
		}
	}
	compose, err := l.Compose()
	if err != nil {
		return Fail(StepConfig, "rendering docker-compose.yml failed", err, "")
	}
	if l.Frontend != nil {
		// compose validates env_file of inactive profiles too
		if _, err := os.Stat(filepath.Join(l.Dir, "frontend.env")); os.IsNotExist(err) {
			if err := writeFile(filepath.Join(l.Dir, "frontend.env"), nil, 0o600); err != nil {
				return err
			}
		}
	}
	return writeFile(filepath.Join(l.Dir, "docker-compose.yml"), compose, 0o644)
}

// writeFile classifies a failed write of a generated file as a config error.
func writeFile(path string, data []byte, perm os.FileMode) error {
	if err := os.WriteFile(path, data, perm); err != nil {
		return Fail(StepConfig, "writing "+path+" failed", err, "")
	}
	return nil
}

func dockerEnvFile(env map[string]string) string {
	var b strings.Builder
	for _, k := range sortedKeys(env) {
		fmt.Fprintf(&b, "%s=%s\n", k, env[k])
	}
	return b.String()
}

func (l *Local) shopVolumes() []string {
	vol := func(name, path string) string { return name + ":" + containerRoot + path }
	v := []string{
		l.CodeDir + ":" + containerRoot,
		"./apache-env.conf:/etc/apache2/conf-enabled/zz-dockware-clone-env.conf:ro",
	}
	if l.MediaURL != "" {
		v = append(v, "./apache-media.conf:/etc/apache2/conf-enabled/zz-dockware-clone-media.conf:ro")
	}
	if l.PHPVersion != "" {
		// the ini dir is per PHP version, so without a known version the image defaults stay
		for _, sapi := range []string{"fpm", "cli"} {
			v = append(v, "./php-clone.ini:/etc/php/"+phpMajorMinor(l.PHPVersion)+"/"+sapi+"/conf.d/99-dockware-clone.ini:ro")
		}
	}
	v = append(v, vol("vendor", "/vendor"))
	if !l.OwnsCode {
		// a developer checkout: nothing Shopware writes may land in git (see SeedVar)
		v = append(v, vol("var", "/var"))
	}
	return append(v,
		vol("cache", "/var/cache"),
		vol("log", "/var/log"),
		vol("theme", "/public/theme"),
		vol("bundles", "/public/bundles"),
		"db:/var/lib/mysql",
	)
}

var mediaURLRe = regexp.MustCompile(`^https?://[^\s"'$%\\]+$`)

// ValidMediaURL reports whether url is safe to embed in the Apache rewrite.
func ValidMediaURL(url string) bool { return mediaURLRe.MatchString(url) }

// mediaFallback redirects media and thumbnail requests whose file does not
// exist locally to the source, so the clone needs no media copy while local
// uploads and an existing public/media in a checkout still win. Shopware's
// .htaccess enables its own rewrite engine, hence InheritDownBefore.
func mediaFallback(url string) string {
	return `<Directory "` + containerRoot + `/public">
    RewriteEngine On
    RewriteOptions InheritDownBefore
    RewriteCond %{REQUEST_FILENAME} !-f
    RewriteRule ^((?:media|thumbnail)/.+)$ ` + strings.TrimRight(url, "/") + `/$1 [R=302,L,NE]
</Directory>
`
}

// Compose renders docker-compose.yml with this volume layout:
// code bind-mounted, vendor/cache/log/theme/bundles/node_modules in volumes.
func (l *Local) Compose() ([]byte, error) {
	var environment []string
	if l.PHPVersion != "" {
		environment = append(environment, "PHP_VERSION="+phpMajorMinor(l.PHPVersion))
	}
	// NODE_VERSION stays unset: the entrypoint relinks node to it without
	// installing it, which breaks node when the image lacks that version (see UseNode)
	environment = append(environment, "XDEBUG_ENABLED=0")
	volumes := l.shopVolumes()
	if sock := hostSSHAgent(); sock != "" {
		// private VCS packages (git@github.com:…) need the developer's keys
		volumes = append(volumes, sock+":"+agentSocket)
		environment = append(environment, "SSH_AUTH_SOCK="+agentSocket)
	}
	shop := map[string]any{
		"image":          l.Image,
		"container_name": l.container(),
		"hostname":       "shop",
		"ports":          []string{fmt.Sprintf("%d:80", l.Port)},
		"env_file":       []string{"shop.env"},
		"environment":    environment,
		"volumes":        volumes,
	}
	services := map[string]any{"shop": shop}
	volumeDefs := map[string]any{"vendor": nil, "cache": nil, "log": nil, "theme": nil, "bundles": nil, "db": nil}
	if !l.OwnsCode {
		volumeDefs["var"] = nil
	}
	if l.Redis {
		services["redis"] = map[string]any{"image": "redis:7-alpine", "container_name": l.Name + "-redis"}
		shop["depends_on"] = []string{"redis"}
	}
	if f := l.Frontend; f != nil {
		app := "/app"
		cmd := f.Command
		if f.Install != "" {
			cmd = f.Install + " && " + f.Command
		}
		services["frontend"] = map[string]any{
			"image":          l.FrontendImage(),
			"container_name": l.Name + "-frontend",
			"working_dir":    app,
			"ports":          []string{fmt.Sprintf("%d:%d", l.FrontPort, f.Port)},
			"env_file":       []string{"frontend.env"},
			"command":        []string{"sh", "-c", cmd},
			"depends_on":     []string{"shop"},
			"profiles":       []string{"frontend"},
			// .output stays on the bind mount: `nuxt dev` removes that directory,
			// which fails with EBUSY on a volume mountpoint
			"volumes": []string{
				filepath.Join(l.CodeDir, f.Path) + ":" + app,
				"node_modules:" + app + "/node_modules",
				"nuxt:" + app + "/.nuxt",
			},
		}
		volumeDefs["node_modules"], volumeDefs["nuxt"] = nil, nil
	}
	return yaml.Marshal(map[string]any{"name": l.Name, "services": services, "volumes": volumeDefs})
}

// FrontendImage is the node image of the frontend service: the configured
// one, else the pinned node major (--node, node:), else the major the
// frontend (or the project) pins, else the shop's node, else the current
// LTS. "" without a frontend.
func (l *Local) FrontendImage() string {
	f := l.Frontend
	if f == nil {
		return ""
	}
	if f.Image != "" {
		return f.Image
	}
	if l.NodePinned && l.NodeVersion != "" {
		return "node:" + l.NodeVersion
	}
	if v := DetectNodeVersion(filepath.Join(l.CodeDir, f.Path), l.CodeDir); v != "" {
		return "node:" + v
	}
	if l.NodeVersion != "" {
		return "node:" + l.NodeVersion
	}
	return "node:lts"
}

// compose runs docker compose; its progress output is only shown on failure,
// classified under step (the shop or the frontend service).
func (l *Local) compose(ctx context.Context, step string, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose", "-f", filepath.Join(l.Dir, "docker-compose.yml")}, args...)...)
	cmd.Dir = l.Dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return Fail(step, "docker compose "+strings.Join(args, " ")+" failed", err, tailLines(string(out), 20))
	}
	return nil
}

// Exec runs a shell script in the shop container as www-data.
func (l *Local) Exec(ctx context.Context, script string, stdin io.Reader, stdout, stderr io.Writer) error {
	args := []string{"exec", "-i", "-w", containerRoot, l.container(), "bash", "-c", script}
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd.Run()
}

// execRoot runs a shell script in the shop container as root and returns
// its combined output; callers classify the failure.
func (l *Local) execRoot(ctx context.Context, script string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "exec", "-u", "root", l.container(), "bash", "-c", script).CombinedOutput()
	return string(out), err
}

// Console runs bin/console and returns combined output.
func (l *Local) Console(ctx context.Context, args ...string) (string, error) {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = ShellQuote(a)
	}
	var out bytes.Buffer
	err := l.Exec(ctx, "php -d memory_limit=-1 bin/console "+strings.Join(quoted, " "), nil, &out, &out)
	if err != nil {
		return out.String(), fmt.Errorf("bin/console %s: %w", strings.Join(args, " "), err)
	}
	return out.String(), nil
}

// SQL runs SQL in the local database and returns tab separated rows.
func (l *Local) SQL(ctx context.Context, sql string) (string, error) {
	var out, errb bytes.Buffer
	if err := l.Exec(ctx, localMySQL+" -N -B "+localDB, strings.NewReader(sql), &out, &errb); err != nil {
		return "", fmt.Errorf("local mysql: %w: %s", err, errb.String())
	}
	return out.String(), nil
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Up starts the shop container, waits for MySQL and Apache and checks that
// the image runs the PHP version the shop needs. A container that exits
// while starting (entrypoint failure) is reported at once with its logs.
func (l *Local) Up(ctx context.Context) error {
	if err := l.compose(ctx, StepDocker, "up", "-d", "shop"); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Minute)
	var logs []byte
	for time.Now().Before(deadline) {
		logs, _ = exec.CommandContext(ctx, "docker", "logs", "--tail", "40", l.container()).CombinedOutput()
		if bytes.Contains(logs, []byte("container IS READY")) {
			// fresh named volumes are root-owned
			out, err := l.execRoot(ctx, "chown www-data:www-data "+strings.Join([]string{
				containerRoot + "/vendor", containerRoot + "/var/cache", containerRoot + "/var/log",
				containerRoot + "/public/theme", containerRoot + "/public/bundles",
			}, " "))
			if err != nil {
				return Fail(StepDocker, "preparing the volumes of "+l.container()+" failed", err, tailLines(out, 20))
			}
			return l.checkPHP(ctx)
		}
		if status, code := l.containerState(ctx); status == "exited" || status == "dead" {
			e := &Error{
				Step:   StepDocker,
				Msg:    fmt.Sprintf("container %s exited with code %d during start", l.container(), code),
				Output: tailLines(string(logs), 20),
			}
			if e.Hint = hintFor(StepDocker, string(logs)); e.Hint == "" {
				e.Hint = "the entrypoint of " + l.Image + " failed: see `docker logs " + l.container() + "`"
			}
			return e
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return &Error{
		Step:   StepDocker,
		Msg:    "container " + l.container() + " did not become ready within 5 minutes",
		Hint:   "the container is still starting: check `docker logs " + l.container() + "`; the first start of an image is slow (nvm decompression, PHP switch), otherwise a wrong --image / --php is the usual cause",
		Output: tailLines(string(logs), 20),
	}
}

// containerState returns status and exit code of the shop container from
// docker inspect; a container that cannot be inspected reports "" and 0.
func (l *Local) containerState(ctx context.Context) (status string, exitCode int) {
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Status}} {{.State.ExitCode}}", l.container()).Output()
	if err != nil {
		return "", 0
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return "", 0
	}
	exitCode, _ = strconv.Atoi(fields[1])
	return fields[0], exitCode
}

// checkPHP fails when the image does not run the PHP version the shop needs.
func (l *Local) checkPHP(ctx context.Context) error {
	if l.PHPVersion == "" {
		return nil
	}
	want := phpMajorMinor(l.PHPVersion)
	got, err := l.phpVersion(ctx)
	if err != nil {
		return err
	}
	if got != want {
		return &Error{
			Step: StepDocker,
			Msg:  fmt.Sprintf("image %s does not provide PHP %s (got %s)", l.Image, want, got),
			Hint: "pass --image with an image that has PHP " + want + " or override with --php / php: in .dockware-clone.yml",
		}
	}
	return nil
}

// phpVersion reads major.minor of the container's php.
func (l *Local) phpVersion(ctx context.Context) (string, error) {
	var out, errb bytes.Buffer
	if err := l.Exec(ctx, `php -r 'echo PHP_MAJOR_VERSION.".".PHP_MINOR_VERSION;'`, nil, &out, &errb); err != nil {
		return "", Fail(StepDocker, "reading the PHP version of "+l.container()+" failed", err, tailLines(errb.String(), 20))
	}
	return strings.TrimSpace(out.String()), nil
}

// phpMajorMinor reduces "8.3.12" to "8.3", the granularity dockware switches.
func phpMajorMinor(v string) string {
	v = strings.TrimSpace(v)
	if parts := strings.SplitN(v, ".", 3); len(parts) > 2 {
		return parts[0] + "." + parts[1]
	}
	return v
}

// phpExtIgnored are profilers and debuggers: no code path requires them, so
// a clone does not install them. OPcache lists itself as a PHP module too.
var phpExtIgnored = map[string]bool{
	"tideways": true, "xdebug": true, "pcov": true, "blackfire": true, "newrelic": true,
	"opcache": true, "zend opcache": true, "ddtrace": true, "excimer": true,
}

// phpExtPackageAlias names the package part of extensions that do not come
// as php<ver>-<ext>: the pdo drivers ship with their database package.
var phpExtPackageAlias = map[string]string{
	"pdo_sqlite": "sqlite3", "pdo_pgsql": "pgsql", "pdo_mysql": "mysql", "mysqli": "mysql", "mysqlnd": "mysql",
}

// phpPackage is one apt package and the missing extensions it provides.
type phpPackage struct {
	Name string
	Exts []string
}

// parsePHPModules returns the modules of a `php -m` output lowercase and
// sorted: the [PHP Modules] section only, so Zend extensions listed under
// [Zend Modules] are left out. Output without the section yields nil.
func parsePHPModules(out string) []string {
	var mods []string
	in := false
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case l == "[PHP Modules]":
			in = true
		case strings.HasPrefix(l, "["):
			in = false
		case in && l != "":
			mods = append(mods, strings.ToLower(l))
		}
	}
	sort.Strings(mods)
	return mods
}

// phpExtPackages maps the extensions of want that have lacks to the apt
// packages providing them, php<ver>-<ext> with the pdo aliases, sorted by
// package name; one package may cover several extensions (pdo_sqlite and
// sqlite3 both come with php8.3-sqlite3). Profilers and debuggers are left out.
func phpExtPackages(ver string, want, have []string) []phpPackage {
	has := make(map[string]bool, len(have))
	for _, e := range have {
		has[e] = true
	}
	byPkg := map[string][]string{}
	for _, e := range want {
		if has[e] || phpExtIgnored[e] {
			continue
		}
		has[e] = true // once per extension
		name := e
		if alias, ok := phpExtPackageAlias[e]; ok {
			name = alias
		}
		pkg := "php" + ver + "-" + name
		byPkg[pkg] = append(byPkg[pkg], e)
	}
	pkgs := make([]phpPackage, 0, len(byPkg))
	for name, exts := range byPkg {
		sort.Strings(exts)
		pkgs = append(pkgs, phpPackage{Name: name, Exts: exts})
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].Name < pkgs[j].Name })
	return pkgs
}

// phpModules lists the extensions of the container's php.
func (l *Local) phpModules(ctx context.Context) ([]string, error) {
	var out, errb bytes.Buffer
	if err := l.Exec(ctx, "php -m", nil, &out, &errb); err != nil {
		return nil, Fail(StepDocker, "listing the PHP extensions of "+l.container()+" failed", err, tailLines(errb.String(), 20))
	}
	return parsePHPModules(out.String()), nil
}

// EnsurePHPExtensions installs the extensions of want (the server's php -m)
// that the container's php lacks from the image's apt sources
// (php<ver>-<ext>) and restarts php-fpm, so composer's platform check and
// code paths needing them (zstd cache compression, imagick, ...) work. It
// returns the extensions still missing afterwards: those without a package,
// or all of them when apt failed; the clone goes on and the caller warns. A
// broken docker exec is the only error. The packages live in the container
// filesystem and are gone once compose recreates the container.
func (l *Local) EnsurePHPExtensions(ctx context.Context, want []string, log io.Writer) ([]string, error) {
	if len(want) == 0 {
		return nil, nil
	}
	have, err := l.phpModules(ctx)
	if err != nil {
		return nil, err
	}
	ver := phpMajorMinor(l.PHPVersion)
	if ver == "" {
		if ver, err = l.phpVersion(ctx); err != nil {
			return nil, err
		}
	}
	pkgs := phpExtPackages(ver, want, have)
	if len(pkgs) == 0 {
		return nil, nil
	}
	names := make([]string, len(pkgs))
	var exts []string
	for i, p := range pkgs {
		names[i] = ShellQuote(p.Name)
		exts = append(exts, p.Exts...)
	}
	sort.Strings(exts)
	// the image ships without package lists; only packages with a candidate
	// are installed, so an extension without one does not fail the others
	out, err := l.execRoot(ctx, `export LC_ALL=C DEBIAN_FRONTEND=noninteractive
out=$(apt-get update -qq 2>&1) || { printf '%s\n' "$out"; exit 1; }
for p in `+strings.Join(names, " ")+`; do apt-cache policy "$p" 2>/dev/null | grep -q '^ *Candidate: [0-9]' && echo "$p"; done
true`)
	if err != nil {
		fmt.Fprintf(log, "  php extensions: apt-get update failed, %s stay missing:\n    %s\n", strings.Join(exts, ", "), strings.ReplaceAll(tailLines(out, 10), "\n", "\n    "))
		return exts, nil
	}
	candidate := map[string]bool{}
	for _, p := range strings.Fields(out) {
		candidate[p] = true
	}
	var install, noPackage []string
	for _, p := range pkgs {
		if candidate[p.Name] {
			install = append(install, p.Name)
		} else {
			noPackage = append(noPackage, p.Name)
		}
	}
	if len(noPackage) > 0 {
		fmt.Fprintf(log, "  php extensions: no apt package %s\n", strings.Join(noPackage, ", "))
	}
	if len(install) == 0 {
		return exts, nil
	}
	if out, err = l.execRoot(ctx, "export DEBIAN_FRONTEND=noninteractive\napt-get install -y -qq "+strings.Join(install, " ")+" 2>&1 || exit 1\nservice php"+ver+"-fpm restart >/dev/null 2>&1 || true"); err != nil {
		fmt.Fprintf(log, "  php extensions: apt-get install %s failed:\n    %s\n", strings.Join(install, " "), strings.ReplaceAll(tailLines(out, 10), "\n", "\n    "))
	}
	// what php reports now decides, not apt's exit status (partial installs)
	if have, err = l.phpModules(ctx); err != nil {
		return nil, err
	}
	var missing []string
	for _, p := range phpExtPackages(ver, exts, have) {
		missing = append(missing, p.Exts...)
	}
	sort.Strings(missing)
	var installed []string
	for _, e := range exts {
		if !containsString(missing, e) {
			installed = append(installed, e)
		}
	}
	if len(installed) > 0 {
		fmt.Fprintf(log, "  php extensions: installed %s\n", strings.Join(installed, ", "))
	}
	return missing, nil
}

// Tuning for the local MySQL: a throwaway dev database, so durability is
// traded for import and indexer speed; a 4 GiB buffer pool fits Docker
// Desktop's default memory limit, smaller VMs get a quarter of their RAM.
const (
	mysqlBufferPoolMax   int64 = 4 << 30
	mysqlBufferPoolChunk int64 = 128 << 20 // innodb resizes the pool in chunks of this size
	mysqlRedoLogBytes    int64 = 2 << 30
	mysqlFlushAtCommit         = 2
)

// bufferPoolBytes returns the innodb_buffer_pool_size for a container with
// memTotalKB of RAM: a quarter of it, at most mysqlBufferPoolMax, rounded
// down to a whole chunk.
func bufferPoolBytes(memTotalKB int64) int64 {
	pool := memTotalKB * 1024 / 4
	if pool > mysqlBufferPoolMax {
		pool = mysqlBufferPoolMax
	}
	return pool - pool%mysqlBufferPoolChunk
}

// tuneMySQL persists the tuning in the db volume: SET PERSIST writes
// mysqld-auto.cnf into the datadir, so it also covers later indexer runs and
// a recreated container. Every statement is sent on its own, so a variable
// this server lacks (innodb_redo_log_capacity before MySQL 8.0.30, MariaDB)
// skips only itself. Failures are logged, never fatal.
func (l *Local) tuneMySQL(ctx context.Context, log io.Writer) {
	stmts := []string{
		fmt.Sprintf("SET PERSIST innodb_flush_log_at_trx_commit = %d", mysqlFlushAtCommit),
		fmt.Sprintf("SET PERSIST innodb_redo_log_capacity = %d", mysqlRedoLogBytes),
	}
	var out bytes.Buffer
	skipped := func(what string) {
		fmt.Fprintf(log, "  mysql tuning skipped: %s: %s\n", what, tailLines(out.String(), 1))
		out.Reset()
	}
	// the pool is sized from the container's RAM and never shrunk
	if err := l.Exec(ctx, "awk '/^MemTotal:/{print $2}' /proc/meminfo && "+localMySQL+" -N -B -e 'SELECT @@innodb_buffer_pool_size'", nil, &out, &out); err != nil {
		skipped("innodb_buffer_pool_size")
	} else if fields := strings.Fields(out.String()); len(fields) != 2 {
		skipped("innodb_buffer_pool_size: expected MemTotal and the current size")
	} else {
		memKB, _ := strconv.ParseInt(fields[0], 10, 64)
		current, _ := strconv.ParseInt(fields[1], 10, 64)
		if pool := bufferPoolBytes(memKB); pool > current {
			stmts = append(stmts, fmt.Sprintf("SET PERSIST innodb_buffer_pool_size = %d", pool))
		}
		out.Reset()
	}
	for _, stmt := range stmts {
		if err := l.Exec(ctx, localMySQL+" -e "+ShellQuote(stmt), nil, io.Discard, &out); err != nil {
			skipped(stmt)
		}
	}
}

// Parallel import: the dump is split in the container into DDL, data parts
// and a trailer. Per-table files loaded in parallel deadlock on DROP/CREATE
// of tables with foreign keys (ERROR 1213); all DDL first, then the data,
// does not. 128 MiB parts balance 8 loaders; more loaders than CPUs
// (nproc) are never started. importDir prefixes the per-run work dir
// (mktemp) and the pid file of the running import script.
const (
	importWorkers         = 8
	importPartBytes int64 = 128 << 20
	importDir             = "/tmp/dockware-clone-import"
)

// importSplitter is the awk program (-v dir=… -v max=…) that splits a
// shopware-cli dump: everything outside data sections goes to schema.sql in
// stream order; the INSERTs of a `-- Data for table` section go to numbered
// *.part.sql files that break only between statements, close at about max
// bytes and never share a table; views and triggers (shopware-cli writes
// them after the last table) and whatever else follows a table's data (the
// FOREIGN_KEY_CHECKS = 1 trailer) go to post.sql, which runs last, so no
// trigger fires during the parallel row load. LOCK/UNLOCK TABLES and the
// MyISAM key hints are dropped, they only serialise. Every output file
// starts with the dump's own session settings. Must stay POSIX awk: the
// image may ship mawk.
const importSplitter = `
function closepart() { if (part != "") close(part); part = ""; size = 0 }
function newpart() { closepart(); n++; part = sprintf("%s/%05d.part.sql", dir, n); printf "%s", pre > part; size = length(pre) }
function topost() { if (!postopen) { printf "%s", pre > post; postopen = 1 }; mode = "post" }
BEGIN {
	pre = "SET NAMES utf8mb4;\nSET FOREIGN_KEY_CHECKS = 0;\nSET UNIQUE_CHECKS = 0;\n"
	schema = dir "/schema.sql"
	post = dir "/post.sql"
	printf "%s", pre > schema
	mode = "schema"
}
/^-- (Trigger|Structure for view) / { closepart(); topost(); print > post; next }
/^-- Structure for / { closepart(); mode = "schema"; print > schema; next }
/^-- Data for table / { closepart(); mode = "data"; stmt = 0; next }
mode == "schema" { print > schema; next }
mode == "post" { print > post; next }
stmt { print > part; size += length($0) + 1; if ($0 ~ /;[ \t\r]*$/) stmt = 0; next }
/^INSERT INTO / { if (part == "" || size >= max) newpart(); print > part; size += length($0) + 1; if ($0 !~ /;[ \t\r]*$/) stmt = 1; next }
/^(LOCK|UNLOCK) TABLES/ || /^\/\*!40000 ALTER TABLE / || /^--/ || /^[ \t\r]*$/ { next }
{ topost(); print > post }
END { closepart() }
`

// importScript builds the container-side import that reads the compressed
// dump on stdin: split it, load schema.sql serially, the parts with up to
// workers mysql clients largest first (ls -S), then post.sql. The work dir
// is per run and removed on every exit; a loaded part is deleted at once to
// bound the disk use. The script's pid is kept in the pid file so
// killImportScript can stop it after the docker exec client is gone.
// DEFINER clauses are stripped because the remote MySQL user does not exist
// locally; no import session writes a binary log.
func importScript(decompress string, workers int, partBytes int64) string {
	load := localMySQL + ` --max-allowed-packet=512M --init-command="SET SESSION sql_log_bin=0" ` + localDB
	w := strconv.Itoa(workers)
	return `set -o pipefail
dir=$(mktemp -d ` + importDir + `.XXXXXX) || exit 1
echo $$ > ` + importDir + `.pid
trap 'rm -rf "$dir" ` + importDir + `.pid' EXIT
LC_ALL=C ` + decompress + ` | LC_ALL=C sed -E 's/DEFINER=` + "`[^`]+`@`[^`]+`" + `//g' | LC_ALL=C awk -v dir="$dir" -v max=` + strconv.FormatInt(partBytes, 10) + ` ` + ShellQuote(importSplitter) + ` || exit 1
` + load + ` < "$dir/schema.sql" || exit 1
workers=$(nproc 2>/dev/null || echo 1)
if [ "$workers" -gt ` + w + ` ]; then workers=` + w + `; fi
parts=$(ls -S "$dir"/*.part.sql 2>/dev/null)
if [ -n "$parts" ]; then
	printf '%s\n' "$parts" | xargs -P "$workers" -n 1 bash -c ` + ShellQuote(load+` < "$1" && rm -f -- "$1" || { echo "loading $1 failed" >&2; exit 1; }`) + ` _ || exit 1
fi
if [ -s "$dir/post.sql" ]; then ` + load + ` < "$dir/post.sql" || exit 1; fi
`
}

// killImportScript stops an import script whose docker exec client died
// (Ctrl+C ends the client, not the container process; the xargs loaders
// would go on for minutes and collide with a rerun). Each process is
// stopped before its children are listed, so xargs cannot start another
// loader meanwhile, and killed after them. The pid file names the script;
// its cmdline is checked so a pid reused after a container restart is left
// alone. Prints a line when something was stopped. The pid file and every
// work dir are removed either way: a killed script runs no EXIT trap.
const killImportScript = `
killtree() {
	local c
	kill -STOP "$1" 2>/dev/null
	for c in $(pgrep -P "$1" 2>/dev/null || ps -o pid= --ppid "$1" 2>/dev/null); do killtree "$c"; done
	kill -KILL "$1" 2>/dev/null
}
pid=$(cat ` + importDir + `.pid 2>/dev/null)
if [ -n "$pid" ] && [ "$pid" != "$$" ] && grep -qa dockware-clone-import "/proc/$pid/cmdline" 2>/dev/null; then
	killtree "$pid"
	echo "stopped a leftover import (pid $pid)"
fi
rm -rf ` + importDir + `.*
`

// Import loads the dump into a fresh local database in two phases: all DDL
// serially (seconds), then the table data in parts loaded by up to
// importWorkers mysql clients at once with foreign_key_checks and
// unique_checks off, as the dump's own header has them. The InnoDB redo log
// is disabled for the data phase where the server allows it (MySQL ≥
// 8.0.21 as root): the database is a throwaway and the import runs three
// times faster without it. A disabled redo log loses the whole instance on
// a crash, so it is re-enabled on every path. Loaders a cancelled run left
// in the container are stopped before the database is dropped and when
// this run's import exec fails.
func (l *Local) Import(ctx context.Context, dump *DumpResult, log io.Writer) error {
	l.tuneMySQL(ctx, log)
	l.killImport(ctx, log)
	var out bytes.Buffer
	if err := l.Exec(ctx, localMySQL+" -e 'DROP DATABASE IF EXISTS "+localDB+"; CREATE DATABASE "+localDB+" CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci'", nil, io.Discard, &out); err != nil {
		return Fail(StepImport, "creating the local database failed", err, tailLines(out.String(), 20))
	}
	f, err := os.Open(dump.File)
	if err != nil {
		return Fail(StepImport, "cannot open the dump", err, "")
	}
	defer f.Close()
	decompress := "zstd -dc"
	if dump.Compression == "gzip" {
		decompress = "gzip -dc"
	}
	if err := l.Exec(ctx, localMySQL+" -e 'ALTER INSTANCE DISABLE INNODB REDO_LOG'", nil, io.Discard, io.Discard); err == nil {
		fmt.Fprintln(log, "  import: redo log disabled during import")
		defer l.enableRedoLog(log)
	} else {
		// MariaDB, MySQL before 8.0.21 or a root without INNODB_REDO_LOG_ENABLE
		fmt.Fprintln(log, "  import: redo log kept (not supported)")
	}
	out.Reset()
	if err := l.Exec(ctx, importScript(decompress, importWorkers, importPartBytes), f, &out, &out); err != nil {
		// before the deferred enableRedoLog, with its own context: ctx may
		// be the reason the exec client died while the loaders live on
		bg, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		l.killImport(bg, log)
		cancel()
		return Fail(StepImport, "importing the dump into the local database failed", err, tailLines(out.String(), 20))
	}
	return nil
}

// killImport runs killImportScript in the container and logs what it
// stopped; a failed exec is logged, the next step reports a broken docker.
func (l *Local) killImport(ctx context.Context, log io.Writer) {
	var out bytes.Buffer
	if err := l.Exec(ctx, killImportScript, nil, &out, io.Discard); err != nil {
		fmt.Fprintf(log, "  import: stopping a leftover import failed: %v\n", err)
		return
	}
	if s := strings.TrimSpace(out.String()); s != "" {
		fmt.Fprintf(log, "  import: %s\n", s)
	}
}

// enableRedoLog turns the redo log back on after Import with its own
// context: a cancelled run must not leave the instance crash-unsafe.
func (l *Local) enableRedoLog(log io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var out bytes.Buffer
	if err := l.Exec(ctx, localMySQL+" -e 'ALTER INSTANCE ENABLE INNODB REDO_LOG'", nil, io.Discard, &out); err != nil {
		fmt.Fprintf(log, "  import: re-enabling the redo log failed: %s\n", tailLines(out.String(), 1))
	}
}

// DomainMap rewrites every remote domain to a local URL, keeping paths.
// The APP_URL host maps to localhost, other hosts to <slug>.localhost.
// Local URLs drop the scheme, so when http and https variants of a domain
// both exist only the first one (in slice order) is mapped; the other keeps
// its remote URL, since sales_channel_domain.url is unique.
func DomainMap(domains []Domain, appURL string, port int) map[string]string {
	primary := ""
	if u, err := url.Parse(appURL); err == nil {
		primary = u.Host
	}
	out := map[string]string{}
	taken := map[string]bool{}
	for _, d := range domains {
		if d.URL == "" {
			continue
		}
		u, err := url.Parse(d.URL)
		if err != nil || u.Host == "" {
			// headless placeholder domains like "default.headless0" stay as they are
			continue
		}
		host := "localhost"
		if u.Host != primary {
			host = strings.NewReplacer(".", "-", ":", "-").Replace(u.Host) + ".localhost"
		}
		local := fmt.Sprintf("http://%s:%d%s", host, port, strings.TrimRight(u.Path, "/"))
		if taken[local] {
			continue
		}
		taken[local] = true
		out[d.URL] = local
	}
	return out
}

func sqlString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// PostImport makes the shop runnable locally: domains, admin user, staging
// mode, plugin and theme state, caches.
func (l *Local) PostImport(ctx context.Context, p *Probe, domains map[string]string, postSQL string, log io.Writer) error {
	var sql strings.Builder
	for _, from := range sortedKeys(domains) {
		fmt.Fprintf(&sql, "UPDATE sales_channel_domain SET url = %s WHERE url = %s;\n", sqlString(domains[from]), sqlString(from))
	}
	if _, err := l.SQL(ctx, sql.String()); err != nil {
		return Fail(StepPostImport, "rewriting the sales channel domains failed", err, "")
	}
	if postSQL != "" {
		data, err := os.ReadFile(postSQL)
		if err != nil {
			return Fail(StepPostImport, "cannot read post_sql "+postSQL, err, "")
		}
		if _, err := l.SQL(ctx, string(data)); err != nil {
			return Fail(StepPostImport, "post_sql "+postSQL+" failed", err, "")
		}
	}
	steps := [][]string{{"plugin:refresh"}}
	list, _ := l.Console(ctx, "list", "system", "--raw")
	if strings.Contains(list, "system:setup:staging") {
		steps = append(steps, []string{"system:setup:staging", "--force", "-n"})
	} else if _, err := l.SQL(ctx, "DELETE FROM system_config WHERE configuration_key = 'core.mailerSettings.disableDelivery' AND sales_channel_id IS NULL;\nINSERT INTO system_config (id, configuration_key, configuration_value, created_at) VALUES (UNHEX(REPLACE(UUID(),'-','')), 'core.mailerSettings.disableDelivery', '{\"_value\": true}', NOW(3));"); err != nil {
		return Fail(StepPostImport, "disabling mail delivery failed", err, "")
	}
	steps = append(steps,
		[]string{"user:create", "admin", "--admin", "--password=shopware", "--email=admin@example.com", "--firstName=Admin", "--lastName=Local", "-n"},
		[]string{"assets:install"},
	)
	if p.HasStorefront {
		steps = append(steps, []string{"theme:compile", "--active-only"})
	}
	steps = append(steps, []string{"cache:clear"})
	for _, s := range steps {
		fmt.Fprintf(log, "  bin/console %s\n", strings.Join(s, " "))
		if out, err := l.Console(ctx, s...); err != nil {
			if s[0] == "user:create" && strings.Contains(out, "already") {
				continue
			}
			if s[0] == "theme:compile" && strings.Contains(out, "active-only") {
				if _, err := l.Console(ctx, "theme:compile"); err == nil {
					continue
				}
			}
			return Fail(StepPostImport, "bin/console "+strings.Join(s, " ")+" failed", err, tailLines(out, 25))
		}
	}
	return nil
}

// copyInto streams hostDir into dir (relative to the project root) of the
// shop container, skipping the given top-level entries.
func (l *Local) copyInto(ctx context.Context, hostDir, dir string, log io.Writer, exclude ...string) error {
	args := []string{"-C", hostDir, "--no-xattrs", "--exclude", ".DS_Store"}
	for _, e := range exclude {
		args = append(args, "--exclude", "./"+e)
	}
	tar := exec.CommandContext(ctx, "tar", append(args, "-cf", "-", ".")...)
	tar.Env = append(os.Environ(), "COPYFILE_DISABLE=1") // no macOS ._ files
	pipe, err := tar.StdoutPipe()
	if err != nil {
		return Fail(StepDocker, "cannot start tar for "+hostDir, err, "")
	}
	if err := tar.Start(); err != nil {
		return Fail(StepDocker, "cannot start tar for "+hostDir, err, "")
	}
	var errb bytes.Buffer
	if err := l.Exec(ctx, "mkdir -p "+ShellQuote(dir)+" && tar -C "+ShellQuote(dir)+" -xf -", pipe, log, &errb); err != nil {
		_ = pipe.Close()
		_ = tar.Wait()
		return Fail(StepDocker, "copying "+hostDir+" into "+l.container()+":"+dir+" failed", err, tailLines(errb.String(), 20))
	}
	if err := tar.Wait(); err != nil {
		return Fail(StepDocker, "reading "+hostDir+" failed", err, "")
	}
	return nil
}

// SeedVar copies the checkout's var/ into the var volume. Shopware writes
// theme and plugin state there, which projects partly track in git; the
// volume keeps those writes out of the developer's working tree.
func (l *Local) SeedVar(ctx context.Context, log io.Writer) error {
	if l.OwnsCode {
		return nil
	}
	// a fresh named volume is root-owned; the shop runs as www-data
	if out, err := l.execRoot(ctx, "chown www-data:www-data "+containerRoot+"/var"); err != nil {
		return Fail(StepDocker, "preparing the var volume of "+l.container()+" failed", err, tailLines(out, 20))
	}
	hostVar := filepath.Join(l.CodeDir, "var")
	if _, err := os.Stat(hostVar); err != nil {
		return nil
	}
	return l.copyInto(ctx, hostVar, "var", log, "cache", "log")
}

// ComposerAuth is the merged content of composer's credential sources and
// where it came from.
type ComposerAuth struct {
	JSON   []byte
	Source string // sources used, highest precedence first, comma separated; "" when nothing was found
}

type composerAuthFile struct{ path, label string }

// composerAuthFiles lists the auth.json files in precedence order: the
// checkout, then composer's home directories.
func composerAuthFiles(codeDir string) []composerAuthFile {
	var files []composerAuthFile
	if codeDir != "" {
		p := filepath.Join(codeDir, "auth.json")
		files = append(files, composerAuthFile{p, p})
	}
	if home := os.Getenv("COMPOSER_HOME"); home != "" {
		files = append(files, composerAuthFile{filepath.Join(home, "auth.json"), "$COMPOSER_HOME/auth.json"})
	}
	if home, err := os.UserHomeDir(); err == nil {
		files = append(files,
			composerAuthFile{filepath.Join(home, ".composer", "auth.json"), "~/.composer/auth.json"},
			composerAuthFile{filepath.Join(home, ".config", "composer", "auth.json"), "~/.config/composer/auth.json"},
		)
	}
	return files
}

// composerAuthLabels names every place credentials are looked for, highest
// precedence first.
func composerAuthLabels(codeDir string) []string {
	labels := []string{"--composer-auth", "COMPOSER_AUTH env"}
	for _, f := range composerAuthFiles(codeDir) {
		labels = append(labels, f.label)
	}
	return labels
}

// authObject parses the shape of composer's auth.json, a JSON object of
// sections; nil for anything else.
func authObject(data []byte) map[string]json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil {
		return nil
	}
	return obj
}

// mergeAuth adds what dst lacks from src: whole sections (http-basic,
// bearer, github-oauth, …) and, within a section both have, hosts. dst
// has the higher precedence, as with composer's global and project auth.json.
func mergeAuth(dst, src map[string]json.RawMessage) {
	for section, raw := range src {
		have, ok := dst[section]
		if !ok {
			dst[section] = raw
			continue
		}
		var hosts, more map[string]json.RawMessage
		if json.Unmarshal(have, &hosts) != nil || hosts == nil || json.Unmarshal(raw, &more) != nil {
			continue // not keyed by host on both sides: the higher precedence value stays
		}
		for host, v := range more {
			if _, ok := hosts[host]; !ok {
				hosts[host] = v
			}
		}
		if merged, err := authJSON(hosts); err == nil {
			dst[section] = merged
		}
	}
}

// authJSON encodes credentials without HTML escaping, so passwords keep
// their characters as composer wrote them.
func authJSON(v map[string]json.RawMessage) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

// FindComposerAuth merges composer's credential sources the way composer
// does, highest precedence first: the --composer-auth file, the COMPOSER_AUTH
// variable, auth.json in the checkout, $COMPOSER_HOME/auth.json,
// ~/.composer/auth.json and ~/.config/composer/auth.json. A section or host
// set by a higher source wins, the rest is added. Nothing is read from the
// server. Errors never contain file contents.
func FindComposerAuth(flagPath, codeDir string) (ComposerAuth, error) {
	var merged map[string]json.RawMessage
	var sources []string
	add := func(label string, data []byte, hint string) error {
		obj := authObject(data)
		if obj == nil {
			return &Error{Step: StepConfig, Msg: label + " is not a JSON object", Hint: hint}
		}
		if merged == nil {
			merged = obj
		} else {
			mergeAuth(merged, obj)
		}
		sources = append(sources, label)
		return nil
	}
	if flagPath != "" {
		data, err := os.ReadFile(flagPath)
		if err != nil {
			return ComposerAuth{}, &Error{Step: StepConfig, Msg: "cannot read --composer-auth " + flagPath, Err: err,
				Hint: "point --composer-auth at a readable composer auth.json, or drop the flag: the checkout's auth.json, $COMPOSER_HOME/auth.json, ~/.composer/auth.json, ~/.config/composer/auth.json and COMPOSER_AUTH are read anyway"}
		}
		if err := add("--composer-auth "+flagPath, data, "composer's auth.json is a JSON object such as {\"http-basic\": {\"packages.shopware.com\": {\"username\": …, \"password\": …}}}"); err != nil {
			return ComposerAuth{}, err
		}
	}
	if v := strings.TrimSpace(os.Getenv("COMPOSER_AUTH")); v != "" {
		if err := add("COMPOSER_AUTH env", []byte(v), "unset COMPOSER_AUTH or set it to the content of a composer auth.json"); err != nil {
			return ComposerAuth{}, err
		}
	}
	for _, f := range composerAuthFiles(codeDir) {
		data, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		if err := add(f.label, data, "fix or remove that auth.json, or point --composer-auth at a valid one"); err != nil {
			return ComposerAuth{}, err
		}
	}
	if merged == nil {
		return ComposerAuth{}, nil
	}
	data, err := authJSON(merged)
	if err != nil {
		return ComposerAuth{}, &Error{Step: StepConfig, Msg: "cannot merge composer credentials from " + strings.Join(sources, ", "), Err: err}
	}
	return ComposerAuth{JSON: data, Source: strings.Join(sources, ", ")}, nil
}

// InstallVendor runs composer install inside the container. The credentials
// reach composer through the process environment (docker exec -e NAME), so
// they never appear on a command line or in the output.
func (l *Local) InstallVendor(ctx context.Context, auth ComposerAuth, log io.Writer) error {
	args := []string{"exec", "-i", "-w", containerRoot}
	env := os.Environ()
	if auth.JSON != nil {
		args = append(args, "-e", "COMPOSER_AUTH")
		env = append(env, "COMPOSER_AUTH="+string(auth.JSON))
	}
	if hostSSHAgent() != "" {
		args = append(args, "-e", "GIT_SSH_COMMAND=ssh -o StrictHostKeyChecking=accept-new")
	}
	args = append(args, l.container(), "bash", "-c", "composer install --no-interaction --no-scripts --prefer-dist --no-progress")
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = env
	var out bytes.Buffer
	w := io.MultiWriter(log, &out)
	cmd.Stdout, cmd.Stderr = w, w
	err := cmd.Run()
	if err == nil {
		return nil
	}
	failed := Fail(StepComposer, "composer install failed", err, tailLines(out.String(), 25))
	var e *Error
	if auth.Source == "" && errors.As(failed, &e) && (e.Hint == "" || e.Hint == hintFor(StepComposer, "Authentication required")) {
		e.Msg = "composer install failed and no composer credentials were found (looked for " + strings.Join(composerAuthLabels(l.CodeDir), ", ") + ")"
	}
	return failed
}

// nvmDir is where the dockware image keeps www-data's node versions.
const nvmDir = "/var/www/.nvm"

// UseNode makes NodeVersion the default node of the shop container the way
// the dockware entrypoint does for NODE_VERSION, but installs the version
// first when the image lacks it. It returns the effective `node -v`; when
// no version was requested and the image has no node, that is "" with a
// warning on log (composer runs without scripts, so the clone goes on).
// Tool output stays out of log: a failure carries its tail in the error.
func (l *Local) UseNode(ctx context.Context, log io.Writer) (string, error) {
	major := l.NodeVersion
	var out bytes.Buffer
	if major != "" {
		if !configNodeVersion.MatchString(major) {
			return "", &Error{Step: StepConfig, Msg: "node version must be a major number such as 20, got " + strconv.Quote(major)}
		}
		nvm := "set -e\nexport NVM_DIR=" + nvmDir + "\n. \"$NVM_DIR/nvm.sh\" --no-use\n"
		if err := l.Exec(ctx, nvm+"nvm ls "+major+" >/dev/null 2>&1 || nvm install --no-progress "+major+"\nnvm alias default "+major, nil, &out, &out); err != nil {
			return "", l.nodeError(major, err, out.String())
		}
		// nvm is per user; like the entrypoint, link the default for root and non-login shells
		rootOut, err := l.execRoot(ctx, nvm+"bin=$(nvm which default)\nln -sfn \"$bin\" /usr/local/bin/node\nln -sfn \"$(dirname \"$bin\")/npm\" /usr/local/bin/npm")
		if err != nil {
			return "", l.nodeError(major, err, rootOut)
		}
		out.Reset()
	}
	if err := l.Exec(ctx, "node -v", nil, &out, &out); err != nil {
		if major == "" {
			fmt.Fprintf(log, "  warning: node is not available in %s; builds that need node will fail\n", l.container())
			return "", nil
		}
		return "", l.nodeError(major, err, out.String())
	}
	return strings.TrimSpace(out.String()), nil
}

func (l *Local) nodeError(major string, err error, out string) error {
	e := &Error{Step: StepDocker, Msg: "switching to node " + major + " failed", Output: tailLines(out, 25), Err: err}
	if e.Hint = hintFor(StepDocker, out); e.Hint == "" {
		e.Hint = "nvm could not provide node " + major + " in " + l.container() + ": check Docker's network access, or pass --node <major> / node: in .dockware-clone.yml with a version the image ships"
	}
	return e
}

// DockerAvailable fails when the docker CLI is missing or no daemon answers.
func DockerAvailable(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput()
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return &Error{Step: StepDocker, Msg: "docker is not installed (no docker binary in PATH)", Hint: "install Docker Desktop and retry", Err: err}
	}
	return Fail(StepDocker, "Docker is not available", err, tailLines(string(out), 10))
}

const agentSocket = "/run/ssh-agent.sock"

// hostSSHAgent returns the host path of the ssh-agent socket that can be
// bind-mounted into a container, or "" when no agent is running.
func hostSSHAgent() string {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return ""
	}
	if runtime.GOOS == "darwin" {
		// Docker Desktop cannot mount launchd sockets; it proxies the agent here
		return "/run/host-services/ssh-auth.sock"
	}
	return sock
}

// AccessKeys lists sales channel access keys from the local (cloned) database.
func (l *Local) AccessKeys(ctx context.Context) (map[string]string, []string, error) {
	out, err := l.SQL(ctx, "SELECT IFNULL(t.name, LOWER(HEX(sc.id))), sc.access_key, LOWER(HEX(sc.type_id)) FROM sales_channel sc LEFT JOIN sales_channel_translation t ON t.sales_channel_id = sc.id AND t.language_id = sc.language_id WHERE sc.active = 1 ORDER BY sc.created_at")
	if err != nil {
		return nil, nil, Fail(StepPostImport, "reading the sales channel access keys failed", err, "")
	}
	keys := map[string]string{}
	var order []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(l, "\t")
		if len(f) < 3 || (f[2] != SalesChannelTypeStorefront && f[2] != SalesChannelTypeAPI) {
			continue
		}
		name := f[0]
		if _, dup := keys[name]; dup {
			name = fmt.Sprintf("%s #%d", f[0], len(order)+1)
		}
		keys[name] = f[1]
		order = append(order, name)
	}
	return keys, order, nil
}

var placeholderRe = regexp.MustCompile(`\$\{(SHOP_URL|SHOP_URL_INTERNAL|FRONTEND_URL|ACCESS_KEY)(?::([^}]+))?\}`)

// FrontendEnv resolves placeholders in the frontend env definition.
func (l *Local) FrontendEnv(keys map[string]string, order []string) (map[string]string, error) {
	env := map[string]string{}
	var missing error
	for k, v := range l.Frontend.Env {
		env[k] = placeholderRe.ReplaceAllStringFunc(v, func(m string) string {
			sub := placeholderRe.FindStringSubmatch(m)
			switch sub[1] {
			case "SHOP_URL":
				return l.ShopURL()
			case "SHOP_URL_INTERNAL":
				return "http://shop"
			case "FRONTEND_URL":
				return "http://localhost:" + strconv.Itoa(l.FrontPort)
			}
			if sub[2] != "" {
				if key, ok := keys[sub[2]]; ok {
					return key
				}
				missing = &Error{Step: StepFrontend, Msg: fmt.Sprintf("frontend env %s: no active sales channel named %q", k, sub[2]),
					Hint: "${ACCESS_KEY:<name>} must name an active storefront or headless sales channel of the cloned shop (frontend.env in .dockware-clone.yml)"}
				return ""
			}
			if len(order) == 0 {
				missing = &Error{Step: StepFrontend, Msg: "frontend env " + k + ": no storefront or headless sales channel",
					Hint: "the cloned shop has no active storefront or headless sales channel to take ${ACCESS_KEY} from"}
				return ""
			}
			return keys[order[0]]
		})
	}
	return env, missing
}

// StartFrontend writes frontend.env and starts the frontend service.
func (l *Local) StartFrontend(ctx context.Context, env map[string]string) error {
	if err := os.WriteFile(filepath.Join(l.Dir, "frontend.env"), []byte(dockerEnvFile(env)), 0o600); err != nil {
		return Fail(StepFrontend, "writing frontend.env failed", err, "")
	}
	return l.compose(ctx, StepFrontend, "--profile", "frontend", "up", "-d", "frontend")
}
