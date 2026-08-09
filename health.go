package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

// version is set at build time with -ldflags "-X main.version=...". See the
// Makefile.
var version = "dev"

// DatabaseHealth describes one artifact as this process actually has it open.
type DatabaseHealth struct {
	Path   string    `json:"path"`
	SHA256 string    `json:"sha256"`
	Rows   int64     `json:"rows"`
	FTS    FTSHealth `json:"fts"`
}

// HealthResponse is a cross-repo contract: the publisher asserts against it
// after every release.
//
// Status is "ok", or "degraded" when an artifact opened successfully but its
// full-text index does not cover every row. The endpoint answers 200 either
// way: a degraded service is still serving, and returning a failure code would
// have a load balancer pull a process that is answering correctly for
// everything except the completeness of search. Read the field, not the code.
type HealthResponse struct {
	Status    string                    `json:"status"`
	Version   string                    `json:"version"`
	Databases map[string]DatabaseHealth `json:"databases"`
}

const (
	statusOK       = "ok"
	statusDegraded = "degraded"
)

// healthSnapshot is computed once at startup and served unchanged thereafter.
var healthSnapshot HealthResponse

// buildHealthSnapshot inspects each artifact once, at startup, so that /health
// reports the inode this process has open rather than whatever the "current"
// symlink points at by the time someone curls it. Those two differing is
// precisely the bug this endpoint exists to catch. Hashing once also keeps a
// 44 MB re-read off every request.
func buildHealthSnapshot(dbs map[string]struct {
	Path string
	DB   *gorm.DB
}) (HealthResponse, error) {
	snapshot := HealthResponse{
		Status:    statusOK,
		Version:   version,
		Databases: make(map[string]DatabaseHealth, len(dbs)),
	}

	for lang, entry := range dbs {
		// Resolve the symlink so the reported path names the release
		// directory, not "current".
		resolved, err := filepath.EvalSymlinks(entry.Path)
		if err != nil {
			return HealthResponse{}, err
		}

		sum, err := fileSHA256(resolved)
		if err != nil {
			return HealthResponse{}, err
		}

		var rows int64
		if err := entry.DB.Model(&Event{}).Count(&rows).Error; err != nil {
			return HealthResponse{}, err
		}

		// A failure here aborts the boot: probeFTS only errors on conditions
		// under which search cannot work at all.
		fts, err := probeFTS(entry.DB, rows)
		if err != nil {
			return HealthResponse{}, fmt.Errorf("%s: %w", lang, err)
		}
		if !fts.Consistent {
			snapshot.Status = statusDegraded
		}

		snapshot.Databases[lang] = DatabaseHealth{
			Path:   resolved,
			SHA256: sum,
			Rows:   rows,
			FTS:    fts,
		}
	}

	return snapshot, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// healthHandler is registered outside the /api group and needs no API key: a
// deploy check that requires a secret is a deploy check that gets skipped.
func healthHandler(c *fiber.Ctx) error {
	return c.JSON(healthSnapshot)
}
