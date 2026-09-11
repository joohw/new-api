//go:build windows

package selfupdate

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"github.com/clovapi/switcher/internal/config"
)

func installBinary(data []byte, targetPath, execPath, version string) error {
	targetPath = strings.TrimSpace(targetPath)
	if targetPath == "" {
		return fmt.Errorf("target path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(targetPath), ".clovapi-update-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanupTmp := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanupTmp()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanupTmp()
		return err
	}

	stopper := strings.TrimSpace(execPath)
	if stopper == "" {
		stopper = targetPath
	}
	stopProxyBeforeInstall(stopper)

	if sameInstalledBinary(targetPath, execPath) {
		return installBinaryDeferredSelfUpdate(tmpName, targetPath, stopper, version)
	}

	if err := replaceWindowsBinary(tmpName, targetPath, stopper); err == nil {
		return writeVersionMeta(version)
	}
	cleanupTmp()
	if err := installBinaryDeferredReplace(data, targetPath, stopper); err != nil {
		return err
	}
	return writeVersionMeta(version)
}

func stopProxyBeforeInstall(cliPath string) {
	cliPath = strings.TrimSpace(cliPath)
	if cliPath == "" {
		return
	}
	if _, err := os.Stat(cliPath); err != nil {
		return
	}
	cmd := exec.Command(cliPath, "proxy", "stop")
	cmd.Stdout = nil
	cmd.Stderr = nil
	_ = cmd.Run()
	time.Sleep(400 * time.Millisecond)
}

func replaceWindowsBinary(sourcePath, targetPath, cliPath string) error {
	for attempt := 0; attempt < 8; attempt++ {
		if _, err := os.Stat(targetPath); err == nil {
			_ = os.Remove(targetPath)
			if _, err := os.Stat(targetPath); err == nil {
				oldPath := targetPath + ".old"
				_ = os.Remove(oldPath)
				_ = os.Rename(targetPath, oldPath)
			}
		}
		if err := os.Rename(sourcePath, targetPath); err == nil {
			_ = os.Remove(targetPath + ".old")
			return nil
		}
		stopProxyBeforeInstall(cliPath)
		time.Sleep(time.Duration(300*(attempt+1)) * time.Millisecond)
	}
	return fmt.Errorf("replace locked binary %s", targetPath)
}

func installBinaryDeferredReplace(data []byte, targetPath, cliPath string) error {
	pendingPath := targetPath + ".new"
	if err := os.WriteFile(pendingPath, data, 0o755); err != nil {
		return err
	}
	if runDeferredWindowsReplace(targetPath, pendingPath, cliPath, 0) {
		return nil
	}
	_ = os.Remove(pendingPath)
	return fmt.Errorf("EPERM: operation not permitted, replace %q; stop the clovapi proxy and retry", targetPath)
}

func installBinaryDeferredSelfUpdate(tmpPath, targetPath, cliPath, version string) error {
	versionPath, err := config.CliVersionMetaPath()
	if err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	pendingPath := targetPath + ".new"
	_ = os.Remove(pendingPath)
	if err := os.Rename(tmpPath, pendingPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	pid := os.Getpid()
	if !runDeferredWindowsReplaceDetached(targetPath, pendingPath, cliPath, pid, versionPath, version) {
		_ = os.Remove(pendingPath)
		return fmt.Errorf("failed to schedule self-update for %q", targetPath)
	}
	return nil
}

func runDeferredWindowsReplace(targetPath, pendingPath, cliPath string, waitPID int) bool {
	script := deferredReplaceScript(targetPath, pendingPath, cliPath, waitPID, "", "")
	cmd := deferredPowerShellCommand(script)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return false
	}
	if _, err := os.Stat(targetPath); err != nil {
		return false
	}
	if _, err := os.Stat(pendingPath); err == nil {
		return false
	}
	return true
}

func runDeferredWindowsReplaceDetached(targetPath, pendingPath, cliPath string, waitPID int, versionPath, version string) bool {
	script := deferredReplaceScript(targetPath, pendingPath, cliPath, waitPID, versionPath, version)
	cmd := deferredPowerShellCommand(script)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return false
	}
	// The helper must outlive this process to replace its executable.
	_ = cmd.Process.Release()
	return true
}

func deferredPowerShellCommand(script string) *exec.Cmd {
	// -EncodedCommand accepts UTF-16LE and avoids Windows command-line quote
	// parsing changing the script or paths containing non-ASCII characters.
	units := utf16.Encode([]rune(script))
	data := make([]byte, len(units)*2)
	for i, unit := range units {
		binary.LittleEndian.PutUint16(data[i*2:], unit)
	}
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", base64.StdEncoding.EncodeToString(data))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd
}

func powerShellLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func deferredReplaceScript(targetPath, pendingPath, cliPath string, waitPID int, versionPath, version string) string {
	waitBlock := ""
	if waitPID > 0 {
		waitBlock = fmt.Sprintf(
			"Wait-Process -Id %d -ErrorAction SilentlyContinue\nStart-Sleep -Milliseconds 300\n",
			waitPID,
		)
	}
	writeVersion := ""
	if versionPath != "" {
		writeVersion = fmt.Sprintf("[System.IO.File]::WriteAllText(%s, %s + [Environment]::NewLine)", powerShellLiteral(versionPath), powerShellLiteral(strings.TrimSpace(version)))
	}
	return strings.Join([]string{
		"$ErrorActionPreference = 'Stop'",
		"$target = " + powerShellLiteral(targetPath),
		"$pending = " + powerShellLiteral(pendingPath),
		"$cli = " + powerShellLiteral(cliPath),
		waitBlock,
		"$installed = $false",
		"for ($i = 0; $i -lt 40; $i++) {",
		"  try {",
		"    if (Test-Path -LiteralPath $cli) { try { & $cli proxy stop 2>$null | Out-Null } catch {} }",
		"    Start-Sleep -Milliseconds 300",
		"    if (Test-Path -LiteralPath $target) { Remove-Item -LiteralPath $target -Force -ErrorAction Stop }",
		"    Move-Item -LiteralPath $pending -Destination $target -Force -ErrorAction Stop",
		"    Remove-Item -LiteralPath ($target + '.old') -Force -ErrorAction SilentlyContinue",
		"    $installed = $true",
		"    break",
		"  } catch {",
		"    Start-Sleep -Milliseconds 500",
		"  }",
		"}",
		"if (-not $installed) { exit 1 }",
		writeVersion,
		"exit 0",
	}, "\n")
}
