package clone

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Remote runs commands on the source server through the system ssh client, so
// ~/.ssh/config aliases, agents, jump hosts and known_hosts behave as usual.
type Remote struct {
	Target     string   // ssh alias or user@host
	Port       int      // optional
	Identity   string   // optional private key
	Root       string   // Shopware project root on the server
	Extra      []string // extra ssh options
	ControlDir string   // holds the shared connection's socket; set by ShareConnection
}

// ParseTarget accepts "alias", "user@host" or "ssh://user@host:port".
func ParseTarget(target string) (Remote, error) {
	if !strings.HasPrefix(target, "ssh://") {
		return Remote{Target: target}, nil
	}
	u, err := url.Parse(target)
	if err != nil {
		return Remote{}, err
	}
	r := Remote{Target: u.Hostname()}
	if u.User != nil {
		r.Target = u.User.Username() + "@" + r.Target
	}
	if p := u.Port(); p != "" {
		r.Port, _ = strconv.Atoi(p)
	}
	return r, nil
}

// SSHArgs returns the ssh options shared by ssh and rsync. The user's extra
// options come first: ssh keeps the first value it sees for an option, so
// --ssh-option overrides the defaults. The timeout covers slow banner
// exchanges through tunnels and jump hosts.
func (r Remote) SSHArgs() []string {
	args := append([]string(nil), r.Extra...)
	args = append(args, "-o", "BatchMode=yes", "-o", "ConnectTimeout=60", "-o", "ServerAliveInterval=30")
	if r.Port != 0 {
		args = append(args, "-p", strconv.Itoa(r.Port))
	}
	if r.Identity != "" {
		args = append(args, "-i", r.Identity, "-o", "IdentitiesOnly=yes")
	}
	if r.ControlDir != "" {
		// ControlPersist bounds a master left behind by a killed run
		args = append(args, "-o", "ControlMaster=auto", "-o", "ControlPath="+r.ControlDir+"/cm", "-o", "ControlPersist=60")
	}
	return args
}

// ShareConnection lets every later ssh and rsync call of r reuse one
// connection, so only the first pays the handshake. done closes the master
// and removes its socket; call it after the last ssh call has returned.
// Nothing is shared on Windows, whose OpenSSH has no ControlMaster, or when
// --ssh-option sets ControlMaster or ControlPath (e.g. ControlMaster=no).
func (r *Remote) ShareConnection() (done func(), err error) {
	if runtime.GOOS == "windows" || r.userSharing() {
		return func() {}, nil
	}
	// not os.TempDir(): macOS caps unix socket paths at 104 bytes and its
	// /var/folders/... temp dir leaves little room for that
	dir, err := os.MkdirTemp("/tmp", "dwc-ssh-")
	if err != nil {
		return nil, err
	}
	r.ControlDir = dir
	shared := *r
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = shared.control(ctx, "exit")
		_ = os.RemoveAll(dir)
	}, nil
}

// userSharing reports whether the extra options configure connection
// sharing themselves; ssh option names are case-insensitive.
func (r Remote) userSharing() bool {
	for _, a := range r.Extra {
		f := strings.FieldsFunc(a, func(c rune) bool { return c == '=' || c == ' ' || c == '\t' })
		if len(f) > 0 && (strings.EqualFold(f[0], "ControlMaster") || strings.EqualFold(f[0], "ControlPath")) {
			return true
		}
	}
	return false
}

// control sends a command (check, exit) to the shared connection's master.
func (r Remote) control(ctx context.Context, command string) error {
	return exec.CommandContext(ctx, "ssh", append(r.SSHArgs(), "-O", command, r.Target)...).Run()
}

// masterLost reports whether the shared connection has died. Its clients
// then exit 255 without a message, as the master logs to /dev/null, so only
// the missing master tells a dropped connection from a remote php fatal.
func (r Remote) masterLost() bool {
	if r.ControlDir == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return r.control(ctx, "check") != nil
}

// Exit codes of the generated remote scripts, so failures are told apart
// without parsing shell messages.
const (
	exitNoRoot        = 3 // project root missing or not enterable
	exitNoDatabaseURL = 4 // DATABASE_URL could not be loaded from the project
	exitNoShopware    = 5 // root exists but holds no Shopware project
	exitNoVendor      = 6 // Shopware project without an installed vendor/
	exitNoPHP         = 7 // php is not in PATH
	exitNoTemp        = 8 // no temp file in /tmp
	sshExit           = 255
)

func shExit(code int) string { return "exit " + strconv.Itoa(code) }

// stderrTail keeps the last bytes written, so a long remote run can report
// its final messages without buffering everything it printed.
type stderrTail struct {
	buf []byte
}

const stderrTailMax = 64 << 10

func (t *stderrTail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > stderrTailMax {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-stderrTailMax:]...)
	}
	return len(p), nil
}

func (t *stderrTail) String() string { return strings.TrimSpace(string(t.buf)) }

// Run executes a POSIX sh script on the server; stdin and stderr may be nil.
// A failed connection (agent, key, host key, network) is returned as *Error
// for StepSSH. A script that exits non-zero is returned as the plain
// *exec.ExitError so the caller can classify it by status; that includes a
// remote exit 255, which php uses for fatal errors, unless stderr shows the
// ssh client itself failed or the shared connection is gone.
func (r Remote) Run(ctx context.Context, script string, stdin io.Reader, stdout, stderr io.Writer) error {
	args := append(r.SSHArgs(), r.Target, "sh -c "+ShellQuote(script))
	cmd := exec.CommandContext(ctx, "ssh", args...)
	tail := &stderrTail{}
	cmd.Stdin, cmd.Stdout = stdin, stdout
	cmd.Stderr = tail
	if stderr != nil {
		cmd.Stderr = io.MultiWriter(tail, stderr)
	}
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	case !errors.As(err, &ee):
		return &Error{Step: StepSSH, Msg: "cannot start the ssh client", Hint: "OpenSSH's ssh must be installed and in PATH", Err: err}
	case ee.ExitCode() == sshExit && sshClientFailed(tail.String()):
		return Fail(StepSSH, "ssh to "+r.Target+" failed", err, tail.String())
	case ee.ExitCode() == sshExit && r.masterLost():
		return &Error{Step: StepSSH, Msg: "the ssh connection to " + r.Target + " was lost", Output: tail.String(), Err: err,
			Hint: "the connection dropped while a command ran on the server: check VPN or tunnel and retry"}
	}
	return err
}

// sshClientMessage matches the lines the OpenSSH client prints when it fails
// before or while running the remote command: connection, name resolution,
// host key, authentication, agent and option problems. A remote script's
// own output does not start lines like this.
var sshClientMessage = regexp.MustCompile(`(?m)^(ssh: |ssh_exchange_identification:|kex_exchange_identification:|banner exchange:|Connection timed out during banner exchange|Connection closed by |Connection reset by |Connection to .* closed|Received disconnect from |client_loop: |packet_write_wait: |Timeout, server |Host key verification failed|Could not resolve hostname|sign_and_send_pubkey: |Unable to negotiate with |Bad owner or permissions on |Bad configuration option|command-line: |usage: ssh )|Permission denied \((publickey|password|keyboard-interactive|gssapi|hostbased)|REMOTE HOST IDENTIFICATION HAS CHANGED`)

// sshClientFailed reports whether stderr carries a failure of the ssh client
// itself. ssh exits 255 for those, but so does a remote php fatal error, so
// the exit status alone cannot tell a lost connection from a failed script.
func sshClientFailed(stderr string) bool { return sshClientMessage.MatchString(stderr) }

// remoteExit returns the remote script's exit status, -1 when unknown.
func remoteExit(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// run executes a script and returns its stdout and trimmed stderr.
func (r Remote) run(ctx context.Context, script string, stdin io.Reader) (stdout, stderr string, err error) {
	var out, errb bytes.Buffer
	err = r.Run(ctx, script, stdin, &out, &errb)
	return out.String(), strings.TrimSpace(errb.String()), err
}

// withStderr appends the script's stderr to a plain exit error; classified
// errors are returned unchanged.
func withStderr(err error, stderr string) error {
	var ce *Error
	if errors.As(err, &ce) || stderr == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, stderr)
}

// Output runs a script and returns stdout; on failure stderr is part of the error.
func (r Remote) Output(ctx context.Context, script string) (string, error) {
	out, errb, err := r.run(ctx, script, nil)
	if err != nil {
		return out, withStderr(err, errb)
	}
	return out, nil
}

// ShellQuote quotes a string for POSIX shells.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// missingTool returns the first of names the remote shell reported as not
// found, "" when the output says nothing of the kind.
func missingTool(stderr string, names ...string) string {
	for _, n := range names {
		if strings.Contains(stderr, n+": not found") || strings.Contains(stderr, n+": command not found") {
			return n
		}
	}
	return ""
}

// rootError explains a failed cd into the project root.
func (r Remote) rootError(err error, stderr string) error {
	return &Error{Step: StepProbe, Msg: "remote path " + r.Root + " does not exist or is not readable",
		Hint:   "pass --path <project root on the server> (the directory with bin/console and composer.json) and make sure the ssh user may enter it",
		Output: stderr, Err: err}
}

// dbEnvScript loads DATABASE_URL the way Shopware does (real env wins over
// .env files) and exports the parts for the mysql client. The password only
// ever lives in the remote MYSQL_PWD variable. The php output is captured
// before eval so a php failure (unreadable .env) ends the script.
func (r Remote) dbEnvScript() string {
	return `cd ` + ShellQuote(r.Root) + ` || ` + shExit(exitNoRoot) + `
E=$(php -r '
if (is_file("vendor/autoload.php")) { require "vendor/autoload.php"; }
if (class_exists("Symfony\\Component\\Dotenv\\Dotenv") && is_file(".env")) {
  (new Symfony\Component\Dotenv\Dotenv())->usePutenv()->bootEnv(getcwd()."/.env", "prod");
}
$raw = getenv("DATABASE_URL");
if (!$raw) { fwrite(STDERR, "DATABASE_URL not found\n"); exit(1); }
$u = parse_url($raw);
$v = ["DBH" => $u["host"] ?? "127.0.0.1", "DBP" => (string)($u["port"] ?? 3306), "DBU" => rawurldecode($u["user"] ?? ""), "MYSQL_PWD" => rawurldecode($u["pass"] ?? ""), "DBN" => rawurldecode(ltrim($u["path"] ?? "", "/"))];
foreach ($v as $k => $x) { echo "export $k=" . escapeshellarg($x) . "\n"; }
') || ` + shExit(exitNoDatabaseURL) + `
eval "$E"
M="mysql --default-character-set=utf8mb4 -h$DBH -P$DBP -u$DBU"
`
}

// dbError classifies a failed mysql or mysqldump run that used dbEnvScript;
// step is used for failures of the SQL itself.
func (r Remote) dbError(step, msg string, err error, stderr string) error {
	var ce *Error
	if errors.As(err, &ce) {
		return err
	}
	switch remoteExit(err) {
	case exitNoRoot:
		return r.rootError(err, stderr)
	case exitNoDatabaseURL:
		return &Error{Step: StepDatabase, Msg: "DATABASE_URL could not be read from the project at " + r.Root,
			Hint:   "the ssh user must be able to read .env and .env.local in the project root (or have DATABASE_URL exported in its environment)",
			Output: stderr, Err: err}
	}
	if tool := missingTool(stderr, "mysql", "mysqldump", "php"); tool != "" {
		return &Error{Step: StepDatabase, Msg: "the " + tool + " command is not available in the ssh user's PATH on the server",
			Hint:   "install the MySQL/MariaDB client tools (and php-cli) on the server or add them to the PATH of non-interactive ssh sessions",
			Output: stderr, Err: err}
	}
	return Fail(step, msg, err, stderr)
}

// MySQL runs SQL against the remote project database and returns tab separated rows.
func (r Remote) MySQL(ctx context.Context, sql string) (string, error) {
	out, errb, err := r.run(ctx, r.dbEnvScript()+`exec $M -N -B "$DBN"`, strings.NewReader(sql))
	if err != nil {
		return "", r.dbError(StepDatabase, "querying the remote database failed", err, errb)
	}
	return out, nil
}

// granteeSQL is the GRANTEE form ('user'@'host') of the connected database
// user, for information_schema privilege lookups.
const granteeSQL = `CONCAT("'", SUBSTRING_INDEX(CURRENT_USER(), '@', 1), "'@'", SUBSTRING_INDEX(CURRENT_USER(), '@', -1), "'")`

// Probe holds everything learned about the source without reading personal data.
type Probe struct {
	Arch            string
	PHPVersion      string
	PHPExtensions   []string // lowercase, sorted `php -m` modules of the server's php; nil when the probe did not report them
	NodeVersion     string   // major version of the server's node, "" when absent
	ShopwareVersion string
	MySQLVersion    string
	Database        string
	BinlogFormat    string
	CanCreateDB     bool
	Tables          []string
	TableRows       map[string]int64
	TableBytes      map[string]int64
	Schema          Schema
	CoreTables      map[string]bool
	AppURL          string
	EnvKeys         []string
	SafeEnv         map[string]string // .env values of keys that are clearly not credentials (flag 1)
	EnvSchemes      map[string]string // URL scheme ("redis", "https", …) of secret-looking keys whose value is a URL (flag 2); the value itself stays on the server
	Domains         []Domain
	MediaURL        string // public filesystem url from config/packages, else APP_URL
	HasStorefront   bool
	HasComposerAuth bool
	hasGzip         bool // gzip exists on the server (compressed uploads)
}

// Domain is one sales channel domain with its channel's type and access key.
type Domain struct {
	SalesChannel string
	Name         string
	TypeID       string
	URL          string
}

const (
	SalesChannelTypeStorefront = "8a243080f92e4c719546314b577cf82b"
	SalesChannelTypeAPI        = "f183ee5650cf4bdb8a774337575067a6"
)

// envScanScript lists .env keys and returns values only for keys that are
// clearly not credentials. Secret values never leave the server; for a secret
// key whose value is a URL only its scheme is reported (flag 2), so the local
// stack can point redis DSNs at its own redis.
const envScanScript = `php -r '
$f = is_file(".env") ? file(".env", FILE_IGNORE_NEW_LINES) : [];
foreach ($f as $l) {
  if (!preg_match("/^\s*(?:export\s+)?([A-Z0-9_]+)\s*=\s*(.*)$/", $l, $m)) continue;
  $k = $m[1]; $v = trim($m[2], " \"'\''");
  $secret = preg_match("/(SECRET|PASS|PWD|TOKEN|KEY|AUTH|CRED|SALT|PRIVATE|DSN|URL)/", $k) || preg_match("/(HOST|USER|ID)$/", $k);
  $safe = !$secret && preg_match("/^[A-Za-z0-9_.\/-]{0,40}$/", $v);
  $scheme = $secret && preg_match("#^([a-z][a-z0-9+.-]*)://#i", $v, $s) ? strtolower($s[1]) : "";
  echo $k, "\t", $safe ? "1\t".$v : ($scheme !== "" ? "2\t".$scheme : "0\t"), "\n";
}'`

// nodeMajor returns "20" for "v20.11.1" and "" for anything that is not a version.
func nodeMajor(v string) string {
	major, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if _, err := strconv.Atoi(major); err != nil {
		return ""
	}
	return major
}

// probeError classifies a failed probe script by its exit status.
func (r Remote) probeError(err error, stderr string) error {
	switch remoteExit(err) {
	case exitNoRoot:
		return r.rootError(err, stderr)
	case exitNoShopware:
		return &Error{Step: StepProbe, Msg: "no Shopware project at " + r.Root,
			Hint:   "--path must be the project root on the server (the directory with bin/console and composer.json)",
			Output: stderr, Err: err}
	case exitNoVendor:
		return &Error{Step: StepProbe, Msg: "vendor/shopware/core is missing at " + r.Root,
			Hint:   "the server needs its vendor/ installed (composer install there); if --path points to another project, it is not Shopware",
			Output: stderr, Err: err}
	case exitNoPHP:
		return &Error{Step: StepProbe, Msg: "php is not in PATH for non-interactive ssh sessions on the server",
			Hint:   "add the php binary to the PATH of non-interactive ssh sessions (top of ~/.bashrc above its interactive guard, or ~/.ssh/environment)",
			Output: stderr, Err: err}
	}
	return Fail(StepProbe, "probing "+r.Root+" failed", err, stderr)
}

// RunProbe inspects the server: versions, schema metadata, core tables,
// domains and privileges. No row of a personal-data table is read.
func (r Remote) RunProbe(ctx context.Context) (*Probe, error) {
	p := &Probe{TableRows: map[string]int64{}, TableBytes: map[string]int64{}, Schema: Schema{}, SafeEnv: map[string]string{}, EnvSchemes: map[string]string{}}

	out, errb, err := r.run(ctx, `cd `+ShellQuote(r.Root)+` || `+shExit(exitNoRoot)+`
test -f composer.json && test -f bin/console || { echo "no composer.json and bin/console in $(pwd)" >&2; `+shExit(exitNoShopware)+`; }
test -f vendor/shopware/core/composer.json || { echo "no vendor/shopware/core in $(pwd)" >&2; `+shExit(exitNoVendor)+`; }
command -v php >/dev/null 2>&1 || { echo "php: not found" >&2; `+shExit(exitNoPHP)+`; }
echo "arch=$(uname -m)"
echo "php=$(php -r 'echo PHP_MAJOR_VERSION.".".PHP_MINOR_VERSION;')"
echo "node=$( (command -v node >/dev/null 2>&1 && node -v) 2>/dev/null || (test -s "$HOME/.nvm/nvm.sh" && bash -c '. "$HOME/.nvm/nvm.sh" >/dev/null 2>&1 && node -v') 2>/dev/null || true)"
echo "gzip=$(command -v gzip >/dev/null 2>&1 && echo 1 || echo 0)"
echo "shopware=$(php -r '$l=json_decode(file_get_contents("composer.lock"),true); foreach(array_merge($l["packages"],$l["packages-dev"]??[]) as $p){ if($p["name"]==="shopware/core"){ echo ltrim($p["version"],"v"); } }')"
echo "storefront=$(test -d vendor/shopware/storefront && echo 1 || echo 0)"
echo "composerauth=$(test -f auth.json && echo 1 || echo 0)"
echo "appurl=$(php -r 'if(is_file("vendor/autoload.php")){require "vendor/autoload.php";} if(class_exists("Symfony\\Component\\Dotenv\\Dotenv")&&is_file(".env")){(new Symfony\Component\Dotenv\Dotenv())->usePutenv()->bootEnv(getcwd()."/.env","prod");} echo getenv("APP_URL");')"
echo "mediaurl=$(php -r '$u=""; foreach(glob("config/packages/*.yaml") as $f){ if(preg_match("/filesystem:\s*\n\s+public:\s*\n(?:\s+(?:type|config|root):.*\n)*\s+url:\s*[\"\x27]?([^\"\x27\s]+)/", file_get_contents($f), $m)){ $u=$m[1]; } } echo $u;')"
echo "--env--"
`+envScanScript+`
echo "--php-modules--"
php -m
echo "--core--"
{ grep -rhoiE "CREATE TABLE (IF NOT EXISTS )?[\`+"`"+`]?[a-zA-Z0-9_]+" vendor/shopware --include=*.php --include=*.sql | sed -E "s/.*TABLE (IF NOT EXISTS )?[\`+"`"+`]?//I"
  grep -rhoE "ENTITY_NAME = .[a-z0-9_]+." vendor/shopware --include=*.php | sed -E "s/.*= .//; s/.$//"; } | tr A-Z a-z | sort -u
`, nil)
	if err != nil {
		return nil, r.probeError(err, errb)
	}
	section := ""
	coreLines, moduleLines := []string{}, []string{}
	for _, l := range strings.Split(out, "\n") {
		switch {
		case l == "--env--" || l == "--php-modules--" || l == "--core--":
			section = l
		case section == "--env--":
			f := strings.SplitN(l, "\t", 3)
			if len(f) < 2 {
				continue
			}
			p.EnvKeys = append(p.EnvKeys, f[0])
			if len(f) == 3 {
				switch f[1] {
				case "1":
					p.SafeEnv[f[0]] = f[2]
				case "2":
					p.EnvSchemes[f[0]] = f[2]
				}
			}
		case section == "--php-modules--":
			moduleLines = append(moduleLines, l)
		case section == "--core--":
			coreLines = append(coreLines, l)
		default:
			k, v, _ := strings.Cut(l, "=")
			switch k {
			case "arch":
				p.Arch = v
			case "php":
				p.PHPVersion = v
			case "node":
				p.NodeVersion = nodeMajor(v)
			case "gzip":
				p.hasGzip = v == "1"
			case "shopware":
				p.ShopwareVersion = v
			case "storefront":
				p.HasStorefront = v == "1"
			case "composerauth":
				p.HasComposerAuth = v == "1"
			case "appurl":
				p.AppURL = v
			case "mediaurl":
				p.MediaURL = v
			}
		}
	}
	p.CoreTables = ParseCoreTables(strings.Join(coreLines, "\n"))
	p.PHPExtensions = parsePHPModules(strings.Join(moduleLines, "\n"))

	meta, err := r.MySQL(ctx, `SELECT 'v', VERSION(), DATABASE(), @@binlog_format;
SELECT 't', table_name, IFNULL(table_rows,0), IFNULL(data_length+index_length,0) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE' ORDER BY table_name;
SELECT 'c', table_name, column_name, is_nullable, data_type, IF(extra IN ('VIRTUAL GENERATED','STORED GENERATED'), 1, 0) FROM information_schema.columns WHERE table_schema = DATABASE();
SELECT 'd', LOWER(HEX(sc.id)), IFNULL(t.name, ''), LOWER(HEX(sc.type_id)), IFNULL(d.url, '') FROM sales_channel sc LEFT JOIN sales_channel_translation t ON t.sales_channel_id = sc.id AND t.language_id = sc.language_id LEFT JOIN sales_channel_domain d ON d.sales_channel_id = sc.id;
SELECT 'g', COUNT(*) FROM information_schema.user_privileges WHERE privilege_type = 'CREATE' AND grantee = `+granteeSQL+`;
`)
	if err != nil {
		return nil, err
	}
	for _, l := range strings.Split(strings.TrimSpace(meta), "\n") {
		f := strings.Split(l, "\t")
		switch f[0] {
		case "v":
			p.MySQLVersion, p.Database, p.BinlogFormat = f[1], f[2], f[3]
		case "t":
			p.Tables = append(p.Tables, f[1])
			p.TableRows[f[1]], _ = strconv.ParseInt(f[2], 10, 64)
			p.TableBytes[f[1]], _ = strconv.ParseInt(f[3], 10, 64)
		case "c":
			t := f[1]
			if p.Schema[t] == nil {
				p.Schema[t] = map[string]Column{}
			}
			p.Schema[t][f[2]] = Column{Table: t, Name: f[2], Nullable: f[3] == "YES", DataType: strings.ToLower(f[4]), Generated: f[5] == "1"}
		case "d":
			p.Domains = append(p.Domains, Domain{SalesChannel: f[1], Name: f[2], TypeID: f[3], URL: f[4]})
		case "g":
			p.CanCreateDB = f[1] != "0"
		}
	}
	if p.MediaURL == "" {
		p.MediaURL = strings.TrimRight(p.AppURL, "/")
	}
	return p, nil
}

// upload copies a local file into a fresh private temp dir on the server and
// returns the dir. With compress the bytes travel gzip'd (BestSpeed, roughly
// 2.5:1 for a Go binary) and are inflated on the server. The dir is removed
// again if the transfer fails, is interrupted, or arrives incomplete.
func (r Remote) upload(ctx context.Context, local, name string, compress bool) (dir string, err error) {
	f, err := os.Open(local)
	if err != nil {
		return "", Fail(StepUpload, "cannot read "+local, err, "")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", Fail(StepUpload, "cannot read "+local, err, "")
	}
	var src io.Reader = f
	receive := "cat"
	if compress {
		receive = "gzip -dc"
		pr, pw := io.Pipe()
		defer pr.Close() // unblocks the writer if ssh stopped reading early
		go func() {
			gz, _ := gzip.NewWriterLevel(pw, gzip.BestSpeed)
			if _, err := io.Copy(gz, f); err != nil {
				pw.CloseWithError(err)
				return
			}
			pw.CloseWithError(gz.Close())
		}()
		src = pr
	}
	target := `"$d/` + name + `"`
	script := `umask 077; d=$(mktemp -d /tmp/dwc.XXXXXX) || ` + shExit(exitNoTemp) + `; trap 'rm -rf "$d"' INT TERM HUP
if ` + receive + ` > ` + target + ` && chmod 700 ` + target + `; then trap - INT TERM HUP; echo "$d"; wc -c < ` + target + `; else rm -rf "$d"; exit 1; fi`
	out, errb, err := r.run(ctx, script, src)
	if err != nil {
		if remoteExit(err) == exitNoTemp {
			return "", &Error{Step: StepUpload, Msg: "cannot create a temp dir in /tmp on the server", Hint: "/tmp must be writable for the ssh user; check disk space and quota there", Output: errb, Err: err}
		}
		return "", Fail(StepUpload, "uploading "+name+" to the server failed", err, errb)
	}
	dir, sizeLine, _ := strings.Cut(strings.TrimSpace(out), "\n")
	size, _ := strconv.ParseInt(strings.TrimSpace(sizeLine), 10, 64)
	if dir == "" || size != st.Size() {
		if dir != "" {
			r.cleanup(dir)
		}
		return "", &Error{Step: StepUpload, Msg: fmt.Sprintf("%s arrived incomplete on the server (%d of %d bytes)", name, size, st.Size()),
			Hint: "the connection dropped or /tmp on the server is full; retry, or free space there", Output: errb}
	}
	return dir, nil
}

// putFile writes content to dir/name on the server.
func (r Remote) putFile(ctx context.Context, dir, name string, content []byte) error {
	_, errb, err := r.run(ctx, `umask 077; cat > `+ShellQuote(dir+"/"+name), bytes.NewReader(content))
	if err != nil {
		return Fail(StepUpload, "writing "+name+" to "+dir+" on the server failed", err, errb)
	}
	return nil
}

// cleanup removes the temp dir; errors are reported but not fatal.
func (r Remote) cleanup(dir string) error {
	if !strings.HasPrefix(dir, "/tmp/dwc.") {
		return &Error{Step: StepUpload, Msg: "refusing to remove " + dir + " on the server"}
	}
	_, errb, err := r.run(context.Background(), "rm -rf "+ShellQuote(dir), nil)
	if err != nil {
		return Fail(StepUpload, "removing "+dir+" on the server failed", err, errb)
	}
	return nil
}
