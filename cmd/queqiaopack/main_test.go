package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestArchivesAreDeterministicAndKeepModes(t *testing.T) {
	timestamp := time.Date(2026, time.August, 17, 3, 4, 6, 0, time.UTC)
	files := []archiveFile{
		{name: "README.md", mode: 0o644, data: []byte("release notes\n")},
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
			if modes["queqiaod"] != 0o755 || modes["README.md"] != 0o644 {
				t.Fatalf("archive modes = %v", modes)
			}
		})
	}
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
			modes[filepath.Base(file.Name)] = file.Mode().Perm()
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
		modes[filepath.Base(header.Name)] = os.FileMode(header.Mode).Perm()
	}
	return modes
}
