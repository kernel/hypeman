//go:build linux

package uffdpager

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, HealthResponse{
		Version:        s.versionKey,
		Draining:       s.isDraining(),
		ActiveSessions: s.activeSessions(),
	})
}

func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	cacheBytes, cacheMax, cacheItems, hits, misses := s.cache.SnapshotStats()
	cacheShards, cacheLookupNanos, cacheLookupMaxNanos, cacheAddNanos, cacheAddMaxNanos := s.cache.SnapshotTimingStats()
	s.writeJSON(w, http.StatusOK, Stats{
		Version:             s.versionKey,
		Draining:            s.isDraining(),
		ActiveSessions:      s.activeSessions(),
		CacheBytes:          cacheBytes,
		CacheMax:            cacheMax,
		CacheItems:          cacheItems,
		CacheHits:           hits,
		CacheMisses:         misses,
		CacheShards:         cacheShards,
		CacheLookupNanos:    cacheLookupNanos,
		CacheLookupMaxNanos: cacheLookupMaxNanos,
		CacheAddNanos:       cacheAddNanos,
		CacheAddMaxNanos:    cacheAddMaxNanos,
		Faults:              s.faults.Load(),
		BackingBytesRead:    s.backingBytesRead.Load(),
		Copies:              s.copies.Load(),
		CopyErrors:          s.copyErrors.Load(),
		ActiveFaults:        s.activeFaults.Load(),
		MaxConcurrentFaults: s.maxConcurrentFaults.Load(),
		FaultNanos:          s.faultNanos.Load(),
		FaultMaxNanos:       s.faultMaxNanos.Load(),
		ReadPageNanos:       s.readPageNanos.Load(),
		ReadPageMaxNanos:    s.readPageMaxNanos.Load(),
		BackingReadNanos:    s.backingReadNanos.Load(),
		BackingReadMaxNanos: s.backingReadMaxNanos.Load(),
		CopyNanos:           s.copyNanos.Load(),
		CopyMaxNanos:        s.copyMaxNanos.Load(),
	})
}

func (s *server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if s.isDraining() {
		http.Error(w, "pager is draining", http.StatusServiceUnavailable)
		return
	}

	var req CreateSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.SessionID) == "" {
		req.SessionID = req.InstanceID
	}
	if strings.TrimSpace(req.SessionID) == "" {
		http.Error(w, "session_id or instance_id is required", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.BackingMemoryPath) == "" {
		http.Error(w, "backing_memory_path is required", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.CacheKey) == "" {
		http.Error(w, "cache_key is required", http.StatusBadRequest)
		return
	}

	created, err := s.createSession(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.writeJSON(w, http.StatusOK, CreateSessionResponse{
		SessionID:      created.id,
		UFFDSocketPath: created.socketPath,
		PagerVersion:   s.versionKey,
	})
}

func (s *server) handleCloseSession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	s.closeSession(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleDrain(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.draining = true
	shouldExit := len(s.sessions) == 0
	s.mu.Unlock()

	if shouldExit {
		s.shutdownSoon()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write json response: %v", err)
	}
}
