package clone

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// DetectNodeVersion returns the node major version the first of dirs pins
// via .nvmrc, .node-version or package.json (volta.node, then engines.node).
// Sources that name no single major (lts/*, node, >=18) are skipped; ""
// when nothing usable is found.
func DetectNodeVersion(dirs ...string) string {
	for _, dir := range dirs {
		for _, spec := range nodeVersionSpecs(dir) {
			if major := nodeSpecMajor(spec); major != "" {
				return major
			}
		}
	}
	return ""
}

// nodeVersionSpecs reads the raw version specs of dir in precedence order;
// missing or unreadable files contribute nothing.
func nodeVersionSpecs(dir string) []string {
	var specs []string
	for _, name := range []string{".nvmrc", ".node-version"} {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			specs = append(specs, string(data))
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		var pkg struct {
			Volta struct {
				Node string `json:"node"`
			} `json:"volta"`
			Engines struct {
				Node string `json:"node"`
			} `json:"engines"`
		}
		if json.Unmarshal(data, &pkg) == nil {
			specs = append(specs, pkg.Volta.Node, pkg.Engines.Node)
		}
	}
	return specs
}

// ltsMajors maps nvm's lts/<codename> aliases to majors.
var ltsMajors = map[string]string{
	"fermium":  "14",
	"gallium":  "16",
	"hydrogen": "18",
	"iron":     "20",
	"jod":      "22",
}

// comparatorSpace joins an operator to the version it precedes (">= 18" → ">=18").
var comparatorSpace = regexp.MustCompile(`([<>]=?|=|\^|~)\s+`)

// nodeSpecMajor extracts the major from a version spec as found in .nvmrc,
// .node-version or package.json: exact versions and caret/tilde/x ranges give
// their major, lts/<codename> its release, "a || b" the highest alternative
// and bounded ranges the highest major below the upper bound. Specs without
// a decidable major ("lts/*", "node", ">=18") yield "".
func nodeSpecMajor(spec string) string {
	spec = strings.ToLower(firstLine(spec))
	if name := strings.TrimPrefix(spec, "lts/"); name != spec {
		return ltsMajors[name]
	}
	best := ""
	for _, alt := range strings.Split(spec, "||") {
		if m := rangeMajor(alt); m != "" && (best == "" || majorInt(m) > majorInt(best)) {
			best = m
		}
	}
	return best
}

// rangeMajor resolves one alternative of a spec (no "||").
func rangeMajor(r string) string {
	tokens := strings.Fields(comparatorSpace.ReplaceAllString(r, "$1"))
	switch {
	case len(tokens) == 0:
		return ""
	case len(tokens) == 3 && tokens[1] == "-":
		return majorOf(tokens[2])
	case len(tokens) == 1 && tokens[0][0] != '<' && tokens[0][0] != '>':
		return majorOf(tokens[0])
	}
	// comparators: the upper bound decides; without one the range is open-ended
	upper := ""
	for _, t := range tokens {
		if t[0] != '<' {
			continue
		}
		v := strings.TrimLeft(t, "<=")
		major := majorOf(v)
		if major == "" {
			return ""
		}
		if !strings.HasPrefix(t, "<=") && zeroBelowMajor(v) {
			// "<21" excludes 21 itself; "<21.5" still allows 21.0
			major = strconv.Itoa(majorInt(major) - 1)
		}
		upper = major
	}
	return upper
}

// majorOf returns the leading number of a version like "v20.11.1", "^20.1"
// or "20.x"; "" when it starts with none ("x", "*", "node").
func majorOf(v string) string {
	v = strings.TrimLeft(v, "v=^~")
	i := 0
	for i < len(v) && v[i] >= '0' && v[i] <= '9' {
		i++
	}
	return v[:i]
}

// zeroBelowMajor reports whether everything after the major is absent, zero
// or a wildcard ("21", "21.0.0", "21.x").
func zeroBelowMajor(v string) bool {
	parts := strings.Split(strings.TrimLeft(v, "v=^~"), ".")
	for _, p := range parts[1:] {
		if p != "" && p != "0" && p != "x" && p != "*" {
			return false
		}
	}
	return true
}

// firstLine returns the first non-empty line of s with a trailing "# comment"
// removed, which is how .nvmrc files are written.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func majorInt(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
