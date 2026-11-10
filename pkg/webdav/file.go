package webdav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/crypto"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/snapshot"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/telegram"
)

// virtualFileInfo implements os.FileInfo for virtual WebDAV files and directories
type virtualFileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
	isDir   bool
}

func (v *virtualFileInfo) Name() string       { return v.name }
func (v *virtualFileInfo) Size() int64        { return v.size }
func (v *virtualFileInfo) Mode() os.FileMode  { return v.mode }
func (v *virtualFileInfo) ModTime() time.Time { return v.modTime }
func (v *virtualFileInfo) IsDir() bool        { return v.isDir }
func (v *virtualFileInfo) Sys() interface{}   { return nil }

// webdavRegularFile implements webdav.File and io.ReadSeeker for snapshot files,
// providing on-the-fly streaming chunk decryption and LRU caching.
type webdavRegularFile struct {
	ctx        context.Context
	info       virtualFileInfo
	chunks     []snapshot.ChunkRef
	client     *telegram.Client
	cache      *ChunkCache
	passphrase string

	mu     sync.Mutex
	offset int64
	closed bool
}

// newWebdavRegularFile creates a virtual file backed by encrypted Telegram chunks.
func newWebdavRegularFile(ctx context.Context, name string, fe snapshot.FileEntry, client *telegram.Client, cache *ChunkCache, passphrase string) *webdavRegularFile {
	modTime := time.Unix(fe.ModTime, 0)
	if fe.ModTime == 0 {
		modTime = time.Now()
	}

	return &webdavRegularFile{
		ctx: ctx,
		info: virtualFileInfo{
			name:    name,
			size:    fe.Size,
			mode:    0644,
			modTime: modTime,
			isDir:   false,
		},
		chunks:     fe.Chunks,
		client:     client,
		cache:      cache,
		passphrase: passphrase,
	}
}

// Close closes the virtual file.
func (f *webdavRegularFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// Stat returns the file metadata.
func (f *webdavRegularFile) Stat() (os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, os.ErrClosed
	}
	return &f.info, nil
}

// Readdir returns an error because regular files cannot be read as directories.
func (f *webdavRegularFile) Readdir(count int) ([]os.FileInfo, error) {
	return nil, errors.New("cannot Readdir on regular file")
}

// Seek sets the logical cursor offset in O(1) time with zero network requests.
func (f *webdavRegularFile) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return 0, os.ErrClosed
	}

	var newOffset int64
	switch whence {
	case io.SeekStart:
		newOffset = offset
	case io.SeekCurrent:
		newOffset = f.offset + offset
	case io.SeekEnd:
		newOffset = f.info.size + offset
	default:
		return 0, errors.New("invalid seek whence")
	}

	if newOffset < 0 {
		return 0, errors.New("negative seek offset")
	}

	f.offset = newOffset
	return newOffset, nil
}

// Read maps current cursor offset to chunk(s), downloading and decrypting ONLY
// the required chunk(s) via the LRU cache. It supports reading across chunk boundaries.
func (f *webdavRegularFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return 0, os.ErrClosed
	}

	if len(p) == 0 {
		return 0, nil
	}

	// EOF condition
	if f.offset >= f.info.size || f.info.size == 0 {
		return 0, io.EOF
	}

	totalRead := 0

	for len(p) > 0 && f.offset < f.info.size {
		chunkIdx := findChunkIndex(f.chunks, f.offset)
		if chunkIdx < 0 || chunkIdx >= len(f.chunks) {
			break
		}

		chunk := f.chunks[chunkIdx]
		chunkRelOffset := f.offset - chunk.Offset
		if chunkRelOffset < 0 {
			break
		}

		// Fetch chunk data through cache
		plainData, err := f.fetchChunk(chunk)
		if err != nil {
			if totalRead > 0 {
				return totalRead, nil
			}
			return 0, err
		}

		if chunkRelOffset >= int64(len(plainData)) {
			break
		}

		availableInChunk := int64(len(plainData)) - chunkRelOffset
		toRead := int64(len(p))
		if toRead > availableInChunk {
			toRead = availableInChunk
		}

		remainingInFile := f.info.size - f.offset
		if toRead > remainingInFile {
			toRead = remainingInFile
		}

		copy(p[:toRead], plainData[chunkRelOffset:chunkRelOffset+toRead])
		p = p[toRead:]
		f.offset += toRead
		totalRead += int(toRead)
	}

	if totalRead == 0 {
		if f.offset >= f.info.size {
			return 0, io.EOF
		}
		return 0, io.ErrUnexpectedEOF
	}

	return totalRead, nil
}

// Write returns os.ErrPermission because snapshot files are immutable and read-only.
func (f *webdavRegularFile) Write(p []byte) (int, error) {
	return 0, os.ErrPermission
}

// fetchChunk retrieves decrypted chunk data via LRU cache and Telegram client.
func (f *webdavRegularFile) fetchChunk(chunk snapshot.ChunkRef) ([]byte, error) {
	if f.cache == nil {
		return f.downloadAndDecryptChunk(chunk)
	}

	cacheKey := chunk.TelegramFileID
	if cacheKey == "" {
		cacheKey = fmt.Sprintf("chunk_%d_%d", chunk.Index, chunk.Offset)
	}

	return f.cache.GetOrFetch(f.ctx, cacheKey, func() ([]byte, error) {
		return f.downloadAndDecryptChunk(chunk)
	})
}

// downloadAndDecryptChunk performs network fetch and cryptographic decryption.
func (f *webdavRegularFile) downloadAndDecryptChunk(chunk snapshot.ChunkRef) ([]byte, error) {
	if f.client == nil {
		return nil, errors.New("telegram client is not configured")
	}

	encData, err := f.client.DownloadFile(f.ctx, chunk.TelegramFileID)
	if err != nil {
		return nil, fmt.Errorf("download chunk %d: %w", chunk.Index, err)
	}

	plainData, err := crypto.DecryptChunk(encData, f.passphrase)
	if err != nil {
		return nil, fmt.Errorf("decrypt chunk %d: %w", chunk.Index, err)
	}

	if chunk.Hash != "" {
		actualHash := crypto.HashData(plainData)
		if actualHash != chunk.Hash {
			return nil, fmt.Errorf("chunk %d checksum mismatch: expected %s, got %s", chunk.Index, chunk.Hash, actualHash)
		}
	}

	return plainData, nil
}

// findChunkIndex uses binary search to find the chunk covering the requested offset.
func findChunkIndex(chunks []snapshot.ChunkRef, offset int64) int {
	if len(chunks) == 0 {
		return -1
	}

	idx := sort.Search(len(chunks), func(i int) bool {
		return chunks[i].Offset+chunks[i].Size > offset
	})

	if idx < len(chunks) && chunks[idx].Offset <= offset {
		return idx
	}
	return -1
}

// webdavDir implements webdav.File for virtual directories.
type webdavDir struct {
	info    virtualFileInfo
	entries []os.FileInfo

	mu     sync.Mutex
	cursor int
	closed bool
}

// newWebdavDir creates a virtual directory file.
func newWebdavDir(name string, modTime time.Time, entries []os.FileInfo) *webdavDir {
	if modTime.IsZero() {
		modTime = time.Now()
	}
	return &webdavDir{
		info: virtualFileInfo{
			name:    name,
			size:    0,
			mode:    os.ModeDir | 0755,
			modTime: modTime,
			isDir:   true,
		},
		entries: entries,
	}
}

func (d *webdavDir) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	return nil
}

func (d *webdavDir) Stat() (os.FileInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, os.ErrClosed
	}
	return &d.info, nil
}

func (d *webdavDir) Read(p []byte) (int, error) {
	return 0, io.EOF
}

// Write returns os.ErrPermission because virtual directories are immutable and read-only.
func (d *webdavDir) Write(p []byte) (int, error) {
	return 0, os.ErrPermission
}

func (d *webdavDir) Seek(offset int64, whence int) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return 0, os.ErrClosed
	}
	if offset == 0 && whence == io.SeekStart {
		d.cursor = 0
		return 0, nil
	}
	return 0, errors.New("cannot seek directory")
}

func (d *webdavDir) Readdir(count int) ([]os.FileInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return nil, os.ErrClosed
	}

	if count <= 0 {
		// Return all remaining entries
		remaining := d.entries[d.cursor:]
		d.cursor = len(d.entries)
		return remaining, nil
	}

	if d.cursor >= len(d.entries) {
		return nil, io.EOF
	}

	end := d.cursor + count
	if end > len(d.entries) {
		end = len(d.entries)
	}

	result := d.entries[d.cursor:end]
	d.cursor = end
	return result, nil
}
