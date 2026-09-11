//go:build windows

package selfupdate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clovapi/switcher/internal/config"
)

func TestDeferredWindowsReplace(t *testing.T) {
	for _, name := range []string{"plain", "space [literal] $value 'quote' 中文"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), name)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(dir, "clovapi.exe")
			pending := target + ".new"
			if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(pending, []byte("new"), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			script := deferredReplaceScript(target, pending, filepath.Join(dir, "missing.exe"), 0, "", "")
			command := deferredPowerShellCommand(script)
			cmd := exec.CommandContext(ctx, command.Path, command.Args[1:]...)
			cmd.SysProcAttr = command.SysProcAttr
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("replacement failed: %v\n%s", err, output)
			}
			got, err := os.ReadFile(target)
			if err != nil || string(got) != "new" {
				t.Fatalf("installed = %q, err = %v", got, err)
			}
			if _, err := os.Stat(pending); !os.IsNotExist(err) {
				t.Fatalf("pending file still exists: %v", err)
			}
		})
	}
}

func TestDeferredWindowsSelfUpdateWritesVersionAfterReplacement(t *testing.T) {
	dir := t.TempDir()
	config.SetDirOverride(dir)
	t.Cleanup(func() { config.SetDirOverride("") })
	target, err := config.CliBinPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	pending := target + ".new"
	versionPath, err := config.CliVersionMetaPath()
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{target: "old", pending: "new", versionPath: "0.2.18\n"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A disposable child stands in for the updating process. No clovapi
	// executable is launched and the helper's CLI path does not exist.
	holder := deferredPowerShellCommand("Start-Sleep -Seconds 30")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill() })
	if !runDeferredWindowsReplaceDetached(target, pending, filepath.Join(dir, "missing.exe"), holder.Process.Pid, versionPath, "0.2.20") {
		t.Fatal("failed to start deferred updater")
	}
	time.Sleep(700 * time.Millisecond)
	for path, want := range map[string]string{target: "old", versionPath: "0.2.18\n"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("updated %s before process exit: %q, %v", path, got, err)
		}
	}
	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := os.ReadFile(versionPath)
		if strings.TrimSpace(string(got)) == "0.2.20" {
			installed, err := os.ReadFile(target)
			if err != nil || string(installed) != "new" {
				t.Fatalf("version updated before binary: %q, %v", installed, err)
			}
			if _, err := os.Stat(pending); !os.IsNotExist(err) {
				t.Fatalf("pending binary remains after success: %v", err)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("deferred update did not finish after process exit")
}
