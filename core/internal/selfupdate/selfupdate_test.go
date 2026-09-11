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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/clovapi/switcher/internal/buildinfo"
	cfgpkg "github.com/clovapi/switcher/internal/config"
)

func TestUpdateInstallsIntoConfigBin(t *testing.T) {
	dir := t.TempDir()
	cfgpkg.SetDirOverride(dir)
	t.Cleanup(func() { cfgpkg.SetDirOverride("") })

	binary := []byte("#!/bin/sh\necho ok\n")
	osName, archName, err := platformArch()
	if err != nil {
		t.Fatal(err)
	}
	archiveName := fmt.Sprintf("clovapi_0.1.99_%s_%s.tar.gz", osName, archName)
	archiveBytes := tarGzWithFile(t, "clovapi", binary)
	if runtime.GOOS == "windows" {
		archiveName = fmt.Sprintf("clovapi_0.1.99_%s_%s.zip", osName, archName)
		archiveBytes = zipWithFile(t, "clovapi.exe", binary)
	}
	sum := sha256.Sum256(archiveBytes)
	checksums := hex.EncodeToString(sum[:]) + "  " + archiveName + "\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest.txt":
			_, _ = w.Write([]byte("v0.1.99"))
		case "/checksums.txt":
			_, _ = w.Write([]byte(checksums))
		case "/" + archiveName:
			_, _ = w.Write(archiveBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("CLOVAPI_CLI_LATEST_URL", srv.URL+"/latest.txt")
	t.Setenv("CLOVAPI_CLI_BASE_URL", srv.URL)

	res, err := Update(context.Background(), "", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Updated {
		t.Fatalf("expected update, got %+v", res)
	}
	target, err := cfgpkg.CliBinPath()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, binary) {
		t.Fatalf("installed binary mismatch")
	}
	versionPath, err := cfgpkg.CliVersionMetaPath()
	if err != nil {
		t.Fatal(err)
	}
	version, err := os.ReadFile(versionPath)
	if err != nil || strings.TrimSpace(string(version)) != "0.1.99" {
		t.Fatalf("installed version = %q, err = %v", version, err)
	}
}

func zipWithFile(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, err := w.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestUpdateCheckOnly(t *testing.T) {
	cfgpkg.SetDirOverride(t.TempDir())
	t.Cleanup(func() { cfgpkg.SetDirOverride("") })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("v9.9.9"))
	}))
	defer srv.Close()
	t.Setenv("CLOVAPI_CLI_LATEST_URL", srv.URL)

	res, err := Update(context.Background(), "", Options{CheckOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.LatestVersion != "9.9.9" {
		t.Fatalf("latest = %q", res.LatestVersion)
	}
}

func TestUpdateCheckDoesNotTrustStaleVersionMeta(t *testing.T) {
	cfgpkg.SetDirOverride(t.TempDir())
	t.Cleanup(func() { cfgpkg.SetDirOverride("") })
	previousVersion := buildinfo.Version
	buildinfo.Version = "0.2.18"
	t.Cleanup(func() { buildinfo.Version = previousVersion })
	if err := writeVersionMeta("0.2.20"); err != nil {
		t.Fatal(err)
	}
	target, err := cfgpkg.CliBinPath()
	if err != nil {
		t.Fatal(err)
	}
	res, err := Update(context.Background(), target, Options{CheckOnly: true, VersionTag: "0.2.20"})
	if err != nil {
		t.Fatal(err)
	}
	if res.UpToDate {
		t.Fatal("stale metadata hid an update required by the running binary")
	}
}

func TestUpdateDoesNotSkipMissingTargetWithStaleVersionMeta(t *testing.T) {
	cfgpkg.SetDirOverride(t.TempDir())
	t.Cleanup(func() { cfgpkg.SetDirOverride("") })
	previousVersion := buildinfo.Version
	buildinfo.Version = "0.2.20"
	t.Cleanup(func() { buildinfo.Version = previousVersion })
	if err := writeVersionMeta("0.2.20"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "offline fixture", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	t.Setenv("CLOVAPI_CLI_BASE_URL", srv.URL)
	res, err := Update(context.Background(), filepath.Join(t.TempDir(), "other-clovapi"), Options{VersionTag: "0.2.20"})
	if err == nil || res.UpToDate {
		t.Fatalf("expected a download attempt for missing target, got %+v, %v", res, err)
	}
}

func tarGzWithFile(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0o755,
		Size: int64(len(content)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestResolveTargetPathInPlaceRejectedOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only")
	}
	_, err := ResolveTargetPath(`C:\bin\clovapi.exe`, true)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestResolveTargetPathDefaultUsesConfigBin(t *testing.T) {
	dir := t.TempDir()
	cfgpkg.SetDirOverride(dir)
	t.Cleanup(func() { cfgpkg.SetDirOverride("") })

	got, err := ResolveTargetPath("/tmp/clovapi", false)
	if err != nil {
		t.Fatal(err)
	}
	want, err := cfgpkg.CliBinPath()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf("got %q want %q", got, want)
	}
}
