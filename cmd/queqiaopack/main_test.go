package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/queqiao/internal/protocol"
)

func TestArchivesAreDeterministicAndKeepModes(t *testing.T) {
	timestamp := time.Date(2026, time.August, 17, 3, 4, 6, 0, time.UTC)
	files := []archiveFile{
		{name: "README.md", mode: 0o644, data: []byte("release notes\n")},
		{name: "assets/queqiao-icon.png", mode: 0o644, data: []byte("icon\n")},
		{name: "queqiaod", mode: 0o755, data: []byte("binary\n")},
	}
	for _, format := range []string{"tar.gz", "zip"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			first := filepath.Join(dir, "first."+format)
			second := filepath.Join(dir, "second."+format)
			var err error
			if format == "tar.gz" {
				err = writeTarGzip(first, "queqiaod_v1_linux_amd64", files, timestamp)
				if err == nil {
					err = writeTarGzip(second, "queqiaod_v1_linux_amd64", files, timestamp)
				}
			} else {
				err = writeZip(first, "queqiaod_v1_windows_amd64", files, timestamp)
				if err == nil {
					err = writeZip(second, "queqiaod_v1_windows_amd64", files, timestamp)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			firstData, _ := os.ReadFile(first)
			secondData, _ := os.ReadFile(second)
			if sha256.Sum256(firstData) != sha256.Sum256(secondData) {
				t.Fatal("identical inputs produced different archives")
			}
			modes := archiveModes(t, first, format)
			if modes["queqiaod"] != 0o755 || modes["README.md"] != 0o644 || modes["assets/queqiao-icon.png"] != 0o644 {
				t.Fatalf("archive modes = %v", modes)
			}
		})
	}
}

// README is included in every release archive and refers to the project icon
// by a relative path. Packaging one without the other leaves the first thing a
// user opens with a broken image.
func TestDistributionIncludesREADMEIcon(t *testing.T) {
	files, err := readDistributionFiles(filepath.Clean(filepath.Join("..", "..")))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.name == "assets/queqiao-icon.png" {
			if len(file.data) < 8 || !bytes.Equal(file.data[:8], []byte("\x89PNG\r\n\x1a\n")) {
				t.Fatal("packaged project icon is not a PNG")
			}
			return
		}
	}
	t.Fatal("distribution omits assets/queqiao-icon.png")
}

func TestTargetsAreParsedOnceInRequestedOrder(t *testing.T) {
	got, err := parseTargets("linux/amd64, windows/arm64,linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	want := []target{{goos: "linux", goarch: "amd64"}, {goos: "windows", goarch: "arm64"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("targets = %#v, want %#v", got, want)
	}
	if _, err := parseTargets("linux"); err == nil {
		t.Fatal("target without architecture was accepted")
	}
}

func TestChecksumsAreSorted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "SHA256SUMS")
	checksums := map[string][sha256.Size]byte{
		"z.zip": sha256.Sum256([]byte("z")),
		"a.zip": sha256.Sum256([]byte("a")),
	}
	if err := writeChecksums(path, checksums); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if !strings.HasSuffix(lines[0], "  a.zip") || !strings.HasSuffix(lines[1], "  z.zip") {
		t.Fatalf("checksums are not sorted: %q", data)
	}
}

func TestCycloneDXIsDeterministicAndNamesTheLinkedBinary(t *testing.T) {
	cfg := config{
		version: "v0.1.0-rc.1", commit: "0123456789abcdef",
		buildDate: time.Date(2026, time.August, 17, 3, 4, 6, 0, time.UTC),
	}
	target := target{goos: "linux", goarch: "amd64"}
	binarySum := sha256.Sum256([]byte("binary"))
	modules := []linkedModule{{
		Path: "example.com/dependency", Version: "v1.2.3", Sum: "h1:example",
		LicenseID: "MIT",
	}}
	first, err := renderCycloneDX(cfg, target, binarySum, modules)
	if err != nil {
		t.Fatal(err)
	}
	second, err := renderCycloneDX(cfg, target, binarySum, modules)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("identical release inputs produced different SBOMs")
	}
	var bom cdxBOM
	if err := json.Unmarshal(first, &bom); err != nil {
		t.Fatal(err)
	}
	if bom.BOMFormat != "CycloneDX" || bom.SpecVersion != "1.5" || bom.Metadata.Component.Version != cfg.version {
		t.Fatalf("unexpected SBOM identity: %+v", bom)
	}
	if !reflect.DeepEqual(bom.Metadata.Component.Licenses, []cdxLicenseChoice{{License: cdxLicense{ID: "MIT"}}}) {
		t.Fatalf("root SBOM license = %+v, want MIT", bom.Metadata.Component.Licenses)
	}
	if got := bom.Metadata.Component.Properties[2].Value; got != fmt.Sprint(protocol.Version) {
		t.Fatalf("wire protocol property = %q, want %d", got, protocol.Version)
	}
	if len(bom.Components) != 1 || bom.Components[0].Name != modules[0].Path || len(bom.Dependencies) != 1 {
		t.Fatalf("linked dependency was not represented: %+v", bom)
	}
}

func TestThirdPartyLicenseBundleUsesExactModuleText(t *testing.T) {
	dir := t.TempDir()
	licenseText := []byte("permission granted\n")
	if err := os.WriteFile(filepath.Join(dir, "LICENSE"), licenseText, 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := renderThirdPartyLicenses([]linkedModule{{
		Path: "example.com/dependency", Version: "v1.2.3", LicenseID: "MIT", Dir: dir,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range [][]byte{[]byte("example.com/dependency v1.2.3"), []byte("SPDX license: MIT"), licenseText} {
		if !bytes.Contains(data, want) {
			t.Fatalf("license bundle does not contain %q:\n%s", want, data)
		}
	}
}

func archiveModes(t *testing.T, path, format string) map[string]os.FileMode {
	t.Helper()
	modes := make(map[string]os.FileMode)
	if format == "zip" {
		reader, err := zip.OpenReader(path)
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		for _, file := range reader.File {
			modes[archiveEntryRelativeName(file.Name)] = file.Mode().Perm()
		}
		return modes
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		modes[archiveEntryRelativeName(header.Name)] = os.FileMode(header.Mode).Perm()
	}
	return modes
}

func archiveEntryRelativeName(name string) string {
	parts := strings.SplitN(filepath.ToSlash(name), "/", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return parts[0]
}
