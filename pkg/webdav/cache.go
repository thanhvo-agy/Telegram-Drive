package webdav

import (
	"container/list"
	"context"
	"errors"
	"runtime"
	"runtime/debug"
	"sync"
	"time"
)

var (
	ErrCacheClosed = errors.New("chunk cache is closed")
)

// cacheEntry holds cached chunk data and its lookup key
type cacheEntry struct {
	key  string
	data []byte
}

// flightCall represents an in-flight chunk download/decryption request
type flightCall struct {
	wg  sync.WaitGroup
	val []byte
	err error
}

// ChunkCache provides an in-memory LRU cache with single-flight request
// deduplication and automatic idle eviction to maintain 0 MB idle RAM overhead.
type ChunkCache struct {
	mu           sync.Mutex
	maxChunks    int
	idleTimeout  time.Duration
	idleTimer    *time.Timer
	items        map[string]*list.Element
	evictList    *list.List
	singleFlight map[string]*flightCall
	totalBytes   int64
	closed       bool
}

// NewChunkCache creates an LRU chunk cache. If idleTimeout is <= 0, a default
// of 60 seconds is used. When idle, all in-memory chunks are evicted and memory
// returned to the OS so idle RAM overhead stays at 0 MB.
func NewChunkCache(maxChunks int, idleTimeout time.Duration) *ChunkCache {
	if maxChunks <= 0 {
		maxChunks = 4 // Default: 4 chunks (approx 196 MB)
	}
	if idleTimeout <= 0 {
		idleTimeout = 60 * time.Second
	}

	c := &ChunkCache{
		maxChunks:    maxChunks,
		idleTimeout:  idleTimeout,
		items:        make(map[string]*list.Element),
		evictList:    list.New(),
		singleFlight: make(map[string]*flightCall),
	}

	c.resetIdleTimerLocked()
	return c
}

// resetIdleTimerLocked restarts the idle eviction timer. Caller must hold c.mu.
func (c *ChunkCache) resetIdleTimerLocked() {
	if c.closed {
		return
	}
	if c.idleTimer != nil {
		c.idleTimer.Stop()
	}
	c.idleTimer = time.AfterFunc(c.idleTimeout, c.onIdleTimeout)
}

// onIdleTimeout is triggered when no read or write requests occur for idleTimeout.
// It purges all cached chunks to fulfill the 0 MB idle RAM overhead constraint.
func (c *ChunkCache) onIdleTimeout() {
	c.mu.Lock()
	if c.closed || len(c.singleFlight) > 0 {
		c.mu.Unlock()
		return
	}
	c.purgeLocked()
	c.mu.Unlock()

	// Release memory aggressively to OS
	runtime.GC()
	debug.FreeOSMemory()
}

// purgeLocked removes all elements from cache. Caller must hold c.mu.
func (c *ChunkCache) purgeLocked() {
	c.items = make(map[string]*list.Element)
	c.evictList.Init()
	c.totalBytes = 0
}

// Get retrieves a decrypted chunk from cache if present.
func (c *ChunkCache) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, false
	}

	c.resetIdleTimerLocked()

	if elem, found := c.items[key]; found {
		c.evictList.MoveToFront(elem)
		entry := elem.Value.(*cacheEntry)
		return entry.data, true
	}
	return nil, false
}

// Put stores a decrypted chunk in the cache, evicting the least-recently used
// entry if capacity is exceeded.
func (c *ChunkCache) Put(key string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return
	}

	c.resetIdleTimerLocked()

	if elem, found := c.items[key]; found {
		c.evictList.MoveToFront(elem)
		entry := elem.Value.(*cacheEntry)
		c.totalBytes += int64(len(data) - len(entry.data))
		entry.data = data
		return
	}

	for c.evictList.Len() >= c.maxChunks && c.evictList.Len() > 0 {
		c.evictOldestLocked()
	}

	entry := &cacheEntry{key: key, data: data}
	elem := c.evictList.PushFront(entry)
	c.items[key] = elem
	c.totalBytes += int64(len(data))
}

// evictOldestLocked removes the LRU entry. Caller must hold c.mu.
func (c *ChunkCache) evictOldestLocked() {
	elem := c.evictList.Back()
	if elem == nil {
		return
	}
	c.evictList.Remove(elem)
	entry := elem.Value.(*cacheEntry)
	delete(c.items, entry.key)
	c.totalBytes -= int64(len(entry.data))
}

// GetOrFetch retrieves the chunk if cached, or calls fetch() exactly once
// across concurrent callers requesting the same chunk key via single-flight mutex.
func (c *ChunkCache) GetOrFetch(ctx context.Context, key string, fetch func() ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrCacheClosed
	}

	c.resetIdleTimerLocked()

	// Check cache hit
	if elem, found := c.items[key]; found {
		c.evictList.MoveToFront(elem)
		entry := elem.Value.(*cacheEntry)
		data := entry.data
		c.mu.Unlock()
		return data, nil
	}

	// Check if another goroutine is already fetching this chunk
	if call, inflight := c.singleFlight[key]; inflight {
		c.mu.Unlock()

		done := make(chan struct{})
		go func() {
			call.wg.Wait()
			close(done)
		}()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-done:
			return call.val, call.err
		}
	}

	// Register this fetch as the active single-flight leader
	call := &flightCall{}
	call.wg.Add(1)
	c.singleFlight[key] = call
	c.mu.Unlock()

	// Execute fetch outside the lock
	data, err := fetch()

	c.mu.Lock()
	call.val = data
	call.err = err
	delete(c.singleFlight, key)

	if err == nil && len(data) > 0 && !c.closed {
		for c.evictList.Len() >= c.maxChunks && c.evictList.Len() > 0 {
			c.evictOldestLocked()
		}
		entry := &cacheEntry{key: key, data: data}
		elem := c.evictList.PushFront(entry)
		c.items[key] = elem
		c.totalBytes += int64(len(data))
	}
	c.resetIdleTimerLocked()
	c.mu.Unlock()

	call.wg.Done()
	return data, err
}

// Clear purges all cached chunks immediately.
func (c *ChunkCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purgeLocked()
	runtime.GC()
	debug.FreeOSMemory()
}

// Close shuts down the cache and stops the idle eviction timer.
func (c *ChunkCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.idleTimer != nil {
		c.idleTimer.Stop()
		c.idleTimer = nil
	}
	c.purgeLocked()
	return nil
}

// Len returns the current number of cached chunks.
func (c *ChunkCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.evictList.Len()
}

// ByteSize returns the total bytes occupied by cached chunks.
func (c *ChunkCache) ByteSize() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.totalBytes
}
