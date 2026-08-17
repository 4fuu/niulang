// Command queqiaopack builds the distributable queqiaod archives.
//
// It deliberately owns archive creation instead of delegating it to the host's
// tar and zip programs. That gives every file a fixed timestamp, owner, mode,
// and order, so identical source and metadata produce identical artifacts.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

var defaultTargets = []target{
	{goos: "linux", goarch: "amd64"},
	{goos: "linux", goarch: "arm64"},
	{goos: "darwin", goarch: "amd64"},
	{goos: "darwin", goarch: "arm64"},
	{goos: "windows", goarch: "amd64"},
	{goos: "windows", goarch: "arm64"},
}

var safeMetadata = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+~-]*$`)

type target struct {
	goos   string
	goarch string
}

func (t target) String() string { return t.goos + "/" + t.goarch }

type config struct {
	repoRoot  string
	outputDir string
	version   string
	commit    string
	buildDate time.Time
	targets   []target
}

type archiveFile struct {
	name string
	mode int64
	data []byte
}

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "queqiaopack: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("queqiaopack", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var cfg config
	var buildDate, targets string
	fs.StringVar(&cfg.repoRoot, "repo", ".", "repository root")
	fs.StringVar(&cfg.outputDir, "output", "dist", "new output directory (must not already exist)")
	fs.StringVar(&cfg.version, "version", "", "release version, for example v0.1.0")
	fs.StringVar(&cfg.commit, "commit", "", "source commit")
	fs.StringVar(&buildDate, "build-date", "", "source commit time in RFC3339 form")
	fs.StringVar(&targets, "targets", targetList(defaultTargets), "comma-separated GOOS/GOARCH targets")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if buildDate == "" {
		return errors.New("--build-date is required")
	}
	parsedDate, err := time.Parse(time.RFC3339, buildDate)
	if err != nil {
		return fmt.Errorf("parse --build-date: %w", err)
	}
	cfg.buildDate = parsedDate.UTC()
	cfg.targets, err = parseTargets(targets)
	if err != nil {
		return err
	}
	return packageRelease(ctx, cfg)
}

func packageRelease(ctx context.Context, cfg config) error {
	if !safeMetadata.MatchString(cfg.version) {
		return errors.New("--version is required and may contain only release-safe characters")
	}
	if !safeMetadata.MatchString(cfg.commit) {
		return errors.New("--commit is required and may contain only release-safe characters")
	}
	if cfg.buildDate.IsZero() {
		return errors.New("--build-date is required")
	}
	if cfg.buildDate.Year() < 1980 {
		return errors.New("--build-date must be 1980 or later for ZIP compatibility")
	}
	if len(cfg.targets) == 0 {
		return errors.New("at least one target is required")
	}

	repoRoot, err := filepath.Abs(cfg.repoRoot)
	if err != nil {
		return fmt.Errorf("resolve repository root: %w", err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err != nil {
		return fmt.Errorf("repository root %q has no go.mod: %w", repoRoot, err)
	}
	outputDir, err := filepath.Abs(cfg.outputDir)
	if err != nil {
		return fmt.Errorf("resolve output directory: %w", err)
	}
	if _, err := os.Lstat(outputDir); err == nil {
		return fmt.Errorf("output directory %q already exists", outputDir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect output directory: %w", err)
	}
	outputParent := filepath.Dir(outputDir)
	if err := os.MkdirAll(outputParent, 0o755); err != nil {
		return fmt.Errorf("create output parent: %w", err)
	}
	tempDir, err := os.MkdirTemp(outputParent, ".queqiaopack-")
	if err != nil {
		return fmt.Errorf("create temporary package directory: %w", err)
	}
	defer os.RemoveAll(tempDir)
	resultDir := filepath.Join(tempDir, "result")
	if err := os.Mkdir(resultDir, 0o755); err != nil {
		return err
	}

	documents, err := readDistributionFiles(repoRoot)
	if err != nil {
		return err
	}
	checksums := make(map[string][sha256.Size]byte, len(cfg.targets))
	for _, target := range cfg.targets {
		archiveName, sum, err := buildTarget(ctx, repoRoot, tempDir, resultDir, cfg, target, documents)
		if err != nil {
			return err
		}
		checksums[archiveName] = sum
	}
	if err := writeChecksums(filepath.Join(resultDir, "SHA256SUMS"), checksums); err != nil {
		return err
	}
	if err := os.Rename(resultDir, outputDir); err != nil {
		return fmt.Errorf("publish package directory: %w", err)
	}
	fmt.Printf("wrote %d release archives and SHA256SUMS to %s\n", len(cfg.targets), outputDir)
	return nil
}

func buildTarget(ctx context.Context, repoRoot, tempDir, resultDir string, cfg config, target target, documents []archiveFile) (string, [sha256.Size]byte, error) {
	packageBase := fmt.Sprintf("queqiaod_%s_%s_%s", cfg.version, target.goos, target.goarch)
	binaryName := "queqiaod"
	if target.goos == "windows" {
		binaryName += ".exe"
	}
	buildDir := filepath.Join(tempDir, "build", target.goos+"-"+target.goarch)
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		return "", [sha256.Size]byte{}, err
	}
	binaryPath := filepath.Join(buildDir, binaryName)
	ldflags := strings.Join([]string{
		"-s", "-w",
		"-X=main.version=" + cfg.version,
		"-X=main.commit=" + cfg.commit,
		"-X=main.buildDate=" + cfg.buildDate.Format(time.RFC3339),
	}, " ")
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-buildvcs=false", "-ldflags", ldflags, "-o", binaryPath, "./cmd/queqiaod")
	cmd.Dir = repoRoot
	cmd.Env = withEnv(os.Environ(), map[string]string{
		"CGO_ENABLED": "0",
		"GOOS":        target.goos,
		"GOARCH":      target.goarch,
	})
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", [sha256.Size]byte{}, fmt.Errorf("build %s: %w\n%s", target, err, output)
	}
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		return "", [sha256.Size]byte{}, fmt.Errorf("read %s binary: %w", target, err)
	}
	binarySum := sha256.Sum256(binary)
	files := make([]archiveFile, 0, len(documents)+2)
	files = append(files, archiveFile{name: binaryName, mode: 0o755, data: binary})
	files = append(files, documents...)
	files = append(files, archiveFile{
		name: "BUILDINFO",
		mode: 0o644,
		data: []byte(fmt.Sprintf(
			"version=%s\ncommit=%s\nbuild_date=%s\ntarget=%s\ngo=%s\nbinary_sha256=%x\n",
			cfg.version, cfg.commit, cfg.buildDate.Format(time.RFC3339), target, runtime.Version(), binarySum,
		)),
	})
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })

	extension := ".tar.gz"
	if target.goos == "windows" {
		extension = ".zip"
	}
	archiveName := packageBase + extension
	archivePath := filepath.Join(resultDir, archiveName)
	if target.goos == "windows" {
		err = writeZip(archivePath, packageBase, files, cfg.buildDate)
	} else {
		err = writeTarGzip(archivePath, packageBase, files, cfg.buildDate)
	}
	if err != nil {
		return "", [sha256.Size]byte{}, fmt.Errorf("archive %s: %w", target, err)
	}
	archive, err := os.ReadFile(archivePath)
	if err != nil {
		return "", [sha256.Size]byte{}, err
	}
	return archiveName, sha256.Sum256(archive), nil
}

func readDistributionFiles(repoRoot string) ([]archiveFile, error) {
	names := []string{
		"LICENSE",
		"README.md",
		"docs/DEPLOYING.md",
		"docs/RELEASING.md",
		"deploy/clash-queqiao.yaml",
		"deploy/me.01.queqiao.client.plist",
		"deploy/queqiaod.service",
	}
	files := make([]archiveFile, 0, len(names))
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(name)))
		if err != nil {
			return nil, fmt.Errorf("read distribution file %s: %w", name, err)
		}
		files = append(files, archiveFile{name: name, mode: 0o644, data: data})
	}
	return files, nil
}

func writeTarGzip(path, root string, files []archiveFile, modTime time.Time) (returnErr error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); returnErr == nil && err != nil {
			returnErr = err
		}
	}()
	gz, err := gzip.NewWriterLevel(file, gzip.BestCompression)
	if err != nil {
		return err
	}
	gz.Header.ModTime = modTime
	gz.Header.OS = 255
	tw := tar.NewWriter(gz)
	for _, entry := range files {
		header := &tar.Header{
			Name:    root + "/" + entry.name,
			Mode:    entry.mode,
			Size:    int64(len(entry.data)),
			ModTime: modTime,
			Format:  tar.FormatGNU,
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if _, err := tw.Write(entry.data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func writeZip(path, root string, files []archiveFile, modTime time.Time) (returnErr error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); returnErr == nil && err != nil {
			returnErr = err
		}
	}()
	zw := zip.NewWriter(file)
	for _, entry := range files {
		header := &zip.FileHeader{Name: root + "/" + entry.name, Method: zip.Deflate}
		header.SetMode(os.FileMode(entry.mode))
		header.SetModTime(modTime)
		writer, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		if _, err := writer.Write(entry.data); err != nil {
			return err
		}
	}
	return zw.Close()
}

func writeChecksums(path string, checksums map[string][sha256.Size]byte) error {
	names := make([]string, 0, len(checksums))
	for name := range checksums {
		names = append(names, name)
	}
	sort.Strings(names)
	var output strings.Builder
	for _, name := range names {
		fmt.Fprintf(&output, "%x  %s\n", checksums[name], name)
	}
	if err := os.WriteFile(path, []byte(output.String()), 0o644); err != nil {
		return fmt.Errorf("write SHA256SUMS: %w", err)
	}
	return nil
}

func parseTargets(value string) ([]target, error) {
	seen := make(map[string]bool)
	var targets []target
	for _, raw := range strings.Split(value, ",") {
		parts := strings.Split(strings.TrimSpace(raw), "/")
		if len(parts) != 2 || !safeMetadata.MatchString(parts[0]) || !safeMetadata.MatchString(parts[1]) {
			return nil, fmt.Errorf("invalid target %q; want GOOS/GOARCH", raw)
		}
		target := target{goos: parts[0], goarch: parts[1]}
		if !seen[target.String()] {
			seen[target.String()] = true
			targets = append(targets, target)
		}
	}
	if len(targets) == 0 {
		return nil, errors.New("at least one target is required")
	}
	return targets, nil
}

func targetList(targets []target) string {
	values := make([]string, len(targets))
	for i, target := range targets {
		values[i] = target.String()
	}
	return strings.Join(values, ",")
}

func withEnv(base []string, replacements map[string]string) []string {
	result := make([]string, 0, len(base)+len(replacements))
	for _, value := range base {
		key, _, found := strings.Cut(value, "=")
		if !found {
			continue
		}
		if _, replace := replacements[key]; !replace {
			result = append(result, value)
		}
	}
	keys := make([]string, 0, len(replacements))
	for key := range replacements {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, key+"="+replacements[key])
	}
	return result
}
