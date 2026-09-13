// Command covercheck compares per-package statement coverage from a Go cover
// profile against the floors listed in a floors file.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
)

const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

const (
	statusOK       = "ok"
	statusExcluded = "excluded"
	statusBelow    = "BELOW FLOOR"
	statusNoFloor  = "NO FLOOR"
	statusStale    = "STALE"
	statusEmpty    = "no statements"
)

var blockPos = regexp.MustCompile(`^\d+\.\d+,\d+\.\d+$`)

type block struct {
	numStmt int
	covered bool
}

type pkgCoverage struct {
	total, covered int
}

func (p pkgCoverage) percent() float64 {
	if p.total == 0 {
		return 100
	}
	return 100 * float64(p.covered) / float64(p.total)
}

// display truncates rather than rounds, so a floor read off the table never exceeds the coverage.
func (p pkgCoverage) display() string {
	if p.total == 0 {
		return "-"
	}
	tenths := 1000 * p.covered / p.total
	return fmt.Sprintf("%d.%d%%", tenths/10, tenths%10)
}

type floor struct {
	excluded bool
	min      float64
}

type row struct {
	pkg    string
	cov    *pkgCoverage
	floor  *floor
	status string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("covercheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profilePath := fs.String("profile", "", "Go cover profile to check (required)")
	floorsPath := fs.String("floors", "", "file of \"<import path> <percent|->\" lines (required)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *profilePath == "" || *floorsPath == "" || fs.NArg() > 0 {
		_, _ = fmt.Fprintln(stderr, "usage: covercheck -profile <cover profile> -floors <floors file>")
		return exitUsage
	}

	pkgs, err := readFile(*profilePath, parseProfile)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "covercheck: %v\n", err)
		return exitUsage
	}
	floors, err := readFile(*floorsPath, parseFloors)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "covercheck: %v\n", err)
		return exitUsage
	}

	if !report(stdout, evaluate(pkgs, floors)) {
		return exitFail
	}
	return exitOK
}

func readFile[T any](name string, parse func(io.Reader, string) (T, error)) (T, error) {
	f, err := os.Open(name) //nolint:gosec // path from a CLI flag
	if err != nil {
		var zero T
		return zero, err
	}
	defer func() { _ = f.Close() }()
	return parse(f, name)
}

func parseProfile(r io.Reader, name string) (map[string]*pkgCoverage, error) {
	blocks := map[string]block{}
	sawMode := false
	lineNo := 0
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if !sawMode {
			if !strings.HasPrefix(line, "mode: ") {
				return nil, fmt.Errorf("%s:%d: profile must start with a \"mode: \" line", name, lineNo)
			}
			sawMode = true
			continue
		}
		key, numStmt, count, err := parseBlock(line)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", name, lineNo, err)
		}
		prev, seen := blocks[key]
		if seen && prev.numStmt != numStmt {
			return nil, fmt.Errorf("%s:%d: block %s has %d statements, earlier %d", name, lineNo, key, numStmt, prev.numStmt)
		}
		blocks[key] = block{numStmt: numStmt, covered: prev.covered || count > 0}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if !sawMode {
		return nil, fmt.Errorf("%s: profile must start with a \"mode: \" line", name)
	}

	pkgs := map[string]*pkgCoverage{}
	for key, b := range blocks {
		pkg := path.Dir(key[:strings.LastIndexByte(key, ':')])
		p := pkgs[pkg]
		if p == nil {
			p = &pkgCoverage{}
			pkgs[pkg] = p
		}
		p.total += b.numStmt
		if b.covered {
			p.covered += b.numStmt
		}
	}
	return pkgs, nil
}

func parseBlock(line string) (key string, numStmt int, count int64, err error) {
	key, numStmt, count, ok := splitBlock(line)
	if !ok {
		return "", 0, 0, fmt.Errorf("malformed block line %q", line)
	}
	return key, numStmt, count, nil
}

func splitBlock(line string) (key string, numStmt int, count int64, ok bool) {
	rest, countField, ok := cutLast(line)
	if !ok {
		return "", 0, 0, false
	}
	key, stmtField, ok := cutLast(rest)
	if !ok {
		return "", 0, 0, false
	}
	numStmt, err := strconv.Atoi(stmtField)
	if err != nil || numStmt < 0 {
		return "", 0, 0, false
	}
	count, err = strconv.ParseInt(countField, 10, 64)
	if err != nil || count < 0 {
		return "", 0, 0, false
	}
	colon := strings.LastIndexByte(key, ':')
	if colon <= 0 || !blockPos.MatchString(key[colon+1:]) {
		return "", 0, 0, false
	}
	return key, numStmt, count, true
}

func cutLast(s string) (before, after string, ok bool) {
	i := strings.LastIndexByte(s, ' ')
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

func parseFloors(r io.Reader, name string) (map[string]floor, error) {
	floors := map[string]floor{}
	lineNo := 0
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("%s:%d: want \"<import path> <percent|->\", got %q", name, lineNo, line)
		}
		pkg, value := fields[0], fields[1]
		if _, dup := floors[pkg]; dup {
			return nil, fmt.Errorf("%s:%d: duplicate floor for %s", name, lineNo, pkg)
		}
		if value == "-" {
			floors[pkg] = floor{excluded: true}
			continue
		}
		pct, err := strconv.ParseFloat(value, 64)
		if err != nil || !(pct >= 0 && pct <= 100) {
			return nil, fmt.Errorf("%s:%d: floor %q for %s is not a number from 0 to 100", name, lineNo, value, pkg)
		}
		floors[pkg] = floor{min: pct}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return floors, nil
}

func evaluate(pkgs map[string]*pkgCoverage, floors map[string]floor) []row {
	names := slices.Collect(maps.Keys(pkgs))
	for pkg := range floors {
		if _, ok := pkgs[pkg]; !ok {
			names = append(names, pkg)
		}
	}
	slices.Sort(names)

	rows := make([]row, 0, len(names))
	for _, pkg := range names {
		r := row{pkg: pkg, cov: pkgs[pkg]}
		if f, ok := floors[pkg]; ok {
			r.floor = &f
		}
		switch {
		case r.cov == nil:
			r.status = statusStale
		case r.floor != nil && r.floor.excluded:
			r.status = statusExcluded
		case r.floor == nil && r.cov.total == 0:
			r.status = statusEmpty
		case r.floor == nil:
			r.status = statusNoFloor
		case r.cov.percent()+1e-9 >= r.floor.min:
			r.status = statusOK
		default:
			r.status = statusBelow
		}
		rows = append(rows, r)
	}
	return rows
}

func report(w io.Writer, rows []row) bool {
	counts := map[string]int{}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PACKAGE\tSTMTS\tCOVERAGE\tFLOOR\tSTATUS")
	for _, r := range rows {
		counts[r.status]++
		stmts, cov, fl := "-", "-", "none"
		if r.cov != nil {
			stmts = strconv.Itoa(r.cov.total)
			cov = r.cov.display()
		}
		switch {
		case r.floor == nil:
		case r.floor.excluded:
			fl = "-"
		default:
			fl = strconv.FormatFloat(r.floor.min, 'f', -1, 64)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.pkg, stmts, cov, fl, r.status)
	}
	_ = tw.Flush()

	ok := counts[statusBelow]+counts[statusNoFloor]+counts[statusStale] == 0
	verdict := "PASS"
	if !ok {
		verdict = "FAIL"
	}
	_, _ = fmt.Fprintf(w, "%s (packages: %d): %d ok, %d excluded, %d below floor, %d without floor, %d stale\n",
		verdict, len(rows), counts[statusOK], counts[statusExcluded], counts[statusBelow], counts[statusNoFloor], counts[statusStale])
	return ok
}
