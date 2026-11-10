package webdav

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/webdav"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/snapshot"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/telegram"
)

// ServerConfig configures the WebDAV RFC 4918 HTTP server.
type ServerConfig struct {
	Addr        string
	Port        int
	ReadOnly    bool
	CacheSize   int64 // In-memory cache size in chunks (or MB)
	Username    string
	Password    string
	SnapshotID  string // Optional specific snapshot ID to mount; empty for multi-snapshot
	IdleTimeout time.Duration
}

// Server wraps golang.org/x/net/webdav.Handler with OS compatibility headers,
// authentication, read-only guards, and lifecycle management.
type Server struct {
	cfg        ServerConfig
	engine     *snapshot.Engine
	client     *telegram.Client
	fs         *VirtualFS
	cache      *ChunkCache
	handler    *webdav.Handler
	httpServer *http.Server
	listener   net.Listener
	mu         sync.Mutex
	running    bool
}

// NewServer initializes a new RFC 4918 compliant WebDAV server.
func NewServer(cfg ServerConfig, engine *snapshot.Engine, client *telegram.Client, encryptionKey string) (*Server, error) {
	if cfg.Port <= 0 {
		cfg.Port = 8089
	}
	if cfg.Addr == "" {
		cfg.Addr = "0.0.0.0"
	}
	// Default to read-only for snapshot mounts
	if !cfg.ReadOnly {
		cfg.ReadOnly = true
	}

	maxChunks := int(cfg.CacheSize)
	if maxChunks <= 0 {
		maxChunks = 4 // Default 4 chunks (approx 196 MB)
	}

	idleTimeout := cfg.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = 60 * time.Second
	}

	cache := NewChunkCache(maxChunks, idleTimeout)
	vfs := NewVirtualFS(engine, client, cache, encryptionKey, cfg.SnapshotID, cfg.ReadOnly)

	wdHandler := &webdav.Handler{
		Prefix:     "",
		FileSystem: vfs,
		LockSystem: webdav.NewMemLS(),
	}

	srv := &Server{
		cfg:     cfg,
		engine:  engine,
		client:  client,
		fs:      vfs,
		cache:   cache,
		handler: wdHandler,
	}

	return srv, nil
}

// ServeHTTP handles incoming HTTP requests, applying OS mount compatibility headers,
// optional HTTP Basic Auth, and read-only mutation guards.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. Mandatory OS compatibility headers (Windows Explorer & macOS Finder)
	w.Header().Set("DAV", "1, 2")
	w.Header().Set("MS-Author-Via", "DAV")
	w.Header().Set("Accept-Ranges", "bytes")

	// 2. HTTP Basic Authentication check
	if s.cfg.Username != "" && s.cfg.Password != "" {
		user, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(user), []byte(s.cfg.Username)) != 1 ||
			subtle.ConstantTimeCompare([]byte(pass), []byte(s.cfg.Password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="tgdrive"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	// 3. Fast rejection of metadata probes for macOS and Windows
	cleanPath := cleanReqPath(r.URL.Path)
	if isMetadataProbe(cleanPath) {
		if r.Method == "PUT" || r.Method == "POST" || r.Method == "MKCOL" {
			// macOS Finder attempts to create .DS_Store: return 403 Forbidden in read-only mode
			http.Error(w, "Forbidden: read-only filesystem", http.StatusForbidden)
			return
		}
		http.NotFound(w, r)
		return
	}

	// 4. Read-only guard: Reject modifying HTTP methods
	if s.cfg.ReadOnly {
		switch r.Method {
		case "PUT", "POST", "DELETE", "MKCOL", "MOVE", "COPY", "PROPPATCH":
			http.Error(w, "Forbidden: snapshot is read-only", http.StatusForbidden)
			return
		case "LOCK":
			// Deny locking in read-only mode
			http.Error(w, "Forbidden: locked resources cannot be created on read-only mount", http.StatusForbidden)
			return
		}
	}

	// 5. Handle OPTIONS explicitly with comprehensive RFC 4918 header set
	if r.Method == "OPTIONS" {
		allowed := "OPTIONS, GET, HEAD, PROPFIND"
		if !s.cfg.ReadOnly {
			allowed += ", PUT, DELETE, MKCOL, MOVE, COPY, PROPPATCH, LOCK, UNLOCK"
		}
		w.Header().Set("Allow", allowed)
		w.WriteHeader(http.StatusOK)
		return
	}

	// 6. Handle HEAD request for directory root or subdirectories
	if r.Method == "HEAD" && (cleanPath == "/" || cleanPath == "" || cleanPath == "/latest" || cleanPath == "/latest/") {
		w.WriteHeader(http.StatusOK)
		return
	}

	// 7. Dispatch to standard webdav.Handler
	s.handler.ServeHTTP(w, r)
}

// Start launches the WebDAV server listener on the configured address and port.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil
	}

	addr := fmt.Sprintf("%s:%d", s.cfg.Addr, s.cfg.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	s.listener = ln
	s.httpServer = &http.Server{
		Handler:      s,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	s.running = true
	s.mu.Unlock()

	errCh := make(chan error, 1)
	go func() {
		err := s.httpServer.Serve(ln)
		if err != nil && err != http.ErrServerClosed {
			errCh <- err
		} else {
			errCh <- nil
		}
	}()

	select {
	case <-ctx.Done():
		_ = s.Stop()
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

// ListenAndServe starts the server synchronously without context.
func (s *Server) ListenAndServe() error {
	return s.Start(context.Background())
}

// Stop gracefully shuts down the WebDAV server and purges the chunk cache.
func (s *Server) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.Shutdown(ctx)
}

// Shutdown gracefully shuts down the WebDAV server with the provided context.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return nil
	}
	s.running = false

	var srvErr error
	if s.httpServer != nil {
		srvErr = s.httpServer.Shutdown(ctx)
	}
	if s.cache != nil {
		_ = s.cache.Close()
	}
	return srvErr
}

// Addr returns the listening network address.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return fmt.Sprintf("%s:%d", s.cfg.Addr, s.cfg.Port)
}

// VirtualFS returns the underlying virtual filesystem.
func (s *Server) VirtualFS() *VirtualFS {
	return s.fs
}

// ChunkCache returns the active LRU chunk cache.
func (s *Server) ChunkCache() *ChunkCache {
	return s.cache
}
