package uffdgraduate

import (
	"fmt"
	"time"
)

// Config controls how aggressively the controller graduates VMs off the pager.
// The zero value is disabled, so the feature is a no-op until explicitly turned
// on.
type Config struct {
	Enabled bool

	// MinSessionAge is how long a session must have been observed before it is
	// eligible to graduate. It gives the pager time to do its job as a restore
	// accelerator and avoids churning a freshly restored VM.
	MinSessionAge time.Duration

	// MaxConcurrent bounds simultaneous graduations. Each one reads the whole
	// remaining memory image, so this is the IO/RAM blast-radius lever.
	MaxConcurrent int

	// MaxActiveSessions is the hard ceiling on concurrent pager sessions. When
	// zero, graduation is purely time based: every session past MinSessionAge is
	// graduated. When positive, only enough oldest sessions are graduated to get
	// back to the ceiling (sessions on an outdated pager version are always
	// graduated regardless of the ceiling).
	MaxActiveSessions int

	// ScanInterval is how often the controller evaluates sessions.
	ScanInterval time.Duration

	// CompletionTimeout bounds a single graduation. Completion reads the whole
	// remaining image, so this is generous.
	CompletionTimeout time.Duration
}

const (
	defaultMinSessionAge     = 10 * time.Minute
	defaultMaxConcurrent     = 1
	defaultScanInterval      = time.Minute
	defaultCompletionTimeout = 10 * time.Minute
)

// Normalize fills in defaults for unset fields.
func (c Config) Normalize() Config {
	if c.MinSessionAge <= 0 {
		c.MinSessionAge = defaultMinSessionAge
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = defaultMaxConcurrent
	}
	if c.ScanInterval <= 0 {
		c.ScanInterval = defaultScanInterval
	}
	if c.CompletionTimeout <= 0 {
		c.CompletionTimeout = defaultCompletionTimeout
	}
	if c.MaxActiveSessions < 0 {
		c.MaxActiveSessions = 0
	}
	return c
}

// Validate rejects nonsensical enabled configs.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.MinSessionAge < 0 {
		return fmt.Errorf("uffd graduation min_session_age must not be negative")
	}
	if c.MaxConcurrent < 0 {
		return fmt.Errorf("uffd graduation max_concurrent must not be negative")
	}
	if c.MaxActiveSessions < 0 {
		return fmt.Errorf("uffd graduation max_active_sessions must not be negative")
	}
	if c.ScanInterval < 0 {
		return fmt.Errorf("uffd graduation scan_interval must not be negative")
	}
	return nil
}
