package clone

import (
	"os"
	"path/filepath"
	"testing"
)

func writeNodeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetectNodeVersionParsesSpecs(t *testing.T) {
	cases := []struct{ spec, want string }{
		{"v20.11.1", "20"},
		{"20", "20"},
		{"20.x", "20"},
		{"^20.1", "20"},
		{"~20", "20"},
		{"=20", "20"},
		{"lts/iron", "20"},
		{"lts/hydrogen", "18"},
		{"lts/jod", "22"},
		{"lts/gallium", "16"},
		{"lts/*", ""},
		{"lts", ""},
		{"node", ""},
		{">=18 <21", "20"},
		{">= 18 < 21.0.0", "20"},
		{">=18 <=20", "20"},
		{">=18", ""},
		{">18", ""},
		{"18 || 20", "20"},
		{"^18 || ^20", "20"},
		{"18 - 20", "20"},
		{"", ""},
		{"# pinned for the storefront build\n20\n", "20"},
	}
	for _, c := range cases {
		t.Run(c.spec, func(t *testing.T) {
			dir := t.TempDir()
			writeNodeFile(t, dir, ".nvmrc", c.spec)
			if got := DetectNodeVersion(dir); got != c.want {
				t.Errorf("DetectNodeVersion(.nvmrc %q) = %q, want %q", c.spec, got, c.want)
			}
		})
	}
}

func TestDetectNodeVersionPackageJSON(t *testing.T) {
	cases := []struct{ name, pkg, want string }{
		{"engines", `{"engines": {"node": ">=18 <21"}}`, "20"},
		{"volta beats engines", `{"volta": {"node": "20.11.1"}, "engines": {"node": "18"}}`, "20"},
		{"open engines range is skipped", `{"engines": {"node": ">=18"}}`, ""},
		{"no node field", `{"name": "x"}`, ""},
		{"invalid json is skipped", `{"engines": `, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeNodeFile(t, dir, "package.json", c.pkg)
			if got := DetectNodeVersion(dir); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestDetectNodeVersionPrecedence(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	writeNodeFile(t, second, ".nvmrc", "22")

	if got := DetectNodeVersion(first, second); got != "22" {
		t.Errorf("empty first dir must fall through to the second: got %q", got)
	}

	writeNodeFile(t, first, "package.json", `{"engines": {"node": "18"}}`)
	if got := DetectNodeVersion(first, second); got != "18" {
		t.Errorf("first dir wins over the second: got %q, want 18", got)
	}

	writeNodeFile(t, first, ".node-version", "16")
	if got := DetectNodeVersion(first, second); got != "16" {
		t.Errorf(".node-version beats package.json: got %q, want 16", got)
	}

	writeNodeFile(t, first, ".nvmrc", "20")
	if got := DetectNodeVersion(first, second); got != "20" {
		t.Errorf(".nvmrc beats .node-version and package.json: got %q, want 20", got)
	}

	writeNodeFile(t, first, ".nvmrc", "lts/*")
	if got := DetectNodeVersion(first, second); got != "16" {
		t.Errorf("an undecidable .nvmrc falls through to the next source: got %q, want 16", got)
	}

	if got := DetectNodeVersion(filepath.Join(first, "missing")); got != "" {
		t.Errorf("missing dir must yield \"\", got %q", got)
	}
	if got := DetectNodeVersion(); got != "" {
		t.Errorf("no dirs must yield \"\", got %q", got)
	}
}
