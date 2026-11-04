package clone

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// CodeExcludes keeps runtime data, secrets, build output, uploads, archives
// and dumps off the laptop. vendor/ is installed by composer in the container
// and media is served from the remote public URL instead. Data directories
// are anchored to the project root so plugin code with the same name stays.
var CodeExcludes = []string{
	"/.env", "/.env.*", "/install.lock", "/auth.json", "/config/jwt/",
	".git/", ".idea/", ".DS_Store", "node_modules/", ".nuxt/", ".output/",
	"/vendor/", "/var/", "/files/", "/uploads/", "/import/", "/export/", "/backup*/", "/dump*/",
	"/public/media/", "/public/thumbnail/", "/public/sitemap/", "/public/theme/", "/public/bundles/", "/public/build/",
	"*.sql", "*.sql.zst", "*.gz", "*.zip", "*.tar", "*.tar.*", "*.tgz", "*.bak",
	"*.csv", "*.xlsx", "*.log", "*.pem", "*.key",
}

// codeIncludes come before CodeExcludes in the rsync filter chain: csv and
// xlsx files count as fixtures only where plugin and project code keep them,
// in a Resources/ or test directory below custom/ and src/. rsync's "**"
// spans slashes, so that directory may sit at any depth, e.g.
// custom/plugins/Acme/src/Resources/fixtures/products.csv; it never matches
// an empty path segment, hence the extra /src/Resources/ pattern. Exports a
// plugin keeps elsewhere (custom/plugins/Acme/import/customers.csv) stay on
// the server. The patterns only match files, so no directory is affected.
var codeIncludes = []string{
	"/custom/**/Resources/**.csv", "/custom/**/Resources/**.xlsx",
	"/custom/**/tests/**.csv", "/custom/**/tests/**.xlsx",
	"/custom/**/Tests/**.csv", "/custom/**/Tests/**.xlsx",
	"/src/Resources/**.csv", "/src/Resources/**.xlsx",
	"/src/**/Resources/**.csv", "/src/**/Resources/**.xlsx",
}

// codeMaxFileMB is the real run's --max-size in MiB (rsync's "m" suffix):
// single files above it are skipped. The dry run lists them like any other
// file and parseRsyncDryRun sets them apart; rsync's own skip notices need
// an option that openrsync (the macOS default) rejects.
const codeMaxFileMB = 50

// codeMaxFileBytes is codeMaxFileMB in bytes.
const codeMaxFileBytes = codeMaxFileMB * 1024 * 1024

// CodeSyncOptions tunes SyncCode.
type CodeSyncOptions struct {
	// Exclude adds rsync exclude patterns; they are checked before the built-in rules.
	Exclude []string
	// MaxBytes aborts before the transfer when the code on the server is larger; 0 = no preflight.
	MaxBytes int64
}

// SyncCode copies the project code via rsync. vendor/ is never copied because
// composer installs it in the container. With MaxBytes set, a dry run first
// measures the tree so that an oversized project fails with its largest
// directories instead of filling the disk.
func SyncCode(ctx context.Context, r Remote, dst string, o CodeSyncOptions, log io.Writer) error {
	for _, p := range o.Exclude {
		if !validExclude(p) {
			return &Error{Step: StepConfig, Msg: fmt.Sprintf("invalid exclude pattern %q", p),
				Hint: "exclude patterns are rsync patterns on one line, e.g. /public/catalogs/ or *.psd"}
		}
	}
	src := strings.TrimRight(r.Root, "/") + "/"
	if o.MaxBytes > 0 {
		size, err := measureCode(ctx, r, src, o.Exclude)
		if err != nil {
			return err
		}
		if err := checkCodeSize(size, o.MaxBytes); err != nil {
			return err
		}
		fmt.Fprintf(log, "  code: %d files, %s\n", size.Files, humanBytes(size.Bytes))
		if len(size.Skipped) > 0 {
			fmt.Fprintf(log, "  skipped (larger than %d MiB): %s\n", codeMaxFileMB, strings.Join(size.Skipped, ", "))
		}
	} else {
		fmt.Fprintf(log, "  files larger than %d MiB are skipped (size preflight disabled, not listed)\n", codeMaxFileMB)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return Fail(StepRsync, "cannot create "+dst, err, "")
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "rsync", rsyncArgs(r, src, dst, o.Exclude, false)...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return rsyncError(err, stderr.String())
	}
	return nil
}

// rsyncArgs builds the pull command. The real run drops files above
// codeMaxFileMB with --max-size; the dry run omits it and lists every file
// with its size instead. Filter order: user excludes, then the fixture
// includes, then the built-in excludes (first match wins).
func rsyncArgs(r Remote, src, dst string, exclude []string, dryRun bool) []string {
	ssh := []string{"ssh"}
	for _, a := range r.SSHArgs() {
		ssh = append(ssh, ShellQuote(a))
	}
	args := []string{"-a", "--compress", "--delete", "-e", strings.Join(ssh, " ")}
	if dryRun {
		args = append(args, "--dry-run", "--out-format=%l %n")
	} else {
		args = append(args, fmt.Sprintf("--max-size=%dm", codeMaxFileMB))
	}
	args = append(args, rsyncFilters(exclude)...)
	return append(args, r.Target+":"+src, dst+"/")
}

// rsyncFilters renders the filter chain: user excludes, then the fixture
// includes, then the built-in excludes (first match wins).
func rsyncFilters(exclude []string) []string {
	var args []string
	for _, p := range exclude {
		args = append(args, "--filter", "- "+strings.TrimSpace(p))
	}
	for _, p := range codeIncludes {
		args = append(args, "--filter", "+ "+p)
	}
	for _, p := range CodeExcludes {
		args = append(args, "--filter", "- "+p)
	}
	return args
}

// validExclude accepts one rsync pattern on a single line.
func validExclude(p string) bool {
	return strings.TrimSpace(p) != "" && !strings.ContainsAny(p, "\n\r")
}

// codeSize is what the dry run saw on the server.
type codeSize struct {
	Files   int
	Bytes   int64
	Dirs    []dirSize // largest first, up to three levels deep
	Skipped []string  // files above codeMaxFileMB, which the real run's --max-size drops
}

type dirSize struct {
	Dir   string
	Bytes int64
}

// measureCode runs the sync as a dry run against an empty directory (one ssh
// connection, only the file list crosses the wire) so every file is listed
// regardless of what is already on the laptop, including the ones the real
// run will skip for their size.
func measureCode(ctx context.Context, r Remote, src string, exclude []string) (codeSize, error) {
	tmp, err := os.MkdirTemp("", "dockware-clone-rsync-")
	if err != nil {
		return codeSize{}, Fail(StepRsync, "cannot create a temporary directory", err, "")
	}
	defer os.RemoveAll(tmp)
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "rsync", rsyncArgs(r, src, tmp, exclude, true)...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return codeSize{}, rsyncError(err, stderr.String())
	}
	return parseRsyncDryRun(stdout.String()), nil
}

// parseRsyncDryRun reads "--out-format=%l %n" lines (size, name). The "./"
// root and directories (trailing slash) carry no bytes; files above
// codeMaxFileBytes go to Skipped and stay out of the totals, as the real run
// does not transfer them.
func parseRsyncDryRun(out string) codeSize {
	var s codeSize
	dirs := map[string]int64{}
	for _, line := range strings.Split(out, "\n") {
		i := strings.IndexByte(line, ' ')
		if i < 1 || strings.HasSuffix(line, "/") {
			continue
		}
		n, err := strconv.ParseInt(line[:i], 10, 64)
		if err != nil {
			continue
		}
		name := line[i+1:]
		if n > codeMaxFileBytes {
			s.Skipped = append(s.Skipped, name)
			continue
		}
		s.Files++
		s.Bytes += n
		parts := strings.Split(name, "/")
		for d := 1; d < len(parts) && d <= 3; d++ {
			dirs[strings.Join(parts[:d], "/")+"/"] += n
		}
	}
	for dir, n := range dirs {
		s.Dirs = append(s.Dirs, dirSize{dir, n})
	}
	sort.Slice(s.Dirs, func(i, j int) bool {
		if s.Dirs[i].Bytes != s.Dirs[j].Bytes {
			return s.Dirs[i].Bytes > s.Dirs[j].Bytes
		}
		return s.Dirs[i].Dir < s.Dirs[j].Dir
	})
	return s
}

// checkCodeSize turns an oversized tree into an error that names the
// directories worth excluding.
func checkCodeSize(s codeSize, maxBytes int64) error {
	if s.Bytes <= maxBytes {
		return nil
	}
	var b strings.Builder
	b.WriteString("largest directories:\n")
	for i, d := range s.Dirs {
		if i == 10 {
			break
		}
		fmt.Fprintf(&b, "  %9s  %s\n", humanBytes(d.Bytes), d.Dir)
	}
	return &Error{
		Step:   StepRsync,
		Msg:    fmt.Sprintf("code on the server is %s, more than the %s limit", humanBytes(s.Bytes), humanBytes(maxBytes)),
		Output: b.String(),
		Hint:   "exclude them with exclude: in .dockware-clone.yml or --exclude <pattern>, raise --max-code-size, or use --code <local checkout>",
	}
}

var rsyncUnknownOption = regexp.MustCompile(`(?i)unknown option|unrecognized option|unknown argument`)

// rsyncError classifies a failed run. Exit 23 (partial transfer) is usually
// unreadable files; those paths are the useful part of the output.
func rsyncError(err error, stderr string) error {
	output := tailLines(strings.TrimSpace(stderr), 20)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 23 {
		if denied := deniedPaths(stderr, 10); len(denied) > 0 {
			output = strings.Join(denied, "\n")
		}
	}
	e := &Error{Step: StepRsync, Msg: "rsync from the server failed", Err: err, Output: output,
		Hint: hintFor(StepRsync, stderr+"\n"+err.Error())}
	if e.Hint == "" && rsyncUnknownOption.MatchString(stderr) {
		e.Hint = "the local rsync is too old or is openrsync (macOS default): install rsync 3.1 or newer (brew install rsync) and put it first in PATH"
	}
	return e
}

// deniedPaths keeps the "Permission denied" lines of the rsync output, at most limit.
func deniedPaths(stderr string, limit int) []string {
	var lines []string
	for _, l := range strings.Split(stderr, "\n") {
		if strings.Contains(l, "Permission denied") {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	if len(lines) > limit {
		lines = append(lines[:limit], fmt.Sprintf("… and %d more", len(lines)-limit))
	}
	return lines
}

// humanBytes formats a size in decimal units, matching the MB figures of the dump.
func humanBytes(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.0f kB", float64(n)/1e3)
	}
	return fmt.Sprintf("%d B", n)
}
