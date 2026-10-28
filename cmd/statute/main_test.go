package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandHelp(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--help"},
		{"-h"},
		{"migrate", "--help"},
		{"migrate", "traefik", "--help"},
		{"migrate", "traefik", "-h"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != 0 {
				t.Fatalf("help exited %d: %s", code, &stderr)
			}
			if stderr.Len() != 0 {
				t.Fatalf("help wrote to stderr: %s", &stderr)
			}
			if !strings.Contains(stdout.String(), "Usage: statute") || !strings.Contains(stdout.String(), "--help") {
				t.Fatalf("missing command help: %s", &stdout)
			}
			if len(args) == 3 && !strings.Contains(stdout.String(), "--compose") {
				t.Fatalf("missing migration flags: %s", &stdout)
			}
		})
	}
}

func TestCommandHelpDoesNotReadInputsOrCreateArtifacts(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	args := []string{
		"migrate", "traefik", "--static", filepath.Join(dir, "missing.yml"),
		"--out", filepath.Join(dir, "main.go"), "--report", filepath.Join(dir, "report.json"), "--help",
	}
	if code := run(args, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("help exited %d: %s", code, &stderr)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("help created artifacts: %v, %v", entries, err)
	}
}

func TestCommandInvalidInvocationDoesNotCreateArtifacts(t *testing.T) {
	dir := t.TempDir()
	out, report := filepath.Join(dir, "main.go"), filepath.Join(dir, "report.json")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"command", []string{"unknown"}, "unexpected argument"},
		{"subcommand", []string{"migrate", "unknown"}, "unexpected argument"},
		{"flag", []string{"migrate", "traefik", "--out", out, "--report", report, "--unknown"}, "unknown flag"},
		{"positional", []string{"migrate", "traefik", "--out", out, "--report", report, "unexpected"}, "unexpected argument"},
		{"missing-output", []string{"migrate", "traefik", "--static", "missing.yml", "--out", out}, "--report"},
		{"missing-input", []string{"migrate", "traefik", "--out", out, "--report", report}, "provide at least one"},
		{"same-output", []string{"migrate", "traefik", "--static", "missing.yml", "--out", out, "--report", out}, "paths must be distinct"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, &stdout, &stderr); code != 2 {
				t.Fatalf("invalid invocation exited %d: %s", code, &stderr)
			}
			if stdout.Len() != 0 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stdout=%q stderr=%q", &stdout, &stderr)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("invalid invocation created artifacts: %v, %v", entries, err)
			}
		})
	}
}

func TestCommandRepeatableInputPathsRemainLiteral(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first,static.yml")
	second := filepath.Join(dir, "$SECOND.yml")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("entryPoints: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("SECOND", "must-not-expand")
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"migrate", "traefik", "--static", first, "--static", second,
		"--out", filepath.Join(dir, "main.go"), "--report", filepath.Join(dir, "report.json"),
	}, &stdout, &stderr)
	if code == 2 {
		t.Fatalf("literal input paths rejected: %s", &stderr)
	}
	if !strings.Contains(stdout.String(), first) || !strings.Contains(stdout.String(), second) {
		t.Fatalf("repeatable inputs not both audited: %s", &stdout)
	}
}

func TestCommandWritesGuardedArtifactsAndRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "static.yml")
	out := filepath.Join(dir, "main.go")
	report := filepath.Join(dir, "report.json")
	if err := os.WriteFile(input, []byte("entryPoints:\n  web:\n    address: ':8080'\nsecret: DO_NOT_ECHO\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"migrate", "traefik", "--static", input, "--out", out, "--report", report}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 1 {
		t.Fatalf("code=%d stderr=%s", code, &stderr)
	}
	if strings.Contains(stdout.String()+stderr.String(), "DO_NOT_ECHO") {
		t.Fatal("source value leaked")
	}
	before := readTestFile(t, out)
	if !bytes.Contains(before, []byte("panic(")) {
		t.Fatal("incomplete source could serve")
	}
	if code := run(args, &stdout, &stderr); code != 2 {
		t.Fatal("existing output overwritten")
	}
	after := readTestFile(t, out)
	if !bytes.Equal(before, after) {
		t.Fatal("existing source changed")
	}
	info, err := os.Stat(report)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatal("report permissions not private")
	}
}

func TestCommandParseFailureRemovesBothReservedOutputs(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "bad.yml")
	out := filepath.Join(dir, "main.go")
	report := filepath.Join(dir, "report.json")
	if err := os.WriteFile(input, []byte("a: SECRET: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"migrate", "traefik", "--static", input, "--out", out, "--report", report}, &stdout, &stderr); code != 2 {
		t.Fatal(code)
	}
	if strings.Contains(stderr.String(), "SECRET") {
		t.Fatal("parser leaked input")
	}
	for _, path := range []string{out, report} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("partial artifact remains: %s", path)
		}
	}
}

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	// #nosec G304 -- Test-owned artifact path under t.TempDir.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
