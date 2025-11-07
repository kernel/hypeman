package images

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// Build status constants
const (
	StatusPending    = "pending"
	StatusPulling    = "pulling"
	StatusUnpacking  = "unpacking"
	StatusConverting = "converting"
	StatusReady      = "ready"
	StatusFailed     = "failed"
)

// ProgressUpdate represents a status update during image build
type ProgressUpdate struct {
	Status        string  `json:"status"`
	Progress      int     `json:"progress"`
	QueuePosition *int    `json:"queue_position,omitempty"`
	Error         *string `json:"error,omitempty"`
}

// ProgressTracker tracks build progress and broadcasts updates to SSE subscribers
type ProgressTracker struct {
	imageID     string
	dataDir     string
	subscribers []chan ProgressUpdate
	mu          sync.RWMutex
	closed      bool
}

// NewProgressTracker creates a new progress tracker
func NewProgressTracker(imageID, dataDir string) *ProgressTracker {
	return &ProgressTracker{
		imageID:     imageID,
		dataDir:     dataDir,
		subscribers: make([]chan ProgressUpdate, 0),
	}
}

// Update updates the progress and broadcasts to all subscribers
func (p *ProgressTracker) Update(status string, progress int, queuePos *int) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.closed {
		return
	}

	// Update metadata on disk
	meta, err := readMetadata(p.dataDir, p.imageID)
	if err != nil {
		return // Best effort
	}

	meta.Status = status
	meta.Progress = progress
	meta.QueuePosition = queuePos
	writeMetadata(p.dataDir, p.imageID, meta)

	// Broadcast to subscribers
	update := ProgressUpdate{
		Status:        status,
		Progress:      progress,
		QueuePosition: queuePos,
	}

	for _, ch := range p.subscribers {
		select {
		case ch <- update:
		default:
			// Non-blocking send (skip slow consumers)
		}
	}
}

// Fail marks the build as failed with error message
func (p *ProgressTracker) Fail(err error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.closed {
		return
	}

	meta, metaErr := readMetadata(p.dataDir, p.imageID)
	if metaErr != nil {
		return
	}

	meta.Status = StatusFailed
	meta.Progress = 0
	meta.QueuePosition = nil
	errorMsg := err.Error()
	meta.Error = &errorMsg
	writeMetadata(p.dataDir, p.imageID, meta)

	// Broadcast failure
	update := ProgressUpdate{
		Status: StatusFailed,
		Error:  &errorMsg,
	}

	for _, ch := range p.subscribers {
		select {
		case ch <- update:
		default:
		}
	}
}

// Complete marks the build as complete
func (p *ProgressTracker) Complete() {
	p.Update(StatusReady, 100, nil)
}

// Subscribe adds a new SSE subscriber and returns their channel
func (p *ProgressTracker) Subscribe(ctx context.Context) (chan ProgressUpdate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, fmt.Errorf("tracker closed")
	}

	ch := make(chan ProgressUpdate, 10) // Buffered for slow consumers
	p.subscribers = append(p.subscribers, ch)

	// Send current state immediately
	meta, err := readMetadata(p.dataDir, p.imageID)
	if err == nil {
		update := ProgressUpdate{
			Status:        meta.Status,
			Progress:      meta.Progress,
			QueuePosition: meta.QueuePosition,
			Error:         meta.Error,
		}
		ch <- update
	}

	// Close channel when context is done
	go func() {
		<-ctx.Done()
		p.Unsubscribe(ch)
	}()

	return ch, nil
}

// Unsubscribe removes a subscriber
func (p *ProgressTracker) Unsubscribe(ch chan ProgressUpdate) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i, sub := range p.subscribers {
		if sub == ch {
			p.subscribers = append(p.subscribers[:i], p.subscribers[i+1:]...)
			close(ch)
			break
		}
	}
}

// Close closes all subscriber channels
func (p *ProgressTracker) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}

	p.closed = true
	for _, ch := range p.subscribers {
		close(ch)
	}
	p.subscribers = nil
}

// ToSSEReader converts a progress channel to an io.ReadCloser for SSE streaming
func ToSSEReader(ch chan ProgressUpdate) io.ReadCloser {
	return &sseStream{ch: ch}
}

// sseStream implements io.ReadCloser for SSE streaming
type sseStream struct {
	ch     chan ProgressUpdate
	buffer []byte
}

func (s *sseStream) Read(p []byte) (n int, err error) {
	// If we have buffered data, return it first
	if len(s.buffer) > 0 {
		n = copy(p, s.buffer)
		s.buffer = s.buffer[n:]
		return n, nil
	}

	// Get next update from channel
	update, ok := <-s.ch
	if !ok {
		return 0, io.EOF
	}

	// Format as SSE
	data, _ := json.Marshal(update)
	msg := fmt.Sprintf("data: %s\n\n", data)
	s.buffer = []byte(msg)

	// Copy to output buffer
	n = copy(p, s.buffer)
	s.buffer = s.buffer[n:]
	return n, nil
}

func (s *sseStream) Close() error {
	return nil
}

