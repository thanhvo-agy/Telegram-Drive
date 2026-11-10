package webdav

import (
	"context"
	"errors"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/snapshot"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/telegram"
)

// VirtualFS implements webdav.FileSystem for mounting Telegram-Drive snapshots
// as a virtual, read-only filesystem compatible with RFC 4918.
type VirtualFS struct {
	engine        *snapshot.Engine
	client        *telegram.Client
	cache         *ChunkCache
	passphrase    string
	snapshotID    string // If set, mounts single snapshot at root; if empty, multi-snapshot mode
	readOnly      bool
	mu            sync.RWMutex
	manifestCache map[string]*snapshot.SnapshotManifest
}

// NewVirtualFS creates a virtual WebDAV filesystem.
func NewVirtualFS(engine *snapshot.Engine, client *telegram.Client, cache *ChunkCache, passphrase string, snapshotID string, readOnly bool) *VirtualFS {
	return &VirtualFS{
		engine:        engine,
		client:        client,
		cache:         cache,
		passphrase:    passphrase,
		snapshotID:    snapshotID,
		readOnly:      readOnly,
		manifestCache: make(map[string]*snapshot.SnapshotManifest),
	}
}

// isMetadataProbe returns true for OS-specific noise and attribute files
// (e.g. macOS AppleDouble and Windows Mini-Redirector probes).
func isMetadataProbe(name string) bool {
	base := path.Base(name)
	if strings.HasPrefix(base, "._") || base == ".DS_Store" {
		return true
	}
	lower := strings.ToLower(base)
	if lower == "desktop.ini" || lower == "autorun.inf" || lower == "thumbs.db" {
		return true
	}
	return false
}

// cleanReqPath standardizes WebDAV request paths to clean slash paths.
func cleanReqPath(name string) string {
	clean := path.Clean("/" + name)
	return clean
}

// Mkdir returns os.ErrPermission to enforce read-only snapshot immutability.
func (fs *VirtualFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	return os.ErrPermission
}

// RemoveAll returns os.ErrPermission to enforce read-only snapshot immutability.
func (fs *VirtualFS) RemoveAll(ctx context.Context, name string) error {
	return os.ErrPermission
}

// Rename returns os.ErrPermission to enforce read-only snapshot immutability.
func (fs *VirtualFS) Rename(ctx context.Context, oldName, newName string) error {
	return os.ErrPermission
}

// OpenFile opens a virtual file or directory for reading. Opening with write flags
// returns os.ErrPermission.
func (fs *VirtualFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	// Rejection of write operations
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_TRUNC) != 0 {
		return nil, os.ErrPermission
	}

	clean := cleanReqPath(name)

	// Cleanly return 404 on metadata probes without freezing
	if isMetadataProbe(clean) {
		return nil, os.ErrNotExist
	}

	// 1. Single snapshot mode: / is the snapshot root
	if fs.snapshotID != "" {
		return fs.openSnapshotPath(ctx, fs.snapshotID, clean)
	}

	// 2. Multi-snapshot mode:
	// / -> virtual root listing /latest and /snapshots
	if clean == "/" || clean == "" {
		entries := []os.FileInfo{
			&virtualFileInfo{name: "latest", mode: os.ModeDir | 0755, modTime: time.Now(), isDir: true},
			&virtualFileInfo{name: "snapshots", mode: os.ModeDir | 0755, modTime: time.Now(), isDir: true},
		}
		return newWebdavDir("/", time.Now(), entries), nil
	}

	// /snapshots -> lists all snapshot IDs
	if clean == "/snapshots" {
		summaries, err := fs.engine.ListSnapshots()
		if err != nil {
			return nil, err
		}
		entries := make([]os.FileInfo, 0, len(summaries))
		for _, s := range summaries {
			entries = append(entries, &virtualFileInfo{
				name:    s.SnapshotID,
				mode:    os.ModeDir | 0755,
				modTime: s.CreatedAt,
				isDir:   true,
			})
		}
		return newWebdavDir("snapshots", time.Now(), entries), nil
	}

	// /snapshots/<id>/... -> routed to snapshot <id>
	if strings.HasPrefix(clean, "/snapshots/") {
		remainder := strings.TrimPrefix(clean, "/snapshots/")
		parts := strings.SplitN(remainder, "/", 2)
		snapID := parts[0]
		subPath := "/"
		if len(parts) > 1 && parts[1] != "" {
			subPath = "/" + parts[1]
		}
		return fs.openSnapshotPath(ctx, snapID, subPath)
	}

	// /latest/... -> routed to latest snapshot
	if clean == "/latest" || strings.HasPrefix(clean, "/latest/") {
		subPath := "/"
		if strings.HasPrefix(clean, "/latest/") {
			subPath = "/" + strings.TrimPrefix(clean, "/latest/")
		}
		latest, err := fs.getLatestManifest()
		if err != nil {
			return nil, os.ErrNotExist
		}
		return fs.openInManifest(ctx, latest, subPath)
	}

	// Direct root access fallback to latest snapshot (for ease of navigation)
	latest, err := fs.getLatestManifest()
	if err == nil {
		return fs.openInManifest(ctx, latest, clean)
	}

	return nil, os.ErrNotExist
}

// Stat returns FileInfo for virtual files and directories.
func (fs *VirtualFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	clean := cleanReqPath(name)

	if isMetadataProbe(clean) {
		return nil, os.ErrNotExist
	}

	// 1. Single snapshot mode
	if fs.snapshotID != "" {
		return fs.statSnapshotPath(ctx, fs.snapshotID, clean)
	}

	// 2. Multi-snapshot root
	if clean == "/" || clean == "" {
		return &virtualFileInfo{name: "/", mode: os.ModeDir | 0755, modTime: time.Now(), isDir: true}, nil
	}

	if clean == "/snapshots" {
		return &virtualFileInfo{name: "snapshots", mode: os.ModeDir | 0755, modTime: time.Now(), isDir: true}, nil
	}

	if strings.HasPrefix(clean, "/snapshots/") {
		remainder := strings.TrimPrefix(clean, "/snapshots/")
		parts := strings.SplitN(remainder, "/", 2)
		snapID := parts[0]
		subPath := "/"
		if len(parts) > 1 && parts[1] != "" {
			subPath = "/" + parts[1]
		}
		return fs.statSnapshotPath(ctx, snapID, subPath)
	}

	if clean == "/latest" {
		latest, err := fs.getLatestManifest()
		if err != nil {
			return nil, os.ErrNotExist
		}
		return &virtualFileInfo{name: "latest", mode: os.ModeDir | 0755, modTime: latest.CreatedAt, isDir: true}, nil
	}

	if strings.HasPrefix(clean, "/latest/") {
		subPath := "/" + strings.TrimPrefix(clean, "/latest/")
		latest, err := fs.getLatestManifest()
		if err != nil {
			return nil, os.ErrNotExist
		}
		return fs.statInManifest(latest, subPath)
	}

	// Fallback to latest snapshot
	latest, err := fs.getLatestManifest()
	if err == nil {
		return fs.statInManifest(latest, clean)
	}

	return nil, os.ErrNotExist
}

// openSnapshotPath opens a path within a specific snapshot ID.
func (fs *VirtualFS) openSnapshotPath(ctx context.Context, snapID string, subPath string) (webdav.File, error) {
	m, err := fs.getManifest(snapID)
	if err != nil {
		return nil, os.ErrNotExist
	}
	return fs.openInManifest(ctx, m, subPath)
}

// statSnapshotPath returns FileInfo for a path within a specific snapshot ID.
func (fs *VirtualFS) statSnapshotPath(ctx context.Context, snapID string, subPath string) (os.FileInfo, error) {
	m, err := fs.getManifest(snapID)
	if err != nil {
		return nil, os.ErrNotExist
	}
	return fs.statInManifest(m, subPath)
}

// openInManifest resolves a subpath inside a loaded manifest.
func (fs *VirtualFS) openInManifest(ctx context.Context, m *snapshot.SnapshotManifest, subPath string) (webdav.File, error) {
	rel := strings.TrimPrefix(path.Clean(subPath), "/")

	// Check if opening root of manifest
	if rel == "" || rel == "." {
		items := m.ListDirectory("")
		entries := make([]os.FileInfo, 0, len(items))
		for _, it := range items {
			mode := os.FileMode(0644)
			if it.IsDir {
				mode = os.ModeDir | 0755
			}
			entries = append(entries, &virtualFileInfo{
				name:    it.Name,
				size:    it.Size,
				mode:    mode,
				modTime: it.ModTime,
				isDir:   it.IsDir,
			})
		}
		return newWebdavDir(path.Base(m.SnapshotID), m.CreatedAt, entries), nil
	}

	// Check if regular file
	if fe, found := m.Files[rel]; found {
		return newWebdavRegularFile(ctx, path.Base(rel), fe, fs.client, fs.cache, fs.passphrase), nil
	}

	// Check if directory
	items := m.ListDirectory(rel)
	if len(items) > 0 || fs.isKnownDirectory(m, rel) {
		entries := make([]os.FileInfo, 0, len(items))
		for _, it := range items {
			mode := os.FileMode(0644)
			if it.IsDir {
				mode = os.ModeDir | 0755
			}
			entries = append(entries, &virtualFileInfo{
				name:    it.Name,
				size:    it.Size,
				mode:    mode,
				modTime: it.ModTime,
				isDir:   it.IsDir,
			})
		}
		return newWebdavDir(path.Base(rel), m.CreatedAt, entries), nil
	}

	return nil, os.ErrNotExist
}

// statInManifest returns FileInfo for a path inside a loaded manifest.
func (fs *VirtualFS) statInManifest(m *snapshot.SnapshotManifest, subPath string) (os.FileInfo, error) {
	rel := strings.TrimPrefix(path.Clean(subPath), "/")

	if rel == "" || rel == "." {
		return &virtualFileInfo{
			name:    path.Base(m.SnapshotID),
			size:    0,
			mode:    os.ModeDir | 0755,
			modTime: m.CreatedAt,
			isDir:   true,
		}, nil
	}

	if fe, found := m.Files[rel]; found {
		modTime := time.Unix(fe.ModTime, 0)
		if fe.ModTime == 0 {
			modTime = m.CreatedAt
		}
		return &virtualFileInfo{
			name:    path.Base(rel),
			size:    fe.Size,
			mode:    0644,
			modTime: modTime,
			isDir:   false,
		}, nil
	}

	items := m.ListDirectory(rel)
	if len(items) > 0 || fs.isKnownDirectory(m, rel) {
		return &virtualFileInfo{
			name:    path.Base(rel),
			size:    0,
			mode:    os.ModeDir | 0755,
			modTime: m.CreatedAt,
			isDir:   true,
		}, nil
	}

	return nil, os.ErrNotExist
}

// isKnownDirectory checks whether rel is recorded in m.Directories or prefix of a file.
func (fs *VirtualFS) isKnownDirectory(m *snapshot.SnapshotManifest, rel string) bool {
	prefix := rel + "/"
	for _, d := range m.Directories {
		if d == rel || strings.HasPrefix(d, prefix) {
			return true
		}
	}
	for p := range m.Files {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// getManifest loads a manifest by snapshot ID with caching.
func (fs *VirtualFS) getManifest(snapID string) (*snapshot.SnapshotManifest, error) {
	fs.mu.RLock()
	m, found := fs.manifestCache[snapID]
	fs.mu.RUnlock()
	if found {
		return m, nil
	}

	m, err := fs.engine.GetSnapshot(snapID)
	if err != nil {
		return nil, err
	}

	fs.mu.Lock()
	fs.manifestCache[snapID] = m
	fs.mu.Unlock()
	return m, nil
}

// getLatestManifest retrieves the latest snapshot manifest.
func (fs *VirtualFS) getLatestManifest() (*snapshot.SnapshotManifest, error) {
	if fs.engine == nil {
		return nil, errors.New("engine not configured")
	}
	return fs.engine.GetLatestSnapshot()
}
