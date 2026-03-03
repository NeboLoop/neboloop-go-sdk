package neboloop

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	// maxNappMetaFileSize is the max size for non-binary files in a .napp (1MB).
	maxNappMetaFileSize = 1 * 1024 * 1024
	// maxNappBinarySize is the max size for the binary in a .napp (500MB).
	maxNappBinarySize = 500 * 1024 * 1024
	// maxNappUIFileSize is the max size for individual UI files (5MB).
	maxNappUIFileSize = 5 * 1024 * 1024
	// maxNappDownloadSize is the max size for a .napp download (600MB).
	maxNappDownloadSize = 600 * 1024 * 1024
)

// Native binary magic bytes.
var (
	elfMagic      = []byte{0x7f, 'E', 'L', 'F'}
	machoMagic32  = []byte{0xfe, 0xed, 0xfa, 0xce}
	machoMagic64  = []byte{0xcf, 0xfa, 0xed, 0xfe}
	machoFatMagic = []byte{0xca, 0xfe, 0xba, 0xbe}
	peMagic       = []byte{0x4d, 0x5a}
	shebangMagic  = []byte{0x23, 0x21}
)

// ExtractNapp extracts a .napp (tar.gz) package to the destination directory.
//
// Security measures:
//   - Path traversal protection (rejects "../" and absolute paths)
//   - Symlink rejection (no symlinks or hard links allowed)
//   - File size limits (500MB binary, 5MB UI files, 1MB metadata files)
//   - Allowlist validation (only expected files: manifest.json, binary, signatures.json, ui/*)
//   - Validates required files are present (manifest.json, binary, signatures.json, SKILL.md)
//   - Validates the binary is a native compiled executable
func ExtractNapp(nappPath, destDir string) error {
	f, err := os.Open(nappPath)
	if err != nil {
		return fmt.Errorf("open napp: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)

	hasManifest := false
	hasBinary := false
	hasSignatures := false
	hasSkillMD := false

	cleanDestDir := filepath.Clean(destDir)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}

		// Reject symlinks and hard links
		if header.Typeflag == tar.TypeSymlink || header.Typeflag == tar.TypeLink {
			return fmt.Errorf("symlinks not allowed in .napp: %s", header.Name)
		}

		// Clean the path and reject traversal attempts
		clean := filepath.Clean(header.Name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return fmt.Errorf("path traversal in .napp: %s", header.Name)
		}

		target := filepath.Join(destDir, clean)
		// Ensure target is within destDir (defense in depth)
		if !strings.HasPrefix(filepath.Clean(target), cleanDestDir+string(filepath.Separator)) &&
			filepath.Clean(target) != cleanDestDir {
			return fmt.Errorf("path escape in .napp: %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0700); err != nil {
				return fmt.Errorf("create dir %s: %w", clean, err)
			}

		case tar.TypeReg:
			if !isAllowedNappFile(clean) {
				return fmt.Errorf("unexpected file in .napp: %s", clean)
			}

			maxSize := maxSizeForFile(clean)
			if header.Size > maxSize {
				return fmt.Errorf("file too large in .napp: %s (%d bytes, max %d)", clean, header.Size, maxSize)
			}

			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return fmt.Errorf("create parent dir for %s: %w", clean, err)
			}

			perm := os.FileMode(0600)
			if clean == "binary" || clean == "app" {
				perm = 0700
			}

			if err := extractFile(tr, target, perm, maxSize); err != nil {
				return fmt.Errorf("extract %s: %w", clean, err)
			}

			switch clean {
			case "manifest.json":
				hasManifest = true
			case "binary", "app":
				hasBinary = true
			case "signatures.json":
				hasSignatures = true
			case "SKILL.md", "skill.md":
				hasSkillMD = true
			}
		}
	}

	if !hasManifest {
		return fmt.Errorf("missing manifest.json in .napp")
	}
	if !hasBinary {
		return fmt.Errorf("missing binary in .napp")
	}
	if !hasSignatures {
		return fmt.Errorf("missing signatures.json in .napp")
	}
	if !hasSkillMD {
		return fmt.Errorf("missing SKILL.md in .napp — every app must include a skill definition")
	}

	// Validate the extracted binary is a native compiled executable.
	binaryPath := filepath.Join(destDir, "binary")
	if _, err := os.Stat(binaryPath); os.IsNotExist(err) {
		binaryPath = filepath.Join(destDir, "app")
	}
	if err := ValidateBinaryFormat(binaryPath); err != nil {
		os.RemoveAll(destDir)
		return fmt.Errorf("binary format validation failed: %w", err)
	}

	return nil
}

// DownloadNapp downloads a .napp from the URL using the client's auth and extracts it to destDir.
// The server must respond with a .napp tar.gz archive (Content-Type: application/x-napp).
// Raw binaries (application/octet-stream) are rejected — apps must be packaged as .napp.
func (c *APIClient) DownloadNapp(ctx context.Context, downloadURL, destDir string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	c.mu.RLock()
	req.Header.Set("Authorization", "Bearer "+c.token)
	c.mu.RUnlock()

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("download returned HTTP %d: %s", resp.StatusCode, string(b))
	}

	// Reject raw binaries — apps must be distributed as .napp packages
	ct := resp.Header.Get("Content-Type")
	if ct == "application/octet-stream" {
		return fmt.Errorf("server sent raw binary instead of .napp package (Content-Type: %s) — app needs to be packaged first", ct)
	}

	tmpFile, err := os.CreateTemp("", "nebo-app-*.napp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	written, err := io.Copy(tmpFile, io.LimitReader(resp.Body, maxNappDownloadSize+1))
	tmpFile.Close()
	if err != nil {
		return fmt.Errorf("download write: %w", err)
	}
	if written > maxNappDownloadSize {
		return fmt.Errorf("download too large (%d bytes, max %d)", written, maxNappDownloadSize)
	}

	if err := os.MkdirAll(destDir, 0700); err != nil {
		return fmt.Errorf("create app dir: %w", err)
	}

	if err := ExtractNapp(tmpPath, destDir); err != nil {
		return fmt.Errorf("extract: %w", err)
	}

	return nil
}

// ValidateBinaryFormat checks that a file is a native compiled binary,
// not a script or interpreted language artifact.
func ValidateBinaryFormat(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open binary for format check: %w", err)
	}
	defer f.Close()

	header := make([]byte, 4)
	n, err := f.Read(header)
	if err != nil || n < 2 {
		return fmt.Errorf("binary too small or unreadable — not a valid executable")
	}

	if bytes.HasPrefix(header, shebangMagic) {
		return fmt.Errorf("binary is a script (shebang #! detected) — only compiled native binaries are allowed")
	}

	if isNativeBinary(header) {
		return nil
	}

	return fmt.Errorf("binary is not a recognized native executable format (expected ELF, Mach-O, or PE) — interpreted languages are not permitted")
}

// extractFile writes a tar entry to disk with size enforcement.
func extractFile(r io.Reader, target string, perm os.FileMode, maxSize int64) error {
	outFile, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}

	written, err := io.Copy(outFile, io.LimitReader(r, maxSize+1))
	outFile.Close()

	if err != nil {
		os.Remove(target)
		return fmt.Errorf("write file: %w", err)
	}

	if written > maxSize {
		os.Remove(target)
		return fmt.Errorf("file exceeds size limit (%d bytes)", maxSize)
	}

	return nil
}

// isAllowedNappFile returns true if the file path is expected in a .napp package.
func isAllowedNappFile(path string) bool {
	path = filepath.ToSlash(path)
	switch path {
	case "manifest.json", "binary", "app", "signatures.json", "SKILL.md", "skill.md":
		return true
	}
	if strings.HasPrefix(path, "ui/") {
		return true
	}
	return false
}

// maxSizeForFile returns the maximum allowed size for a file in a .napp package.
func maxSizeForFile(path string) int64 {
	path = filepath.ToSlash(path)
	switch path {
	case "binary", "app":
		return maxNappBinarySize
	}
	if strings.HasPrefix(path, "ui/") {
		return maxNappUIFileSize
	}
	return maxNappMetaFileSize
}

// isNativeBinary checks if the file header matches any known native binary format.
func isNativeBinary(header []byte) bool {
	switch {
	case bytes.HasPrefix(header, elfMagic):
		return true
	case bytes.Equal(header, machoMagic32):
		return true
	case bytes.Equal(header, machoMagic64):
		return true
	case bytes.Equal(header, machoFatMagic):
		return true
	case bytes.HasPrefix(header, peMagic) && runtime.GOOS == "windows":
		return true
	case bytes.HasPrefix(header, peMagic) && len(header) >= 2:
		return true
	}
	return false
}
