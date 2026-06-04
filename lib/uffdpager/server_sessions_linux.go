//go:build linux

package uffdpager

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func (s *server) createSession(req CreateSessionRequest) (*session, error) {
	id := sanitizeSessionID(req.SessionID)
	socketPath := filepath.Join(s.sessionRoot, id+".sock")
	_ = os.Remove(socketPath)
	addr := net.UnixAddr{Name: socketPath, Net: "unix"}
	listener, err := net.ListenUnix("unix", &addr)
	if err != nil {
		return nil, fmt.Errorf("listen for uffd session %s: %w", id, err)
	}
	backingFile, err := os.Open(req.BackingMemoryPath)
	if err != nil {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		return nil, fmt.Errorf("open backing memory for uffd session %s: %w", id, err)
	}

	sess := &session{
		id:                id,
		instanceID:        req.InstanceID,
		backingMemoryPath: req.BackingMemoryPath,
		cacheKey:          req.CacheKey,
		socketPath:        socketPath,
		listener:          listener,
		server:            s,
		done:              make(chan struct{}),
		backingFile:       backingFile,
		uffdFD:            -1,
	}

	s.mu.Lock()
	if existing := s.sessions[id]; existing != nil {
		existing.close()
	}
	s.sessions[id] = sess
	s.mu.Unlock()

	go sess.run()
	return sess, nil
}

func (s *server) closeSession(id string) {
	id = sanitizeSessionID(id)
	s.mu.Lock()
	sess := s.sessions[id]
	delete(s.sessions, id)
	shouldExit := s.draining && len(s.sessions) == 0
	s.mu.Unlock()
	if sess != nil {
		sess.close()
	}
	if shouldExit {
		s.shutdownSoon()
	}
}

func (s *server) removeSession(sess *session) {
	s.mu.Lock()
	if current := s.sessions[sess.id]; current == sess {
		delete(s.sessions, sess.id)
	}
	shouldExit := s.draining && len(s.sessions) == 0
	s.mu.Unlock()
	if shouldExit {
		s.shutdownSoon()
	}
}

func (s *server) shutdownSoon() {
	go func() {
		time.Sleep(50 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.httpServer.Shutdown(ctx)
	}()
}

func (s *server) activeSessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

func (s *server) isDraining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}

func (s *session) run() {
	defer func() {
		s.close()
		s.server.removeSession(s)
	}()

	conn, err := s.listener.AcceptUnix()
	if err != nil {
		return
	}
	s.conn = conn

	if s.backingFile == nil {
		file, err := os.Open(s.backingMemoryPath)
		if err != nil {
			log.Printf("uffd session %s open backing memory: %v", s.id, err)
			return
		}
		s.backingFile = file
	}

	mappings, uffdFD, err := recvMappingsAndFD(conn)
	if err != nil {
		log.Printf("uffd session %s receive mappings and fd: %v", s.id, err)
		return
	}
	s.uffdFD = uffdFD

	sort.Slice(mappings, func(i, j int) bool {
		return mappings[i].BaseHostVirtAddr < mappings[j].BaseHostVirtAddr
	})
	s.handleFaults(mappings)
}

func (s *session) close() {
	s.closeOnce.Do(func() {
		if s.listener != nil {
			_ = s.listener.Close()
		}
		if s.conn != nil {
			_ = s.conn.Close()
		}
		if s.uffdFD >= 0 {
			_ = unix.Close(s.uffdFD)
		}
		if s.backingFile != nil {
			_ = s.backingFile.Close()
		}
		_ = os.Remove(s.socketPath)
		close(s.done)
	})
}

func sanitizeSessionID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Sprintf("session-%d", time.Now().UnixNano())
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, id)
}
