package webapp

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	platform "agent-platform"
	"golang.org/x/mod/semver"
)

// Package tooling is offline: it never opens runtime stores or runs application code.
const packageByteLimit = 128 << 20
const packageEntryLimit = 10000

var packageRoots = []string{"backend", "frontend", "platform", "webapp.json", "AGENTS.md"}

type PackageReport struct {
	Source        string `json:"source"`
	AppID         string `json:"appId"`
	Key           string `json:"key"`
	Version       string `json:"version"`
	FileCount     int    `json:"fileCount"`
	ExpandedBytes int64  `json:"expandedBytes"`
	Output        string `json:"output,omitempty"`
	SHA256        string `json:"sha256,omitempty"`
}

type packageFile struct {
	name string
	data []byte
	mode fs.FileMode
}

type packageInput struct {
	files    []packageFile
	paths    map[string]string
	total    int64
	count    int
	manifest Manifest
}

func excludedPackagePath(name string) bool {
	for _, part := range strings.Split(strings.ToLower(name), "/") {
		if part == ".ds_store" || part == ".git" || part == "node_modules" || part == "__pycache__" || strings.HasPrefix(part, ".env") || strings.HasSuffix(part, ".pem") || strings.HasSuffix(part, ".local.yml") || strings.HasSuffix(part, ".local.yaml") {
			return true
		}
	}
	return false
}

func packagePath(name string, dir bool) error {
	if !safeRelative(name) || strings.ContainsAny(name, "\r\n\t") {
		return fmt.Errorf("unsafe WebApp package path: %q", name)
	}
	top, rest, nested := strings.Cut(name, "/")
	switch top {
	case "webapp.json", "AGENTS.md":
		if nested || dir {
			return fmt.Errorf("%s must be a root file", top)
		}
	case "backend", "frontend", "platform":
		if !nested && !dir {
			return fmt.Errorf("%s must be a directory", top)
		}
		if top == "platform" && nested {
			switch strings.Split(rest, "/")[0] {
			case "agents", "skills-center", "connectors-center", "registries", "configs":
			default:
				return fmt.Errorf("unknown platform resource: %s", name)
			}
			if !strings.Contains(rest, "/") && !dir {
				return fmt.Errorf("platform resource must be a directory: %s", name)
			}
		}
	default:
		return fmt.Errorf("unexpected WebApp package root: %s", top)
	}
	return nil
}

func (p *packageInput) add(name string, mode fs.FileMode, data []byte) error {
	if err := packagePath(name, mode.IsDir()); err != nil {
		return err
	}
	if excludedPackagePath(name) {
		return fmt.Errorf("private or development file in WebApp package: %s", name)
	}
	if mode.Type() != 0 && !mode.IsDir() {
		return fmt.Errorf("WebApp package links and special files are forbidden: %s", name)
	}
	if len(p.files) >= packageEntryLimit || int64(len(data)) > packageByteLimit-p.total {
		return fmt.Errorf("WebApp package limit exceeded")
	}
	for parent := name; parent != "."; parent = path.Dir(parent) {
		key := strings.ToLower(parent)
		if old, ok := p.paths[key]; ok && old != parent {
			return fmt.Errorf("case-colliding WebApp package paths: %s and %s", old, parent)
		}
		p.paths[key] = parent
	}
	p.files = append(p.files, packageFile{name, data, mode})
	p.total += int64(len(data))
	if !mode.IsDir() {
		p.count++
	}
	return nil
}

func (p *packageInput) validate() error {
	files := map[string]packageFile{}
	for _, file := range p.files {
		if _, ok := files[file.name]; ok {
			return fmt.Errorf("duplicate WebApp package path: %s", file.name)
		}
		files[file.name] = file
	}
	for name := range files {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if file, ok := files[parent]; ok && !file.mode.IsDir() {
				return fmt.Errorf("WebApp package file/directory collision: %s", parent)
			}
		}
	}
	m, err := ParseManifest(files["webapp.json"].data)
	if err != nil {
		return fmt.Errorf("webapp.json: %w", err)
	}
	if m.Hosting != "platform" {
		return fmt.Errorf("WebApp packaging requires hosting=platform")
	}
	entries := []string{path.Join(m.Frontend.Root, m.Frontend.Index)}
	if fallback := m.Frontend.RouteConfig.NavigationFallback; fallback != "" {
		entries = append(entries, path.Join(m.Frontend.Root, fallback))
	}
	if m.Backend != nil {
		entries = append(entries, m.Backend.Command.Entry)
	}
	for _, name := range entries {
		if file, ok := files[name]; !ok || !file.mode.IsRegular() {
			return fmt.Errorf("WebApp entry is missing or not a regular file: %s", name)
		}
	}
	p.manifest = m
	sort.Slice(p.files, func(i, j int) bool { return p.files[i].name < p.files[j].name })
	return nil
}

func readPackage(ctx context.Context, source string, directoryOnly bool) (*packageInput, PackageReport, error) {
	p := &packageInput{paths: map[string]string{}}
	var report PackageReport
	if err := ctx.Err(); err != nil {
		return nil, report, err
	}
	name, err := filepath.Abs(source)
	if err != nil {
		return nil, report, err
	}
	info, err := os.Lstat(name)
	if err != nil {
		return nil, report, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, report, fmt.Errorf("WebApp source must not be a link")
	}
	name, err = filepath.EvalSymlinks(name)
	if err != nil {
		return nil, report, err
	}
	if info.IsDir() {
		root, err := os.OpenRoot(name)
		if err != nil {
			return nil, report, err
		}
		defer root.Close()
		for _, top := range packageRoots {
			if _, err := root.Lstat(top); os.IsNotExist(err) && top != "frontend" && top != "webapp.json" {
				continue
			} else if err != nil {
				return nil, report, err
			}
			err := fs.WalkDir(root.FS(), top, func(name string, d fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if excludedPackagePath(name) {
					if d.IsDir() {
						return fs.SkipDir
					}
					return nil
				}
				info, err := d.Info()
				if err != nil {
					return err
				}
				if !info.Mode().IsRegular() {
					return p.add(name, info.Mode(), nil)
				}
				if info.Size() > packageByteLimit-p.total {
					return fmt.Errorf("WebApp package limit exceeded")
				}
				f, err := root.Open(name)
				if err != nil {
					return err
				}
				data, readErr := io.ReadAll(io.LimitReader(f, packageByteLimit-p.total+1))
				closeErr := f.Close()
				if readErr != nil {
					return readErr
				}
				if closeErr != nil {
					return closeErr
				}
				return p.add(name, info.Mode(), data)
			})
			if err != nil {
				return nil, report, err
			}
		}
	} else if info.Mode().IsRegular() && !directoryOnly {
		if info.Size() > packageByteLimit {
			return nil, report, fmt.Errorf("WebApp ZIP size limit exceeded")
		}
		z, err := zip.OpenReader(name)
		if err != nil {
			return nil, report, err
		}
		defer z.Close()
		if len(z.File) > packageEntryLimit {
			return nil, report, fmt.Errorf("WebApp package limit exceeded")
		}
		for _, file := range z.File {
			if err := ctx.Err(); err != nil {
				return nil, report, err
			}
			name := strings.TrimSuffix(file.Name, "/")
			if err := packagePath(name, file.Mode().IsDir()); err != nil {
				return nil, report, err
			}
			if file.UncompressedSize64 > uint64(packageByteLimit-p.total) {
				return nil, report, fmt.Errorf("WebApp expanded ZIP size limit exceeded")
			}
			if file.Mode().IsDir() {
				err = p.add(name, file.Mode(), nil)
			} else {
				f, openErr := file.Open()
				if openErr != nil {
					return nil, report, openErr
				}
				data, readErr := io.ReadAll(io.LimitReader(f, packageByteLimit-p.total+1))
				closeErr := f.Close()
				if readErr != nil {
					return nil, report, readErr
				}
				if closeErr != nil {
					return nil, report, closeErr
				}
				err = p.add(name, file.Mode(), data)
			}
			if err != nil {
				return nil, report, err
			}
		}
	} else {
		kind := "a directory or ZIP"
		if directoryOnly {
			kind = "a directory"
		}
		return nil, report, fmt.Errorf("WebApp source must be %s", kind)
	}
	if err := p.validate(); err != nil {
		return nil, report, err
	}
	report = PackageReport{Source: name, AppID: p.manifest.ID, Key: p.manifest.Key, Version: p.manifest.Version, FileCount: p.count, ExpandedBytes: p.total}
	return p, report, nil
}

// CheckPackage validates the same selected files PackPackage will distribute.
// ZIP inputs must already use the flat five-root layout and contain no excluded files.
func CheckPackage(ctx context.Context, source string) (PackageReport, error) {
	_, report, err := readPackage(ctx, source, false)
	return report, err
}

// UnpackPackage validates the entire input before writing a fresh destination.
// It never merges into a runtime or follows an existing destination symlink.
func UnpackPackage(ctx context.Context, source, output string) (PackageReport, error) {
	p, report, err := readPackage(ctx, source, false)
	if err != nil {
		return report, err
	}
	if p.manifest.Hosting == "platform" && semver.Compare("v"+platform.Version(), "v"+p.manifest.MinPlatformVersion) < 0 {
		return report, fmt.Errorf("WebApp requires Platform %s (current %s)", p.manifest.MinPlatformVersion, platform.Version())
	}
	if output == "" {
		return report, fmt.Errorf("unpack requires --output")
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return report, err
	}
	if _, err = os.Lstat(output); !os.IsNotExist(err) {
		return report, fmt.Errorf("unpack destination must not exist")
	}
	if err = os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		return report, err
	}
	stage, err := os.MkdirTemp(filepath.Dir(output), ".webapp-unpack-")
	if err != nil {
		return report, err
	}
	defer os.RemoveAll(stage)
	for _, f := range p.files {
		if err = ctx.Err(); err != nil {
			return report, err
		}
		target := filepath.Join(stage, filepath.FromSlash(f.name))
		if f.mode.IsDir() {
			err = os.MkdirAll(target, 0755)
		} else {
			err = os.MkdirAll(filepath.Dir(target), 0755)
			if err == nil {
				mode := fs.FileMode(0644)
				if f.mode.Perm()&0111 != 0 {
					mode = 0755
				}
				err = os.WriteFile(target, f.data, mode)
			}
		}
		if err != nil {
			return report, err
		}
	}
	if err = os.Rename(stage, output); err != nil {
		return report, err
	}
	report.Output = output
	return report, nil
}

func packageOutputPath(output string) (string, error) {
	parent, tail := filepath.Dir(output), filepath.Base(output)
	for {
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			return filepath.Join(resolved, tail), nil
		}
		if !os.IsNotExist(err) || parent == filepath.Dir(parent) {
			return "", err
		}
		tail = filepath.Join(filepath.Base(parent), tail)
		parent = filepath.Dir(parent)
	}
}

// PackPackage preserves manifest bytes and resource paths. It never disables a
// backend, changes deployment settings, or copies runtime state into the package.
func PackPackage(ctx context.Context, source, output string) (PackageReport, error) {
	p, report, err := readPackage(ctx, source, true)
	if err != nil {
		return report, err
	}
	if output == "" {
		output = filepath.Join(report.Source, "dist", report.Key+"-"+report.Version+".zip")
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return report, err
	}
	if !strings.EqualFold(filepath.Ext(output), ".zip") {
		return report, fmt.Errorf("WebApp output must be a .zip file")
	}
	output, err = packageOutputPath(output)
	if err != nil {
		return report, err
	}
	if relative, err := filepath.Rel(report.Source, output); err == nil {
		top := strings.Split(filepath.ToSlash(relative), "/")[0]
		for _, selected := range packageRoots {
			if strings.EqualFold(top, selected) {
				return report, fmt.Errorf("WebApp output cannot replace selected source content")
			}
		}
	}
	if info, err := os.Lstat(output); err == nil && !info.Mode().IsRegular() {
		return report, fmt.Errorf("WebApp output must not be a link or directory")
	} else if err != nil && !os.IsNotExist(err) {
		return report, err
	}
	parent := filepath.Dir(output)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return report, err
	}
	f, err := os.CreateTemp(parent, ".webapp-pack-*.zip")
	if err != nil {
		return report, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	hash := sha256.New()
	z := zip.NewWriter(io.MultiWriter(f, hash))
	for _, file := range p.files {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		header := &zip.FileHeader{Name: file.name, Method: zip.Deflate, Modified: time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)}
		mode := fs.FileMode(0644)
		if file.mode.IsDir() {
			header.Name += "/"
			header.Method = zip.Store
			mode = fs.ModeDir | 0755
		} else if file.mode&0111 != 0 {
			mode = 0755
		}
		header.SetMode(mode)
		writer, err := z.CreateHeader(header)
		if err != nil {
			return report, err
		}
		if _, err := writer.Write(file.data); err != nil {
			return report, err
		}
	}
	if err := z.Close(); err != nil {
		return report, err
	}
	info, err := f.Stat()
	if err != nil {
		return report, err
	}
	if info.Size() > packageByteLimit {
		return report, fmt.Errorf("WebApp ZIP size limit exceeded")
	}
	if err := f.Sync(); err != nil {
		return report, err
	}
	if err := f.Close(); err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if err := os.Rename(f.Name(), output); err != nil {
		return report, err
	}
	report.Output, report.SHA256 = output, hex.EncodeToString(hash.Sum(nil))
	return report, nil
}
