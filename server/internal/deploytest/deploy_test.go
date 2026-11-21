// Package deploytest runs the deploy/install.sh + deploy/install-interactive
// contract tests from `go test ./...` so CI needs no separate shell harness.
package deploytest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(wd, "deploy", "install.sh")); err == nil {
			return wd
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			t.Fatal("repo root not found")
		}
		wd = parent
	}
}

func runScript(t *testing.T, script string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(),
		"UFOT_PREFIX="+t.TempDir(),
		"UFOT_ETC="+t.TempDir(),
		"UFOT_STATE="+t.TempDir(),
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %s: %v (%s)", script, err, out)
	}
	return string(out), code
}

func TestInstallFlags(t *testing.T) {
	root := repoRoot(t)
	// bash -n syntax check on both scripts.
	for _, s := range []string{"deploy/install.sh", "deploy/install-interactive"} {
		cmd := exec.Command("bash", "-n", filepath.Join(root, s))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("bash -n %s: %v (%s)", s, err, out)
		}
	}
	// --help parses and exits 0, documents the new flags.
	out, code := runScript(t, filepath.Join(root, "deploy/install.sh"), "--help")
	if code != 0 || !strings.Contains(out, "--domain") ||
		!strings.Contains(out, "--admin-pass-file") || !strings.Contains(out, "--turn-server") ||
		!strings.Contains(out, "--lang") {
		t.Fatalf("install.sh --help: code=%d out=%s", code, out)
	}
	// unknown flag → exit 64.
	out, code = runScript(t, filepath.Join(root, "deploy/install.sh"), "--frobnicate")
	if code != 64 {
		t.Fatalf("unknown flag exit = %d (out=%s)", code, out)
	}
	// --lang override accepted by the parser (dry parse only: flags are checked
	// before the root check would fire, so we assert on the usage error path
	// for a *later* unknown flag, proving --lang was consumed).
	out, code = runScript(t, filepath.Join(root, "deploy/install.sh"), "--lang", "zh", "--frobnicate")
	if code != 64 || !strings.Contains(out, "--frobnicate") {
		t.Fatalf("--lang parse: code=%d out=%s", code, out)
	}
}

func TestInstallInteractiveDryRun(t *testing.T) {
	root := repoRoot(t)
	// Feed the full answer sequence via stdin (non-TTY): language, domain,
	// admin user, password twice, cold DSN (skip), TURN, confirm.
	input := "2\nchat.example.com\nroot\nSuperSecretPass14\nSuperSecretPass14\n\nturn:3478\ny\n"
	cmd := exec.Command("bash", filepath.Join(root, "deploy/install-interactive"), "--interactive-test")
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dry run: %v (%s)", err, out)
	}
	s := string(out)
	for _, want := range []string{
		"--domain chat.example.com", "--admin-user root", "--lang en", "--turn-server turn:3478",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, s)
		}
	}
	// password must never appear in dry-run output (only the pass-file path).
	if strings.Contains(s, "SuperSecretPass14") {
		t.Fatal("password leaked into dry-run output")
	}
}

func TestInstallInteractiveZh(t *testing.T) {
	root := repoRoot(t)
	input := "1\n\nroot\nSuperSecretPass14\nSuperSecretPass14\n\n\ny\n"
	cmd := exec.Command("bash", filepath.Join(root, "deploy/install-interactive"), "--interactive-test")
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dry run zh: %v (%s)", err, out)
	}
	if !strings.Contains(string(out), "--lang zh") || !strings.Contains(string(out), "安装摘要") {
		t.Fatalf("zh output wrong:\n%s", out)
	}
}
