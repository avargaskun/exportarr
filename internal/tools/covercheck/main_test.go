package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/onedr0p/exportarr/internal/assert"
)

// Package a: 2 of 4 statements covered, one block by only one of two binaries; b: 1 of 1.
const twoPkgProfile = `mode: atomic
example.com/m/a/a.go:3.10,5.2 2 0
example.com/m/a/a.go:7.10,9.2 2 0
example.com/m/b/b.go:1.1,2.2 1 4
example.com/m/a/a.go:3.10,5.2 2 1
example.com/m/a/a.go:7.10,9.2 2 0
`

type result struct {
	code           int
	stdout, stderr string
}

func runFiles(t *testing.T, profile, floors string, extra ...string) result {
	t.Helper()
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "coverage.out")
	floorsPath := filepath.Join(dir, "floors.txt")
	assert.NoError(t, os.WriteFile(profilePath, []byte(profile), 0o600))
	assert.NoError(t, os.WriteFile(floorsPath, []byte(floors), 0o600))
	args := append([]string{"-profile", profilePath, "-floors", floorsPath}, extra...)
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

var spaces = regexp.MustCompile(` +`)

func rowFor(t *testing.T, stdout, pkg string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(pkg) + ` .*$`).FindString(stdout)
	if m == "" {
		t.Fatalf("no row for %s in:\n%s", pkg, stdout)
	}
	return spaces.ReplaceAllString(m, " ")
}

func TestRun(t *testing.T) {
	tests := []struct {
		name     string
		profile  string
		floors   string
		wantCode int
		wantRows map[string]string
		wantOut  []string
		wantErr  []string
	}{
		{
			name:     "duplicated block covered in one binary counts as covered",
			profile:  twoPkgProfile,
			floors:   "example.com/m/a 50\nexample.com/m/b 100\n",
			wantCode: exitOK,
			wantRows: map[string]string{
				"example.com/m/a": "example.com/m/a 4 50.0% 50 ok",
				"example.com/m/b": "example.com/m/b 1 100.0% 100 ok",
			},
			wantOut: []string{"PASS (packages: 2): 2 ok, 0 excluded, 0 below floor, 0 without floor, 0 stale"},
		},
		{
			name:     "fractional floor at the exact percentage passes and the table truncates",
			profile:  "mode: set\nexample.com/m/c/c.go:1.1,2.2 2 1\nexample.com/m/c/c.go:3.1,4.2 1 0\n",
			floors:   "example.com/m/c 66.66666666666667\n",
			wantCode: exitOK,
			wantRows: map[string]string{"example.com/m/c": "example.com/m/c 3 66.6% 66.66666666666667 ok"},
		},
		{
			name:     "floor just above the percentage fails",
			profile:  "mode: set\nexample.com/m/c/c.go:1.1,2.2 2 1\nexample.com/m/c/c.go:3.1,4.2 1 0\n",
			floors:   "example.com/m/c 66.667\n",
			wantCode: exitFail,
			wantRows: map[string]string{"example.com/m/c": "example.com/m/c 3 66.6% 66.667 BELOW FLOOR"},
		},
		{
			name:     "below floor",
			profile:  twoPkgProfile,
			floors:   "example.com/m/a 50.1\nexample.com/m/b 100\n",
			wantCode: exitFail,
			wantRows: map[string]string{"example.com/m/a": "example.com/m/a 4 50.0% 50.1 BELOW FLOOR"},
			wantOut:  []string{"FAIL (packages: 2): 1 ok, 0 excluded, 1 below floor, 0 without floor, 0 stale"},
		},
		{
			name:     "excluded package is not checked",
			profile:  twoPkgProfile,
			floors:   "example.com/m/a -\nexample.com/m/b 100\n",
			wantCode: exitOK,
			wantRows: map[string]string{"example.com/m/a": "example.com/m/a 4 50.0% - excluded"},
			wantOut:  []string{"1 ok, 1 excluded"},
		},
		{
			name:     "package without a floor line",
			profile:  twoPkgProfile,
			floors:   "example.com/m/a 0\n",
			wantCode: exitFail,
			wantRows: map[string]string{"example.com/m/b": "example.com/m/b 1 100.0% none NO FLOOR"},
			wantOut:  []string{"1 without floor"},
		},
		{
			name:     "floor line for a package missing from the profile",
			profile:  twoPkgProfile,
			floors:   "example.com/m/a 0\nexample.com/m/b 0\nexample.com/m/gone 10\n",
			wantCode: exitFail,
			wantRows: map[string]string{"example.com/m/gone": "example.com/m/gone - - 10 STALE"},
			wantOut:  []string{"1 stale"},
		},
		{
			name:     "excluded line for a package missing from the profile is stale",
			profile:  twoPkgProfile,
			floors:   "example.com/m/a 0\nexample.com/m/b 0\nexample.com/m/gone -\n",
			wantCode: exitFail,
			wantRows: map[string]string{"example.com/m/gone": "example.com/m/gone - - - STALE"},
		},
		{
			name:     "package with only empty blocks needs no floor",
			profile:  "mode: set\nexample.com/m/e/e.go:1.1,1.2 0 0\n",
			floors:   "",
			wantCode: exitOK,
			wantRows: map[string]string{"example.com/m/e": "example.com/m/e 0 - none no statements"},
		},
		{
			name:     "package with only empty blocks passes any floor",
			profile:  "mode: set\nexample.com/m/e/e.go:1.1,1.2 0 0\n",
			floors:   "example.com/m/e 100\n",
			wantCode: exitOK,
			wantRows: map[string]string{"example.com/m/e": "example.com/m/e 0 - 100 ok"},
		},
		{
			name:     "profile line too long to scan",
			profile:  "mode: set\n" + strings.Repeat("x", 70*1024) + "\n",
			wantCode: exitUsage,
			wantErr:  []string{"coverage.out: bufio.Scanner: token too long"},
		},
		{
			name:     "floors line too long to scan",
			profile:  twoPkgProfile,
			floors:   strings.Repeat("x", 70*1024) + "\n",
			wantCode: exitUsage,
			wantErr:  []string{"floors.txt: bufio.Scanner: token too long"},
		},
		{
			name:     "comments and blank lines",
			profile:  "\n" + twoPkgProfile + "\n\n",
			floors:   "# header\n\n   \n  # indented comment\nexample.com/m/a 50\n\texample.com/m/b\t100  \n",
			wantCode: exitOK,
		},
		{
			name:     "file path with a space",
			profile:  "mode: set\nexample.com/m/sp ace/f.go:1.1,2.2 3 1\n",
			wantCode: exitFail,
			wantRows: map[string]string{"example.com/m/sp ace": "example.com/m/sp ace 3 100.0% none NO FLOOR"},
		},
		{
			name:     "malformed profile line",
			profile:  "mode: set\nexample.com/m/a/a.go:1.1,2.2 1\n",
			floors:   "example.com/m/a 0\n",
			wantCode: exitUsage,
			wantErr:  []string{"coverage.out:2: malformed block line"},
		},
		{
			name:     "profile line without a position",
			profile:  "mode: set\nexample.com/m/a/a.go 1 1\n",
			wantCode: exitUsage,
			wantErr:  []string{"coverage.out:2: malformed block line"},
		},
		{
			name:     "profile line with a bad position",
			profile:  "mode: set\nexample.com/m/a/a.go:1.1-2.2 1 1\n",
			wantCode: exitUsage,
			wantErr:  []string{"coverage.out:2: malformed"},
		},
		{
			name:     "profile line with a single field",
			profile:  "mode: set\ngarbage\n",
			wantCode: exitUsage,
			wantErr:  []string{"coverage.out:2: malformed"},
		},
		{
			name:     "profile line with non-numeric statements",
			profile:  "mode: set\nexample.com/m/a/a.go:1.1,2.2 x 1\n",
			wantCode: exitUsage,
			wantErr:  []string{"coverage.out:2: malformed"},
		},
		{
			name:     "profile line with a negative count",
			profile:  "mode: set\nexample.com/m/a/a.go:1.1,2.2 1 -1\n",
			wantCode: exitUsage,
			wantErr:  []string{"coverage.out:2: malformed"},
		},
		{
			name:     "missing mode line",
			profile:  "example.com/m/a/a.go:1.1,2.2 1 1\n",
			wantCode: exitUsage,
			wantErr:  []string{"coverage.out:1: profile must start with a \"mode: \" line"},
		},
		{
			name:     "empty profile",
			profile:  "\n\n",
			wantCode: exitUsage,
			wantErr:  []string{"profile must start with a \"mode: \" line"},
		},
		{
			name:     "conflicting statement counts",
			profile:  "mode: set\nexample.com/m/a/a.go:1.1,2.2 1 1\nexample.com/m/a/a.go:1.1,2.2 2 0\n",
			wantCode: exitUsage,
			wantErr:  []string{"coverage.out:3: block example.com/m/a/a.go:1.1,2.2 has 2 statements, earlier 1"},
		},
		{
			name:     "malformed floors line",
			profile:  twoPkgProfile,
			floors:   "example.com/m/a 50\nexample.com/m/b\n",
			wantCode: exitUsage,
			wantErr:  []string{"floors.txt:2: want \"<import path> <percent|->\""},
		},
		{
			name:     "duplicate floor",
			profile:  twoPkgProfile,
			floors:   "example.com/m/a 50\n# again\nexample.com/m/a 40\n",
			wantCode: exitUsage,
			wantErr:  []string{"floors.txt:3: duplicate floor for example.com/m/a"},
		},
		{
			name:     "floor above 100",
			profile:  twoPkgProfile,
			floors:   "example.com/m/a 100.5\n",
			wantCode: exitUsage,
			wantErr:  []string{"floors.txt:1: floor \"100.5\" for example.com/m/a is not a number from 0 to 100"},
		},
		{
			name:     "negative floor",
			profile:  twoPkgProfile,
			floors:   "example.com/m/a -1\n",
			wantCode: exitUsage,
			wantErr:  []string{"floors.txt:1: floor \"-1\""},
		},
		{
			name:     "NaN floor",
			profile:  twoPkgProfile,
			floors:   "example.com/m/a NaN\n",
			wantCode: exitUsage,
			wantErr:  []string{"floors.txt:1: floor \"NaN\""},
		},
		{
			name:     "non-numeric floor",
			profile:  twoPkgProfile,
			floors:   "example.com/m/a high\n",
			wantCode: exitUsage,
			wantErr:  []string{"floors.txt:1: floor \"high\""},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := runFiles(t, tc.profile, tc.floors)
			assert.Equal(t, got.code, tc.wantCode, "stdout:\n"+got.stdout+"\nstderr:\n"+got.stderr)
			for pkg, want := range tc.wantRows {
				assert.Equal(t, rowFor(t, got.stdout, pkg), want)
			}
			for _, want := range tc.wantOut {
				assert.Contains(t, got.stdout, want)
			}
			for _, want := range tc.wantErr {
				assert.Contains(t, got.stderr, want)
			}
			if tc.wantCode == exitUsage {
				assert.Empty(t, got.stdout)
			} else {
				assert.Empty(t, got.stderr)
				assert.Contains(t, spaces.ReplaceAllString(got.stdout, " "), "PACKAGE STMTS COVERAGE FLOOR STATUS\n")
			}
		})
	}
}

func TestRun_RowOrder(t *testing.T) {
	got := runFiles(t,
		"mode: set\nexample.com/m/z/z.go:1.1,2.2 1 1\nexample.com/m/y/y.go:1.1,2.2 1 1\n",
		"example.com/m/z 0\nexample.com/m/y 0\nexample.com/m/x -\n")
	x := strings.Index(got.stdout, "example.com/m/x ")
	y := strings.Index(got.stdout, "example.com/m/y ")
	z := strings.Index(got.stdout, "example.com/m/z ")
	assert.True(t, x > 0 && x < y && y < z, got.stdout)
}

func TestRun_Usage(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "exists")
	assert.NoError(t, os.WriteFile(existing, []byte("mode: set\n"), 0o600))
	missing := filepath.Join(dir, "missing")

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "no flags", args: nil, wantErr: "usage: covercheck"},
		{name: "profile only", args: []string{"-profile", existing}, wantErr: "usage: covercheck"},
		{name: "floors only", args: []string{"-floors", existing}, wantErr: "usage: covercheck"},
		{name: "positional argument", args: []string{"-profile", existing, "-floors", existing, "extra"}, wantErr: "usage: covercheck"},
		{name: "unknown flag", args: []string{"-bogus"}, wantErr: "flag provided but not defined: -bogus"},
		{name: "missing profile file", args: []string{"-profile", missing, "-floors", existing}, wantErr: "covercheck: open " + missing},
		{name: "missing floors file", args: []string{"-profile", existing, "-floors", missing}, wantErr: "covercheck: open " + missing},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			assert.Equal(t, run(tc.args, &stdout, &stderr), exitUsage)
			assert.Contains(t, stderr.String(), tc.wantErr)
			assert.Empty(t, stdout.String())
		})
	}
}
