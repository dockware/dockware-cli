package clone

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Steps a clone can fail in; they prefix the message and are stored in the audit.
const (
	StepConfig     = "config"
	StepSSH        = "ssh"
	StepProbe      = "probe"
	StepDatabase   = "database"
	StepShadow     = "shadow"
	StepUpload     = "upload"
	StepDump       = "dump"
	StepRsync      = "rsync"
	StepDocker     = "docker"
	StepComposer   = "composer"
	StepImport     = "import"
	StepVerify     = "verify"
	StepPostImport = "post-import"
	StepFrontend   = "frontend"
	StepSmoke      = "smoke"
)

// Error is a failed clone step: what failed, why, and what to do about it.
type Error struct {
	Step   string // one of the Step* constants
	Msg    string // what failed, one line
	Hint   string // what the user can do; empty when nothing specific is known
	Output string // tail of the failing tool's output
	Err    error  // underlying error, may be nil
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s", e.Step, e.Msg)
	if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	if out := strings.TrimSpace(e.Output); out != "" {
		b.WriteString("\n  output:\n    " + strings.ReplaceAll(tailLines(out, 15), "\n", "\n    "))
	}
	if e.Hint != "" {
		b.WriteString("\n  hint: " + e.Hint)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// Fail builds an *Error and derives the hint from the tool output and the
// error text. An existing *Error in err is returned unchanged, so the most
// specific classification wins when errors are wrapped on the way up.
func Fail(step, msg string, err error, output string) error {
	var ce *Error
	if errors.As(err, &ce) {
		return err
	}
	text := output
	if err != nil {
		text += "\n" + err.Error()
	}
	return &Error{Step: step, Msg: msg, Hint: hintFor(step, text), Output: output, Err: err}
}

type hint struct {
	steps []string // empty = any step
	re    *regexp.Regexp
	text  string
}

// serverSteps talk to the source server over ssh; only their output can
// carry ssh client or remote MySQL messages.
var serverSteps = []string{StepSSH, StepProbe, StepDatabase, StepShadow, StepUpload, StepDump, StepRsync}

// localDBSteps run SQL against the MySQL inside the local shop container.
var localDBSteps = []string{StepImport, StepPostImport, StepVerify}

// hints are checked in order; the first match wins, so step-specific entries
// come before the generic ones that match the same text.
var hints = []hint{
	// composer's git failures quote ssh messages; they need the agent hint, not the login one
	{[]string{StepComposer}, regexp.MustCompile(`(?i)Permission denied \(publickey\)|Could not read from remote repository|Host key verification failed`),
		"a private git package needs your ssh key: load it into the ssh agent (`ssh-add -l` must list it); the agent is forwarded into the container"},
	{nil, regexp.MustCompile(`agent refused operation|communication with agent failed|sign_and_send_pubkey: signing failed`),
		"the ssh agent could not sign: unlock 1Password (or your agent) and approve the request, check `ssh-add -l`, or pass --identity <key>"},
	{serverSteps, regexp.MustCompile(`Permission denied \((publickey|password|keyboard-interactive)`),
		"the server rejected the ssh login: check user and host (ssh config alias), pass --identity <key>, or authorise your public key on the server"},
	{serverSteps, regexp.MustCompile(`Host key verification failed|REMOTE HOST IDENTIFICATION HAS CHANGED`),
		"the host key is unknown or changed: connect once with `ssh <target>` to verify it, or fix ~/.ssh/known_hosts"},
	{serverSteps, regexp.MustCompile(`Could not resolve hostname`),
		"the host name does not resolve: check the ssh target or the Host alias in ~/.ssh/config"},
	{serverSteps, regexp.MustCompile(`Connection timed out|Operation timed out|No route to host|Network is unreachable|Connection refused|banner exchange`),
		"the server is not reachable: check VPN, firewall/IP allow-list and the ssh port (--ssh-port)"},
	{[]string{StepShadow}, regexp.MustCompile(`(?i)CREATE command denied|Access denied for user .* to database|ERROR 1044`),
		"the database user may not create schemas: use --strategy stream (default), or ask the hoster for CREATE on <db>_dwc_shadow"},
	{serverSteps, regexp.MustCompile(`(?i)Access denied for user|ERROR 1045`),
		"the server rejected the database credentials from its .env (DATABASE_URL): check them on the server"},
	{serverSteps, regexp.MustCompile(`(?i)Can't connect to (local )?MySQL server|ERROR 2002|ERROR 2003|ERROR 2005`),
		"the database server is not reachable from the ssh host: check DATABASE_URL host/port in the server .env"},
	// the same MySQL errors from the container mean the local database, not the server
	{localDBSteps, regexp.MustCompile(`(?i)Can't connect to (local )?MySQL server|ERROR 200[23]|SQLSTATE\[HY000\] \[200[23]\]`),
		"the local database in the shop container is not up: docker compose logs shop"},
	{localDBSteps, regexp.MustCompile(`(?i)Access denied for user|ERROR 1045`),
		"the local MySQL in the shop container rejected root/root: the image is not a stock dockware image, check --image"},
	{nil, regexp.MustCompile(`(?i)No space left on device|Disk quota exceeded|ERROR 1114`),
		"a disk is full: free space in /tmp on the server or in Docker Desktop (docker system df)"},
	{[]string{StepUpload, StepDump}, regexp.MustCompile(`(?i)Exec format error|cannot execute binary file`),
		"the uploaded shopware-cli does not run on the server (architecture or libc); try --strategy shadow"},
	{[]string{StepUpload, StepDump}, regexp.MustCompile(`Permission denied`),
		"the server does not allow executing files in /tmp (noexec mount?) or writing there; try --strategy shadow"},
	{[]string{StepRsync}, regexp.MustCompile(`(?i)Permission denied|rsync error: some files/attrs were not transferred \(code 23\)`),
		"the ssh user cannot read some project files (see output): fix permissions on the server or exclude them (exclude: in .dockware-clone.yml or --exclude)"},
	{[]string{StepRsync}, regexp.MustCompile(`(?i)rsync: command not found|rsync: not found|protocol version mismatch`),
		"rsync is missing on the server: install it there or pass --code <local checkout>"},
	{[]string{StepProbe}, regexp.MustCompile(`(?i)Permission denied`),
		"the ssh user cannot read the project (see output): check --path and file permissions on the server"},
	{[]string{StepComposer}, regexp.MustCompile(`(?i)Authentication required|401 Unauthorized|Invalid credentials|could not authenticate|http-basic|bearer`),
		"composer needs credentials (e.g. packages.shopware.com): pass --composer-auth <auth.json>, put auth.json in the checkout, ~/.composer/auth.json or ~/.config/composer/auth.json, or set COMPOSER_AUTH"},
	{[]string{StepComposer}, regexp.MustCompile(`(?i)requires php|your php version|ext-[a-z0-9_]+ \* -> it is missing`),
		"the lock file needs another PHP version or extension than the container has: check the detected PHP version or pass --php"},
	// composer and nvm download failures inside the container; after the
	// credential hint, as a 401 from packagist is also reported as a curl error
	{[]string{StepComposer, StepDocker}, regexp.MustCompile(`(?i)Could not resolve host|curl error|Connection timed out|Operation timed out|Network is unreachable`),
		"no network access from the container to packagist/nodejs.org/GitHub: check Docker's network/proxy"},
	// the entrypoint relinks node to NODE_VERSION without installing it and
	// exits; after the network hint, as a failed nvm install reads the same
	{[]string{StepDocker}, regexp.MustCompile(`Version '.*' does not exist|is not yet installed`),
		"the image could not switch node: a NODE_VERSION value reached the container; pass --node <major> instead"},
	{[]string{StepDocker}, regexp.MustCompile(`(?i)Cannot connect to the Docker daemon|docker daemon is not running|error during connect|docker\.sock`),
		"Docker is not running: start Docker Desktop and retry"},
	{[]string{StepDocker, StepFrontend}, regexp.MustCompile(`(?i)port is already allocated|address already in use|bind: .*in use`),
		"the port is already in use: stop the other container or pass --port / --frontend-port"},
	{[]string{StepDocker, StepFrontend}, regexp.MustCompile(`(?i)pull access denied|manifest unknown|not found: manifest|failed to resolve reference`),
		"the image does not exist or needs a docker login: check --image / frontend.image"},
	{nil, regexp.MustCompile(`(?i)command not found|: not found`),
		"a required tool is missing on that host (see output); install it or use another strategy"},
	{[]string{StepImport}, regexp.MustCompile(`(?i)ERROR \d+`),
		"the dump does not import into the local MySQL: see the SQL error above; the local MySQL version may differ from the server"},
}

func hintFor(step, text string) string {
	for _, h := range hints {
		if len(h.steps) > 0 && !containsString(h.steps, step) {
			continue
		}
		if h.re.MatchString(text) {
			return h.text
		}
	}
	return ""
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
