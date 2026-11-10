package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/config"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/qos"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/restore"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/snapshot"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/telegram"
	"github.com/thanhvo-agy/Telegram-Drive/web"
)

// Server represents the embedded web dashboard server
type Server struct {
	cfg        *config.Config
	engine     *snapshot.Engine
	restorer   *restore.Restorer
	tgClient   *telegram.Client
	throttler  *qos.BandwidthThrottler
	httpServer *http.Server

	mu          sync.RWMutex
	isBackingUp bool
	isRestoring bool
	lastBackup  *snapshot.ProgressEvent
	lastRestore *restore.RestoreProgress
	sseClients  map[chan []byte]bool
	sseMu       sync.Mutex
}

// NewServer initializes the embedded dashboard server
func NewServer(cfg *config.Config, engine *snapshot.Engine, restorer *restore.Restorer, tgClient *telegram.Client, throttler *qos.BandwidthThrottler) *Server {
	return &Server{
		cfg:        cfg,
		engine:     engine,
		restorer:   restorer,
		tgClient:   tgClient,
		throttler:  throttler,
		sseClients: make(map[chan []byte]bool),
	}
}

// Start launches the HTTP server
func (s *Server) Start(port int) error {
	if port <= 0 {
		port = s.cfg.ServerPort
	}
	if port <= 0 {
		port = 8088
	}

	mux := http.NewServeMux()

	// REST API Endpoints
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/snapshots", s.handleSnapshots)
	mux.HandleFunc("/api/snapshots/diff", s.handleSnapshotDiff)
	mux.HandleFunc("/api/snapshots/", s.handleSnapshotDetail)
	mux.HandleFunc("/api/backup/trigger", s.handleTriggerBackup)
	mux.HandleFunc("/api/restore", s.handleTriggerRestore)
	mux.HandleFunc("/api/file/history", s.handleFileHistory)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/bots", s.handleBots)
	mux.HandleFunc("/api/events", s.handleSSE)

	// Embedded Static UI
	mux.Handle("/", web.Handler())

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	return s.httpServer.ListenAndServe()
}

// Stop gracefully shuts down the server
func (s *Server) Stop(ctx context.Context) error {
	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}
	return nil
}

func (s *Server) broadcastSSE(event string, data interface{}) {
	payload, err := json.Marshal(map[string]interface{}{
		"event": event,
		"data":  data,
	})
	if err != nil {
		return
	}

	msg := fmt.Sprintf("event: %s\ndata: %s\n\n", event, string(payload))
	b := []byte(msg)

	s.sseMu.Lock()
	defer s.sseMu.Unlock()
	for ch := range s.sseClients {
		select {
		case ch <- b:
		default:
		}
	}
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	msgCh := make(chan []byte, 16)
	s.sseMu.Lock()
	s.sseClients[msgCh] = true
	s.sseMu.Unlock()

	defer func() {
		s.sseMu.Lock()
		delete(s.sseClients, msgCh)
		close(msgCh)
		s.sseMu.Unlock()
	}()

	notify := r.Context().Done()
	for {
		select {
		case <-notify:
			return
		case msg := <-msgCh:
			_, _ = w.Write(msg)
			flusher.Flush()
		}
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snapshots, _ := eSafe(s.engine.ListSnapshots())
	var totalSnapshots int
	var totalStorageBytes int64
	var latestTime time.Time

	if len(snapshots) > 0 {
		totalSnapshots = len(snapshots)
		totalStorageBytes = snapshots[0].TotalBytes
		latestTime = snapshots[0].CreatedAt
	}

	currentBwLimit := int64(0)
	if s.throttler != nil {
		currentBwLimit = s.throttler.CurrentLimitBytes()
	}

	resp := map[string]interface{}{
		"node_name":           s.cfg.NodeName,
		"node_mode":           s.cfg.NodeMode,
		"source_path":         s.cfg.SourcePath,
		"is_backing_up":       s.isBackingUp,
		"is_restoring":        s.isRestoring,
		"total_snapshots":     totalSnapshots,
		"total_storage_bytes": totalStorageBytes,
		"latest_snapshot_at":  latestTime,
		"active_bandwidth":    currentBwLimit,
		"bot_mesh_count":      len(s.cfg.BotTokens),
		"last_backup_event":   s.lastBackup,
		"last_restore_event":  s.lastRestore,
	}

	jsonResponse(w, resp)
}

func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	list, err := s.engine.ListSnapshots()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, list)
}

func (s *Server) handleSnapshotDetail(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/snapshots/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "snapshot id required", http.StatusBadRequest)
		return
	}

	snapID := parts[0]
	if len(parts) > 1 && parts[1] == "tree" {
		// File Station virtual directory navigation
		subPath := r.URL.Query().Get("path")
		m, err := s.engine.GetSnapshot(snapID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		items := m.ListDirectory(subPath)
		jsonResponse(w, map[string]interface{}{
			"snapshot_id": snapID,
			"path":        subPath,
			"items":       items,
		})
		return
	}

	// Full manifest
	m, err := s.engine.GetSnapshot(snapID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonResponse(w, m)
}

func (s *Server) handleSnapshotDiff(w http.ResponseWriter, r *http.Request) {
	snapA := r.URL.Query().Get("a")
	snapB := r.URL.Query().Get("b")

	if snapA == "" || snapB == "" {
		http.Error(w, "query params 'a' and 'b' required", http.StatusBadRequest)
		return
	}

	mA, err := s.engine.GetSnapshot(snapA)
	if err != nil {
		http.Error(w, fmt.Sprintf("snapshot %s: %v", snapA, err), http.StatusNotFound)
		return
	}

	mB, err := s.engine.GetSnapshot(snapB)
	if err != nil {
		http.Error(w, fmt.Sprintf("snapshot %s: %v", snapB, err), http.StatusNotFound)
		return
	}

	diffs := snapshot.DiffManifests(mA, mB)
	jsonResponse(w, map[string]interface{}{
		"snapshot_a": snapA,
		"snapshot_b": snapB,
		"diffs":      diffs,
	})
}

func (s *Server) handleFileHistory(w http.ResponseWriter, r *http.Request) {
	relPath := r.URL.Query().Get("path")
	if relPath == "" {
		http.Error(w, "query param 'path' required", http.StatusBadRequest)
		return
	}

	history, err := s.engine.GetFileHistory(relPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, history)
}

func (s *Server) handleTriggerBackup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	if s.isBackingUp {
		s.mu.Unlock()
		http.Error(w, "Backup is already in progress", http.StatusConflict)
		return
	}
	s.isBackingUp = true
	s.mu.Unlock()

	go func() {
		ctx := context.Background()
		_, err := s.engine.CreateSnapshot(ctx, func(p snapshot.ProgressEvent) {
			s.mu.Lock()
			s.lastBackup = &p
			s.mu.Unlock()
			s.broadcastSSE("backup_progress", p)
		})

		s.mu.Lock()
		s.isBackingUp = false
		s.mu.Unlock()

		if err != nil {
			s.broadcastSSE("backup_error", err.Error())
		} else {
			s.broadcastSSE("backup_complete", true)
		}
	}()

	jsonResponse(w, map[string]string{"status": "started"})
}

func (s *Server) handleTriggerRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		SnapshotID string `json:"snapshot_id"`
		Path       string `json:"path"`
		TargetDir  string `json:"target_dir"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.SnapshotID == "" || req.TargetDir == "" {
		http.Error(w, "snapshot_id and target_dir are required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	if s.isRestoring {
		s.mu.Unlock()
		http.Error(w, "Restore is already in progress", http.StatusConflict)
		return
	}
	s.isRestoring = true
	s.mu.Unlock()

	go func() {
		ctx := context.Background()
		var err error
		if req.Path != "" && !strings.HasSuffix(req.Path, "/") {
			// Restore single file
			destFile := filepath.Join(req.TargetDir, filepath.Base(req.Path))
			err = s.restorer.RestoreFile(ctx, req.SnapshotID, req.Path, destFile)
		} else {
			// Restore directory or full snapshot
			err = s.restorer.RestoreDirectory(ctx, req.SnapshotID, req.Path, req.TargetDir, func(p restore.RestoreProgress) {
				s.mu.Lock()
				s.lastRestore = &p
				s.mu.Unlock()
				s.broadcastSSE("restore_progress", p)
			})
		}

		s.mu.Lock()
		s.isRestoring = false
		s.mu.Unlock()

		if err != nil {
			s.broadcastSSE("restore_error", err.Error())
		} else {
			s.broadcastSSE("restore_complete", true)
		}
	}()

	jsonResponse(w, map[string]string{"status": "started"})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		// Return config sanitized (hide tokens and encryption key)
		sanitized := *s.cfg
		sanitized.EncryptionKey = "********"
		var maskedTokens []string
		for _, tok := range s.cfg.BotTokens {
			if len(tok) > 10 {
				maskedTokens = append(maskedTokens, tok[:6]+"..."+tok[len(tok)-4:])
			} else {
				maskedTokens = append(maskedTokens, "********")
			}
		}
		sanitized.BotTokens = maskedTokens
		jsonResponse(w, sanitized)
		return
	}

	if r.Method == http.MethodPost {
		var updated config.Config
		if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Only apply non-sensitive/updatable fields if keys are masked
		if updated.NodeName != "" {
			s.cfg.NodeName = updated.NodeName
		}
		if updated.NodeMode != "" {
			s.cfg.NodeMode = updated.NodeMode
		}
		if updated.SourcePath != "" {
			s.cfg.SourcePath = updated.SourcePath
		}
		if updated.BandwidthSchedule != "" {
			s.cfg.BandwidthSchedule = updated.BandwidthSchedule
			if th, err := qos.ParseBandwidthSchedule(updated.BandwidthSchedule); err == nil {
				s.throttler = th
			}
		}
		if updated.Retention.KeepHourly > 0 {
			s.cfg.Retention = updated.Retention
		}
		if updated.EncryptionKey != "" && updated.EncryptionKey != "********" {
			s.cfg.EncryptionKey = updated.EncryptionKey
		}
		if len(updated.BotTokens) > 0 && !strings.Contains(updated.BotTokens[0], "...") {
			s.cfg.BotTokens = updated.BotTokens
		}

		_ = s.cfg.Save("")
		jsonResponse(w, map[string]string{"status": "saved"})
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

func (s *Server) handleBots(w http.ResponseWriter, r *http.Request) {
	if s.tgClient == nil {
		jsonResponse(w, []telegram.BotStatus{})
		return
	}
	jsonResponse(w, s.tgClient.GetMeshStatus())
}

func jsonResponse(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}

func eSafe(list []snapshot.SnapshotSummary, err error) ([]snapshot.SnapshotSummary, error) {
	return list, err
}
