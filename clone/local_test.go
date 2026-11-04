package clone

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFrontendImagePinnedNodeBeatsCheckout(t *testing.T) {
	code := t.TempDir()
	writeNodeFile(t, code, ".nvmrc", "18")
	if err := os.Mkdir(filepath.Join(code, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	l := &Local{CodeDir: code, Frontend: &FrontendConfig{Path: "web"}}

	if got := l.FrontendImage(); got != "node:18" {
		t.Errorf("checkout pin: got %q, want node:18", got)
	}
	l.NodeVersion = "20"
	if got := l.FrontendImage(); got != "node:18" {
		t.Errorf("a detected node (server or checkout) must not beat the frontend's own pin: got %q", got)
	}
	l.NodePinned = true
	if got := l.FrontendImage(); got != "node:20" {
		t.Errorf("--node / node: must win over the checkout: got %q", got)
	}
	writeNodeFile(t, filepath.Join(code, "web"), ".nvmrc", "22")
	if got := l.FrontendImage(); got != "node:20" {
		t.Errorf("--node / node: must win over the frontend directory too: got %q", got)
	}
	l.Frontend.Image = "node:21-alpine"
	if got := l.FrontendImage(); got != "node:21-alpine" {
		t.Errorf("frontend.image wins over everything: got %q", got)
	}
}

func TestFrontendImageFallbacks(t *testing.T) {
	l := &Local{CodeDir: t.TempDir(), Frontend: &FrontendConfig{Path: "web"}}
	if got := l.FrontendImage(); got != "node:lts" {
		t.Errorf("nothing known: got %q, want node:lts", got)
	}
	l.NodeVersion = "20"
	if got := l.FrontendImage(); got != "node:20" {
		t.Errorf("shop node without a checkout pin: got %q, want node:20", got)
	}
	writeNodeFile(t, l.CodeDir, ".nvmrc", "18")
	if got := l.FrontendImage(); got != "node:18" {
		t.Errorf("checkout pin beats an unpinned shop node: got %q, want node:18", got)
	}
	if got := (&Local{NodeVersion: "20", NodePinned: true}).FrontendImage(); got != "" {
		t.Errorf("no frontend: got %q, want \"\"", got)
	}
}

// authEnv isolates every composer credential source in temp directories.
func authEnv(t *testing.T) (home, code string) {
	t.Helper()
	home, code = t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("COMPOSER_HOME", "")
	t.Setenv("COMPOSER_AUTH", "")
	return home, code
}

func writeAuth(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// decodeAuth returns section → host → entry of a merged auth.json.
func decodeAuth(t *testing.T, data []byte) map[string]map[string]json.RawMessage {
	t.Helper()
	var auth map[string]map[string]json.RawMessage
	if err := json.Unmarshal(data, &auth); err != nil {
		t.Fatalf("merged auth is not section → host → entry: %v", err)
	}
	return auth
}

func TestFindComposerAuthNothingFound(t *testing.T) {
	_, code := authEnv(t)
	auth, err := FindComposerAuth("", code)
	if err != nil || auth.JSON != nil || auth.Source != "" {
		t.Fatalf("got %+v, %v", auth, err)
	}
}

func TestFindComposerAuthMergesSourcesByPrecedence(t *testing.T) {
	home, code := authEnv(t)
	composerHome := t.TempDir()
	t.Setenv("COMPOSER_HOME", composerHome)
	t.Setenv("COMPOSER_AUTH", `{"http-basic": {"packages.shopware.com": {"username": "env", "password": "e"}}}`)
	writeAuth(t, filepath.Join(code, "auth.json"), `{"http-basic": {"packages.shopware.com": {"username": "project", "password": "p"}, "repo.example.com": {"username": "project", "password": "p"}}, "bearer": {"api.example.com": "project-token"}}`)
	writeAuth(t, filepath.Join(composerHome, "auth.json"), `{"http-basic": {"repo.example.com": {"username": "composer-home", "password": "c"}}, "github-oauth": {"github.com": "home-token"}}`)
	writeAuth(t, filepath.Join(home, ".composer", "auth.json"), `{"github-oauth": {"github.com": "dot-composer-token"}, "gitlab-token": {"gitlab.com": "dot-composer"}}`)
	writeAuth(t, filepath.Join(home, ".config", "composer", "auth.json"), `{"gitlab-token": {"gitlab.com": "config-composer", "git.example.com": "config-composer"}}`)

	auth, err := FindComposerAuth("", code)
	if err != nil {
		t.Fatal(err)
	}
	wantSource := "COMPOSER_AUTH env, " + filepath.Join(code, "auth.json") + ", $COMPOSER_HOME/auth.json, ~/.composer/auth.json, ~/.config/composer/auth.json"
	if auth.Source != wantSource {
		t.Errorf("source %q, want %q", auth.Source, wantSource)
	}
	got := decodeAuth(t, auth.JSON)
	want := map[string]map[string]string{
		"http-basic":   {"packages.shopware.com": `{"username":"env","password":"e"}`, "repo.example.com": `{"username":"project","password":"p"}`},
		"bearer":       {"api.example.com": `"project-token"`},
		"github-oauth": {"github.com": `"home-token"`},
		"gitlab-token": {"gitlab.com": `"dot-composer"`, "git.example.com": `"config-composer"`},
	}
	if len(got) != len(want) {
		t.Errorf("sections %d, want %d", len(got), len(want))
	}
	for section, hosts := range want {
		if len(got[section]) != len(hosts) {
			t.Errorf("%s: %d hosts, want %d", section, len(got[section]), len(hosts))
		}
		for host, entry := range hosts {
			var w, g any
			if err := json.Unmarshal([]byte(entry), &w); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(got[section][host], &g); err != nil || !equalJSON(g, w) {
				t.Errorf("%s/%s: got %s, want %s", section, host, got[section][host], entry)
			}
		}
	}
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestFindComposerAuthFlagFileWinsAndIsMerged(t *testing.T) {
	_, code := authEnv(t)
	t.Setenv("COMPOSER_AUTH", `{"http-basic": {"packages.shopware.com": {"username": "env", "password": "e"}}, "bearer": {"api.example.com": "env-token"}}`)
	flag := filepath.Join(t.TempDir(), "my-auth.json")
	writeAuth(t, flag, `{"http-basic": {"packages.shopware.com": {"username": "flag", "password": "f"}}}`)

	auth, err := FindComposerAuth(flag, code)
	if err != nil {
		t.Fatal(err)
	}
	if auth.Source != "--composer-auth "+flag+", COMPOSER_AUTH env" {
		t.Errorf("source %q", auth.Source)
	}
	got := decodeAuth(t, auth.JSON)
	if !strings.Contains(string(got["http-basic"]["packages.shopware.com"]), `"flag"`) || string(got["bearer"]["api.example.com"]) != `"env-token"` {
		t.Errorf("merged %s", auth.JSON)
	}
}

func TestFindComposerAuthSingleSourceKeepsItsContent(t *testing.T) {
	_, code := authEnv(t)
	writeAuth(t, filepath.Join(code, "auth.json"), "{\n  \"http-basic\": {\"packages.shopware.com\": {\"username\": \"a&b\", \"password\": \"<p>\"}}\n}\n")
	auth, err := FindComposerAuth("", code)
	if err != nil {
		t.Fatal(err)
	}
	if auth.Source != filepath.Join(code, "auth.json") {
		t.Errorf("source %q", auth.Source)
	}
	got := decodeAuth(t, auth.JSON)
	if string(got["http-basic"]["packages.shopware.com"]) != `{"username":"a&b","password":"<p>"}` {
		t.Errorf("content must survive unescaped: %s", auth.JSON)
	}
}

func TestFindComposerAuthErrorsNameTheSourceNotTheContent(t *testing.T) {
	home, code := authEnv(t)
	secret := `["not-an-object-but-a-secret"]`
	writeAuth(t, filepath.Join(home, ".config", "composer", "auth.json"), secret)
	writeAuth(t, filepath.Join(code, "auth.json"), `{"http-basic": {}}`)

	_, err := FindComposerAuth("", code)
	var ce *Error
	if !errors.As(err, &ce) || ce.Step != StepConfig || !strings.Contains(ce.Msg, "~/.config/composer/auth.json") || ce.Hint == "" {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("error leaks file content: %v", err)
	}

	t.Setenv("COMPOSER_AUTH", `{"bearer": "secret-value"`)
	_, err = FindComposerAuth("", code)
	if !errors.As(err, &ce) || !strings.Contains(ce.Msg, "COMPOSER_AUTH env") || strings.Contains(err.Error(), "secret-value") {
		t.Errorf("got %v", err)
	}
	t.Setenv("COMPOSER_AUTH", "")

	flag := filepath.Join(t.TempDir(), "bad.json")
	writeAuth(t, flag, `"secret-string"`)
	_, err = FindComposerAuth(flag, code)
	if !errors.As(err, &ce) || !strings.Contains(ce.Msg, flag) || strings.Contains(err.Error(), "secret-string") {
		t.Errorf("got %v", err)
	}
}

func TestFindComposerAuthUnreadableFlagFile(t *testing.T) {
	_, code := authEnv(t)
	missing := filepath.Join(t.TempDir(), "missing.json")
	_, err := FindComposerAuth(missing, code)
	var ce *Error
	if !errors.As(err, &ce) || ce.Step != StepConfig || !strings.Contains(ce.Msg, missing) || !strings.Contains(ce.Hint, "--composer-auth") {
		t.Fatalf("got %v", err)
	}
}

// phpModulesOutput is `php -m` of a server with profilers and OPcache, which
// lists itself under both sections.
const phpModulesOutput = `PHP Warning:  Module "zstd" is already loaded in Unknown on line 0
[PHP Modules]
bcmath
bz2
Core
ctype
curl
imagick
mysqli
mysqlnd
PDO
pdo_mysql
pdo_sqlite
sqlite3
tideways
Xdebug
Zend OPcache
zstd

[Zend Modules]
Xdebug
Zend OPcache
`

func TestParsePHPModules(t *testing.T) {
	cases := []struct {
		name, out string
		want      []string
	}{
		{"php -m", phpModulesOutput, []string{"bcmath", "bz2", "core", "ctype", "curl", "imagick", "mysqli", "mysqlnd",
			"pdo", "pdo_mysql", "pdo_sqlite", "sqlite3", "tideways", "xdebug", "zend opcache", "zstd"}},
		{"unsorted with blank lines", "[PHP Modules]\nzstd\n\nbz2\n", []string{"bz2", "zstd"}},
		{"older probe without the section", "", nil},
		{"zend section only", "[Zend Modules]\nXdebug\n", nil},
	}
	for _, c := range cases {
		if got := parsePHPModules(c.out); fmt.Sprint(got) != fmt.Sprint(c.want) || (got == nil) != (c.want == nil) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPHPExtPackages(t *testing.T) {
	have := []string{"bcmath", "core", "curl", "mysqli", "mysqlnd", "pdo", "pdo_mysql"}
	cases := []struct {
		name string
		want []string
		pkgs []phpPackage
	}{
		{"zstd missing", []string{"zstd", "curl"}, []phpPackage{{"php8.3-zstd", []string{"zstd"}}}},
		{"pdo driver and its database share one package", []string{"sqlite3", "pdo_sqlite"},
			[]phpPackage{{"php8.3-sqlite3", []string{"pdo_sqlite", "sqlite3"}}}},
		{"mysql drivers present via alias", []string{"pdo_mysql", "mysqli", "mysqlnd"}, nil},
		{"profilers and debuggers ignored", []string{"tideways", "xdebug", "zend opcache", "opcache", "pcov", "blackfire"}, nil},
		{"nothing missing", have, nil},
		{"nothing wanted", nil, nil},
		{"sorted by package, duplicates once", []string{"zstd", "bz2", "zstd", "imagick", "tideways", "curl"},
			[]phpPackage{{"php8.3-bz2", []string{"bz2"}}, {"php8.3-imagick", []string{"imagick"}}, {"php8.3-zstd", []string{"zstd"}}}},
	}
	for _, c := range cases {
		got := phpExtPackages("8.3", c.want, have)
		if len(got) != len(c.pkgs) || fmt.Sprint(got) != fmt.Sprint(c.pkgs) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.pkgs)
		}
	}
}

func TestBufferPoolBytes(t *testing.T) {
	const mib = int64(1 << 20)
	cases := []struct {
		name       string
		memTotalKB int64
		want       int64
	}{
		{"2 GiB container: a quarter", 2 * 1024 * 1024, 512 * mib},
		{"27 GiB host: capped at 4 GiB", 27 * 1024 * 1024, 4096 * mib},
		{"4 GiB: one quarter is whole chunks", 4 * 1024 * 1024, 1024 * mib},
		{"5000000 kB: 1.19 GiB rounds down to 9 chunks", 5000000, 9 * 128 * mib},
		{"3000000 kB: 732 MiB rounds down to 5 chunks", 3000000, 5 * 128 * mib},
		{"256 MiB: below one chunk is 0, so never set", 256 * 1024, 0},
	}
	for _, c := range cases {
		got := bufferPoolBytes(c.memTotalKB)
		if got != c.want {
			t.Errorf("%s: bufferPoolBytes(%d) = %d, want %d", c.name, c.memTotalKB, got, c.want)
		}
		if got%(128*mib) != 0 {
			t.Errorf("%s: %d is not a multiple of 128 MiB", c.name, got)
		}
	}
}
