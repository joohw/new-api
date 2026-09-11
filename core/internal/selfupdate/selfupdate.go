package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/clovapi/switcher/internal/buildinfo"
	"github.com/clovapi/switcher/internal/config"
)

const defaultLatestURL = "https://downloads.clovapi.com/clovapi/latest.txt"

// Options controls `clovapi update`.
type Options struct {
	VersionTag string // empty = resolve latest.txt
	TargetPath string // empty = config CliBinPath
	InPlace    bool   // replace running executable (Unix only)
	CheckOnly  bool   // report availability without installing
}

// Result summarizes an update attempt.
type Result struct {
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version"`
	TargetPath     string `json:"target_path"`
	Updated        bool   `json:"updated"`
	UpToDate       bool   `json:"up_to_date"`
}

// ResolveTargetPath picks where an updated binary should be written.
func ResolveTargetPath(execPath string, inPlace bool) (string, error) {
	if v := strings.TrimSpace(os.Getenv("CLOVAPI_CLI_INSTALL_PATH")); v != "" {
		return v, nil
	}
	if inPlace {
		if runtime.GOOS == "windows" {
			return "", fmt.Errorf("--in-place is not supported on Windows; omit the flag to install into the user config bin directory")
		}
		p := strings.TrimSpace(execPath)
		if p == "" {
			return "", fmt.Errorf("cannot resolve running executable path")
		}
		return p, nil
	}
	return config.CliBinPath()
}

// FetchLatestVersion reads the release tag from downloads.clovapi.com/latest.txt (e.g. v0.1.11).
func FetchLatestVersion(ctx context.Context, client *http.Client) (string, error) {
	url := strings.TrimSpace(os.Getenv("CLOVAPI_CLI_LATEST_URL"))
	if url == "" {
		url = defaultLatestURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch latest version: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch latest version: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", err
	}
	tag := strings.TrimSpace(string(body))
	if tag == "" {
		return "", fmt.Errorf("latest version response was empty")
	}
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	return tag, nil
}

// Update downloads and installs the requested release.
func Update(ctx context.Context, execPath string, opts Options) (Result, error) {
	client := &http.Client{Timeout: 5 * time.Minute}
	current := buildinfo.VersionString()

	versionTag := strings.TrimSpace(opts.VersionTag)
	if versionTag != "" && !strings.HasPrefix(versionTag, "v") {
		versionTag = "v" + versionTag
	}
	if versionTag == "" {
		latest, err := FetchLatestVersion(ctx, client)
		if err != nil {
			return Result{CurrentVersion: current}, err
		}
		versionTag = latest
	}
	latest := strings.TrimPrefix(versionTag, "v")

	target, err := ResolveTargetPath(execPath, opts.InPlace)
	if err != nil {
		return Result{CurrentVersion: current, LatestVersion: latest}, err
	}
	if v := strings.TrimSpace(opts.TargetPath); v != "" {
		target = v
	}

	res := Result{
		CurrentVersion: current,
		LatestVersion:  latest,
		TargetPath:     target,
	}
	installed := readInstalledVersion(ctx, target, execPath, current)
	if current != "" && current != "dev" && current == latest && !opts.InPlace {
		if installed == latest {
			res.UpToDate = true
			return res, nil
		}
	}
	if opts.CheckOnly {
		res.UpToDate = installed == latest
		return res, nil
	}

	archiveName, archiveBytes, err := downloadReleaseArchive(ctx, client, versionTag)
	if err != nil {
		return res, err
	}
	binary, err := extractCLIBytes(archiveBytes, archiveName)
	if err != nil {
		return res, err
	}
	unlock, err := acquireInstallLock()
	if err != nil {
		return res, err
	}
	defer unlock()
	if err := installBinary(binary, target, execPath, latest); err != nil {
		return res, err
	}
	res.Updated = true
	return res, nil
}

func acquireInstallLock() (func(), error) {
	path, err := config.CliInstallLockPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	for i := 0; i < 100; i++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			return func() {
				_ = f.Close()
				_ = os.Remove(path)
			}, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("timed out waiting for install lock: %s", path)
}

func readInstalledVersion(ctx context.Context, target, execPath, current string) string {
	if sameInstalledBinary(target, execPath) {
		return current
	}
	// version.txt can describe a deferred Windows update that never replaced
	// the binary. Ask the target itself before declaring it up to date.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, target, "version").Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(strings.SplitN(string(data), "\n", 2)[0])
	if len(fields) != 2 || fields[0] != "clovapi" {
		return ""
	}
	return strings.TrimPrefix(fields[1], "v")
}

func sameInstalledBinary(target, execPath string) bool {
	target = filepath.Clean(strings.TrimSpace(target))
	execPath = filepath.Clean(strings.TrimSpace(execPath))
	return target != "" && execPath != "" && target == execPath
}

func writeVersionMeta(version string) error {
	path, err := config.CliVersionMetaPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.TrimSpace(version)+"\n"), 0o600)
}

func downloadReleaseArchive(ctx context.Context, client *http.Client, versionTag string) (string, []byte, error) {
	osName, archName, err := platformArch()
	if err != nil {
		return "", nil, err
	}
	artifactVersion := strings.TrimPrefix(strings.TrimSpace(versionTag), "v")
	ext := ".tar.gz"
	if runtime.GOOS == "windows" {
		ext = ".zip"
	}
	archiveName := fmt.Sprintf("clovapi_%s_%s_%s%s", artifactVersion, osName, archName, ext)

	var lastErr error
	for _, base := range releaseBaseURLs(versionTag) {
		checksumURL := strings.TrimRight(base, "/") + "/checksums.txt"
		archiveURL := strings.TrimRight(base, "/") + "/" + archiveName
		checksumBody, err := fetchBytes(ctx, client, checksumURL)
		if err != nil {
			lastErr = err
			continue
		}
		expected, err := parseChecksum(string(checksumBody), archiveName)
		if err != nil {
			lastErr = err
			continue
		}
		archiveBody, err := fetchBytes(ctx, client, archiveURL)
		if err != nil {
			lastErr = err
			continue
		}
		actual := sha256.Sum256(archiveBody)
		if expected != hex.EncodeToString(actual[:]) {
			lastErr = fmt.Errorf("checksum mismatch for %s", archiveName)
			continue
		}
		return archiveName, archiveBody, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no release sources succeeded")
	}
	return "", nil, lastErr
}

func releaseBaseURLs(versionTag string) []string {
	if base := strings.TrimSpace(os.Getenv("CLOVAPI_CLI_BASE_URL")); base != "" {
		return []string{strings.TrimRight(base, "/")}
	}
	tag := strings.TrimSpace(versionTag)
	if tag == "" {
		return nil
	}
	r2Base := strings.TrimSpace(os.Getenv("CLOVAPI_R2_BASE_URL"))
	if r2Base == "" {
		r2Base = "https://downloads.clovapi.com/clovapi/" + tag
	}
	return []string{
		strings.TrimRight(r2Base, "/"),
		fmt.Sprintf("https://github.com/joohw/clovapi/releases/download/%s", tag),
	}
}

func fetchBytes(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}

func parseChecksum(content, fileName string) (string, error) {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		if parts[len(parts)-1] == fileName {
			return parts[0], nil
		}
	}
	return "", fmt.Errorf("checksum not found for %s", fileName)
}

func platformArch() (osName, archName string, err error) {
	switch runtime.GOOS {
	case "darwin":
		osName = "darwin"
	case "linux":
		osName = "linux"
	case "windows":
		osName = "windows"
	default:
		return "", "", fmt.Errorf("unsupported platform %q", runtime.GOOS)
	}
	switch runtime.GOARCH {
	case "amd64":
		archName = "amd64"
	case "arm64":
		archName = "arm64"
	default:
		return "", "", fmt.Errorf("unsupported arch %q", runtime.GOARCH)
	}
	return osName, archName, nil
}

func extractCLIBytes(archive []byte, archiveName string) ([]byte, error) {
	if strings.HasSuffix(strings.ToLower(archiveName), ".zip") {
		return extractFromZip(archive)
	}
	return extractFromTarGz(archive)
}

func extractFromZip(data []byte) ([]byte, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	want := "clovapi.exe"
	for _, f := range r.File {
		if filepath.Base(f.Name) == want {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("%s not found in zip", want)
}

func extractFromTarGz(data []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.FileInfo().IsDir() {
			continue
		}
		if filepath.Base(hdr.Name) == "clovapi" {
			return io.ReadAll(tr)
		}
	}
	return nil, fmt.Errorf("clovapi not found in archive")
}
