package clone

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Output of `rsync --dry-run --out-format='%l %n'` against an empty directory:
// directories end with a slash, symlinks list the target length, files above
// the 50 MiB limit are listed like any other.
const dryRunOutput = `4096 ./
4096 custom/
4096 custom/plugins/
4096 custom/plugins/Foo/
4096 custom/plugins/Foo/fixtures/
7000000 custom/plugins/Foo/fixtures/data.csv
3000000 custom/plugins/Foo/src/Plugin.php
4096 custom/plugins/Bar/
1000000 custom/plugins/Bar/composer.json
4096 public/
2000000000 public/big.bin
20000000 public/catalog with space.pdf
52428801 public/other big.bin
50 composer.json
7 link
`

func TestParseRsyncDryRunTotalsAndOrdersDirectories(t *testing.T) {
	s := parseRsyncDryRun(dryRunOutput)
	if s.Files != 6 || s.Bytes != 31000057 {
		t.Fatalf("files=%d bytes=%d", s.Files, s.Bytes)
	}
	var dirs []string
	for _, d := range s.Dirs {
		dirs = append(dirs, fmt.Sprintf("%s=%d", d.Dir, d.Bytes))
	}
	want := "public/=20000000 custom/=11000000 custom/plugins/=11000000 custom/plugins/Foo/=10000000 custom/plugins/Bar/=1000000"
	if got := strings.Join(dirs, " "); got != want {
		t.Errorf("dirs\n got %s\nwant %s", got, want)
	}
	if got := strings.Join(s.Skipped, ","); got != "public/big.bin,public/other big.bin" {
		t.Errorf("skipped = %q", got)
	}
}

func TestParseRsyncDryRunSkipsOnlyFilesAboveMaxSize(t *testing.T) {
	limit := int64(codeMaxFileBytes)
	s := parseRsyncDryRun(fmt.Sprintf("4096 ./\n%d at-limit.bin\n4096 over/\n%d over/limit.bin\n", limit, limit+1))
	if s.Files != 1 || s.Bytes != limit || len(s.Dirs) != 0 {
		t.Fatalf("files=%d bytes=%d dirs=%v", s.Files, s.Bytes, s.Dirs)
	}
	if got := strings.Join(s.Skipped, ","); got != "over/limit.bin" {
		t.Errorf("skipped = %q", got)
	}
}

func TestCheckCodeSizeNamesLargestDirectories(t *testing.T) {
	s := parseRsyncDryRun(dryRunOutput)
	if err := checkCodeSize(s, s.Bytes); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	err := checkCodeSize(s, 3e6)
	var ce *Error
	if !errors.As(err, &ce) || ce.Step != StepRsync {
		t.Fatalf("not an rsync error: %v", err)
	}
	if ce.Msg != "code on the server is 31.0 MB, more than the 3.0 MB limit" {
		t.Errorf("msg = %q", ce.Msg)
	}
	lines := strings.Split(strings.TrimSpace(ce.Output), "\n")
	if lines[0] != "largest directories:" || !strings.HasSuffix(lines[1], "20.0 MB  public/") || len(lines) != 6 {
		t.Errorf("output = %q", ce.Output)
	}
	if !strings.Contains(ce.Hint, "--exclude") || !strings.Contains(ce.Hint, "--max-code-size") {
		t.Errorf("hint = %q", ce.Hint)
	}
}

func TestCheckCodeSizeListsAtMostTen(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&b, "%d dir%02d/file\n", (i+1)*1e6, i)
	}
	err := checkCodeSize(parseRsyncDryRun(b.String()), 1)
	out := strings.TrimSpace(err.(*Error).Output)
	if n := strings.Count(out, "\n"); n != 10 {
		t.Fatalf("%d directory lines:\n%s", n, out)
	}
	if !strings.HasSuffix(strings.Split(out, "\n")[1], "dir11/") {
		t.Fatalf("largest directory first:\n%s", out)
	}
}

func TestRsyncErrorReportsDeniedPaths(t *testing.T) {
	err := exec.Command("sh", "-c", "exit 23").Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Skip("no sh to produce exit status 23")
	}
	stderr := `rsync: [sender] opendir "/srv/shop/files/private" failed: Permission denied (13)
rsync: [sender] send_files failed to open "/srv/shop/config/secret.yaml": Permission denied (13)
rsync error: some files/attrs were not transferred (see previous errors) (code 23) at main.c(1338) [sender=3.2.7]
`
	e := rsyncError(err, stderr)
	var ce *Error
	if !errors.As(e, &ce) || ce.Step != StepRsync {
		t.Fatalf("not an rsync error: %v", e)
	}
	if strings.Count(ce.Output, "\n") != 1 || !strings.Contains(ce.Output, "/srv/shop/files/private") || strings.Contains(ce.Output, "rsync error:") {
		t.Errorf("output should list only the denied paths:\n%s", ce.Output)
	}
	if !strings.Contains(ce.Hint, "cannot read some project files") {
		t.Errorf("hint = %q", ce.Hint)
	}
}

func TestRsyncErrorHintsAtOldLocalRsync(t *testing.T) {
	e := rsyncError(errors.New("exit status 1"), "rsync: --out-format=%l %n: unknown option\n")
	if h := e.(*Error).Hint; !strings.Contains(h, "brew install rsync") {
		t.Fatalf("hint = %q", h)
	}
}

func TestSyncCodeRejectsInvalidExcludeBeforeConnecting(t *testing.T) {
	r := Remote{Target: "nobody@localhost", Root: "/srv/shop"}
	err := SyncCode(context.Background(), r, t.TempDir(), CodeSyncOptions{Exclude: []string{"/public/x/", "bad\npattern"}}, io.Discard)
	var ce *Error
	if !errors.As(err, &ce) || ce.Step != StepConfig || !strings.Contains(ce.Msg, "invalid exclude pattern") {
		t.Fatalf("got %v", err)
	}
}

func TestRsyncArgsOrderUserExcludesThenFixturesThenBuiltins(t *testing.T) {
	r := Remote{Target: "shop", Port: 2222, Root: "/srv/shop/"}
	args := rsyncArgs(r, "/srv/shop/", "/tmp/code", []string{" /custom/plugins/Huge/ "}, false)
	joined := strings.Join(args, "\x00")
	user := strings.Index(joined, "\x00- /custom/plugins/Huge/\x00")
	include := strings.Index(joined, "\x00+ /custom/**/Resources/**.csv\x00")
	exclude := strings.Index(joined, "\x00- *.csv\x00")
	if user < 0 || include < 0 || exclude < 0 || user > include || include > exclude {
		t.Fatalf("filter order user=%d include=%d exclude=%d in %q", user, include, exclude, args)
	}
	if strings.Contains(joined, "--dry-run") {
		t.Fatalf("real run must not be a dry run: %q", args)
	}
	if !strings.Contains(joined, "\x00- /vendor/\x00") || !strings.Contains(joined, "'-p' '2222'") {
		t.Fatalf("vendor exclude or ssh port missing in %q", args)
	}
	if args[len(args)-2] != "shop:/srv/shop/" || args[len(args)-1] != "/tmp/code/" {
		t.Fatalf("endpoints %q", args[len(args)-2:])
	}
	if !strings.Contains(joined, "\x00--max-size=50m\x00") {
		t.Fatalf("real run must cap single files: %q", args)
	}
	dry := strings.Join(rsyncArgs(r, "/srv/shop/", "/tmp/x", nil, true), "\x00")
	if !strings.Contains(dry, "--dry-run") || !strings.Contains(dry, "--out-format=%l %n") {
		t.Fatalf("dry run args %q", dry)
	}
	if strings.Contains(dry, "--max-size") || strings.Contains(dry, "--info") {
		t.Fatalf("dry run must list oversized files and stay openrsync-compatible: %q", dry)
	}
}

// TestRsyncFiltersKeepFixturesAndDropExports runs the filter chain through the
// installed rsync (GNU rsync or openrsync) against a local tree, so the
// include patterns are checked by the matcher that applies them.
func TestRsyncFiltersKeepFixturesAndDropExports(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync not installed")
	}
	// path → transferred; tests/ and Tests/ live in different plugins because
	// the filesystem may fold case
	files := map[string]bool{
		"custom/plugins/Acme/src/Resources/fixtures/products.csv": true,
		"custom/plugins/Acme/src/Resources/import/prices.xlsx":    true,
		"custom/plugins/Acme/tests/data/orders.csv":               true,
		"custom/plugins/Legacy/Tests/data/orders.xlsx":            true,
		"custom/plugins/Acme/src/Resources/config/config.xml":     true,
		"src/Resources/fixtures/countries.csv":                    true,
		"src/AcmeBundle/Resources/fixtures/countries.xlsx":        true,
		"custom/plugins/Acme/import/customers.csv":                false,
		"custom/plugins/Acme/export.xlsx":                         false,
		"custom/plugins/Acme/src/Resources/dump.sql":              false,
		"files/Resources/export.csv":                              false,
		"public/catalogs/old.csv":                                 false,
	}
	src := t.TempDir()
	for p := range files {
		full := filepath.Join(src, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	args := append([]string{"-a", "--dry-run", "--out-format=%n"}, rsyncFilters([]string{"/public/catalogs/"})...)
	out, err := exec.Command("rsync", append(args, src+"/", t.TempDir()+"/")...).CombinedOutput()
	if err != nil {
		t.Fatalf("rsync: %v\n%s", err, out)
	}
	got := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		got[strings.TrimSpace(line)] = true
	}
	for p, want := range files {
		if got[p] != want {
			t.Errorf("%s transferred=%v, want %v\n%s", p, got[p], want, out)
		}
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".dockware-clone.yml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadProjectConfigNewFields(t *testing.T) {
	cfg, err := LoadProjectConfig(writeConfig(t, `
exclude: ["/public/catalogs/", "*.psd"]
max_code_size_mb: 800
orders_since_days: 90
skip_search_index: true
php: 8.3
node: 20
frontend:
  path: frontend
  command: npm run dev
`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.Exclude, ",") != "/public/catalogs/,*.psd" || cfg.MaxCodeSizeMB != 800 || cfg.OrdersSinceDays != 90 || !cfg.SkipSearchIndex {
		t.Errorf("sync fields: %+v", cfg)
	}
	if cfg.PHP != "8.3" || cfg.Node != "20" {
		t.Errorf("versions: php=%q node=%q", cfg.PHP, cfg.Node)
	}
	if cfg.Frontend.Image != "" || cfg.Frontend.Port != 3000 {
		t.Errorf("frontend image must stay empty (detected) and port default: %+v", cfg.Frontend)
	}
	if cfg, err := LoadProjectConfig(writeConfig(t, "max_code_size_mb: -1\n")); err != nil || cfg.MaxCodeSizeMB != -1 {
		t.Errorf("-1 disables the limit: %v %+v", err, cfg)
	}
}

func TestLoadProjectConfigReadsUnquotedNullKind(t *testing.T) {
	cfg, err := LoadProjectConfig(writeConfig(t, `
rules:
  - {table: customer, column: title, kind: null}
  - table: order
    column: custom_fields
    kind: ~
  - {table: user, column: store_token, kind: "null"}
  - {table: acme_x, column: mail, kind: email}
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Rule{
		{Table: "customer", Column: "title", Kind: KindNull},
		{Table: "order", Column: "custom_fields", Kind: KindNull},
		{Table: "user", Column: "store_token", Kind: KindNull},
		{Table: "acme_x", Column: "mail", Kind: KindEmail},
	}
	if fmt.Sprint(cfg.Rules) != fmt.Sprint(want) {
		t.Errorf("rules %+v, want %+v", cfg.Rules, want)
	}
}

func TestLoadProjectConfigRejectsBadValues(t *testing.T) {
	cases := []struct{ body, want string }{
		{"orders_since_dayz: 1\n", "orders_since_dayz"},
		{"orders_since_days: -1\n", "orders_since_days"},
		{"max_code_size_mb: -2\n", "max_code_size_mb"},
		{"php: 8\n", "php"},
		{"php: \"8.3.1\"\n", "php"},
		{"node: v20\n", "node"},
		{"exclude: [\"\"]\n", "exclude"},
		{"rules:\n  - {table: x, column: y, kind: nope}\n", "rule kind"},
		{"rules:\n  - {table: x, column: y, kinds: email}\n", "kinds"},
		{"rules:\n  - just a string\n", "valid config"},
		{"frontend: {path: web}\n", "frontend"},
		{"- not a map\n", "valid config"},
	}
	for _, c := range cases {
		_, err := LoadProjectConfig(writeConfig(t, c.body))
		var ce *Error
		if !errors.As(err, &ce) {
			t.Errorf("%q: %v is not an *Error", c.body, err)
			continue
		}
		if ce.Step != StepConfig || !strings.Contains(ce.Msg, ".dockware-clone.yml") || !strings.Contains(ce.Hint, "docs/clone.md") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v", c.body, err)
		}
	}
	_, err := LoadProjectConfig(filepath.Join(t.TempDir(), "missing.yml"))
	var ce *Error
	if !errors.As(err, &ce) || ce.Step != StepConfig || !strings.Contains(ce.Msg, "missing.yml") {
		t.Errorf("missing file: %v", err)
	}
}
