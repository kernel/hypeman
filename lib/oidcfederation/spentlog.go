package oidcfederation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// maxSpentLogBytes is well above what the log can reach between compactions.
const maxSpentLogBytes = 64 << 20

// spentLog is the durable record of spent single-use assertions, so that a
// restart does not forget them. It is an append-only file with one line per
// assertion: the SHA-256 of the issuer URL and token ID in hex, the Unix time
// after which the record is no longer needed, and a checksum of the two,
// separated by spaces. Two lines of the same shape, "floor" and "dropped",
// say what the record does not hold.
//
// The file is trusted as far as it reads as one this code wrote. A copy of the
// data directory restored from an earlier time, or a file cut back to an
// earlier line boundary, reads the same as a complete record and is not
// detected.
type spentLog struct {
	path string
	file *os.File
	// floor is the issue time below which the record cannot say whether a
	// single-use assertion was spent, so such assertions are refused. It
	// rises whenever a start finds no complete record. It is never lowered:
	// a later start with an intact file still knows nothing about what came
	// before.
	floor time.Time
	// dropped is the latest expiry among the entries the record has let go
	// of as expired. An assertion expiring at or before it may be one of
	// them, so it is refused. It is never lowered either.
	dropped time.Time
	// lines counts the lines in the file, including expired ones.
	lines int
	// dirty means the file on disk may not hold exactly what was recorded: a
	// write left a partial line, or a rewritten file is not known to be
	// durable under its name. Nothing is appended until a rewrite succeeds.
	dirty bool
}

// spentKey identifies an assertion by the issuer URL it was verified against
// and its token ID. The URL is what the issuer signed; the name an operator
// gives the issuer can change. Uniqueness is per issuer, so one assertion
// cannot be spent once for each rule that would accept it.
func spentKey(issuerURL, jti string) string {
	sum := sha256.Sum256([]byte(issuerURL + "\x00" + jti))
	return hex.EncodeToString(sum[:])
}

// openSpentLog loads the record at path, creating it if needed, and returns
// the entries that have not expired. complete is false unless the file reads
// in full as one this code wrote: every line parses and ends in a newline, and
// the floor and dropped lines every such file carries are there. Otherwise the
// record cannot vouch for everything an earlier process spent, and the floor
// is raised to incompleteFloor.
func openSpentLog(path string, now, incompleteFloor time.Time) (log *spentLog, entries map[string]time.Time, complete bool, err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, false, err
	}
	// A directory created just now must outlive a crash, like the file in it.
	if err := syncDir(filepath.Dir(dir)); err != nil {
		return nil, nil, false, err
	}
	log = &spentLog{path: path}
	entries = make(map[string]time.Time)
	raw, err := readSpentLog(path)
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, errSpentLogTooLarge):
	case err != nil:
		return nil, nil, false, err
	default:
		lines := bytes.Split(raw, []byte("\n"))
		// After a final newline the last element is empty. Anything else is a
		// line without its end, which may be a record that was cut short
		// before its token was returned or one that was damaged afterwards.
		complete = len(lines[len(lines)-1]) == 0
		var sawFloor, sawDropped bool
		for _, line := range lines[:len(lines)-1] {
			key, until, ok := parseSpentLine(line)
			switch {
			case !ok:
				complete = false
			case key == floorKey:
				sawFloor = true
				if until.After(log.floor) {
					log.floor = until
				}
			case key == droppedKey:
				sawDropped = true
				if until.After(log.dropped) {
					log.dropped = until
				}
			case until.After(now):
				entries[key] = until
			default:
				log.forget(until)
			}
		}
		if !sawFloor || !sawDropped {
			complete = false
		}
	}
	if !complete && incompleteFloor.After(log.floor) {
		// Whole seconds, rounded up, as the file stores them.
		log.floor = incompleteFloor.Add(time.Second - time.Nanosecond).Truncate(time.Second)
	}
	if err := log.rewrite(entries); err != nil {
		return nil, nil, false, err
	}
	return log, entries, complete, nil
}

// forget notes the expiry of an entry that is being dropped as expired. With
// a correct clock that changes nothing: an assertion expiring that early is
// one the verifier refuses anyway. It matters when the host clock ran ahead
// and expired entries early: once the clock is corrected their assertions
// would otherwise be accepted again.
func (l *spentLog) forget(until time.Time) {
	// An entry is held until its assertion's expiry plus the clock leeway.
	if expiry := until.Add(-clockLeeway); expiry.After(l.dropped) {
		l.dropped = expiry
	}
}

// syncDir makes the names in a directory durable. Tests replace it.
var syncDir = func(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	err = dir.Sync()
	if closeErr := dir.Close(); err == nil {
		err = closeErr
	}
	return err
}

const (
	floorKey   = "floor"
	droppedKey = "dropped"
)

var errSpentLogTooLarge = errors.New("spent assertion log is too large")

func readSpentLog(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxSpentLogBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxSpentLogBytes {
		return nil, errSpentLogTooLarge
	}
	return raw, nil
}

func parseSpentLine(line []byte) (string, time.Time, bool) {
	fields := bytes.Split(line, []byte(" "))
	if len(fields) != 3 {
		return "", time.Time{}, false
	}
	key, until := string(fields[0]), string(fields[1])
	// The checksum catches damage that leaves a line readable, such as one
	// changed digit, which would otherwise drop or expire a record unnoticed.
	if string(fields[2]) != spentChecksum(key, until) {
		return "", time.Time{}, false
	}
	if key != floorKey && key != droppedKey {
		if len(key) != hex.EncodedLen(sha256.Size) {
			return "", time.Time{}, false
		}
		if _, err := hex.DecodeString(key); err != nil {
			return "", time.Time{}, false
		}
	}
	seconds, err := strconv.ParseInt(until, 10, 64)
	if err != nil {
		return "", time.Time{}, false
	}
	return key, time.Unix(seconds, 0), true
}

func spentChecksum(key, until string) string {
	sum := sha256.Sum256([]byte(key + " " + until))
	return hex.EncodeToString(sum[:8])
}

func spentLine(key string, until time.Time) string {
	seconds := strconv.FormatInt(until.Unix(), 10)
	return key + " " + seconds + " " + spentChecksum(key, seconds) + "\n"
}

// append records one spent assertion and returns once it is on disk.
func (l *spentLog) append(key string, until time.Time, entries map[string]time.Time) error {
	if l.dirty {
		// Start from what is known to be recorded, dropping any partial line.
		if err := l.rewrite(entries); err != nil {
			return err
		}
	}
	if _, err := l.file.WriteString(spentLine(key, until)); err != nil {
		l.dirty = true
		return err
	}
	if err := l.file.Sync(); err != nil {
		l.dirty = true
		return err
	}
	l.lines++
	return nil
}

// rewrite replaces the file with exactly the given entries, through a
// temporary file renamed over it.
func (l *spentLog) rewrite(entries map[string]time.Time) error {
	tmp := l.path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("write spent assertion log: %w", err)
	}
	var buf bytes.Buffer
	buf.WriteString(spentLine(floorKey, l.floor))
	buf.WriteString(spentLine(droppedKey, l.dropped))
	for key, until := range entries {
		buf.WriteString(spentLine(key, until))
	}
	_, err = file.Write(buf.Bytes())
	if err == nil {
		err = file.Sync()
	}
	if err == nil {
		err = os.Rename(tmp, l.path)
	}
	if err != nil {
		// The file at the path is untouched and still the one appended to.
		file.Close()
		os.Remove(tmp)
		return fmt.Errorf("write spent assertion log: %w", err)
	}
	// The path now names the new file, so appends go to it: the same
	// descriptor, with no reopening that could fail. Until the rename is
	// known to be durable a crash could bring the old file back, so nothing
	// may be appended, and no token issued, before a rewrite succeeds.
	if l.file != nil {
		l.file.Close()
	}
	l.file, l.lines, l.dirty = file, len(entries), true
	if err := syncDir(filepath.Dir(l.path)); err != nil {
		return fmt.Errorf("sync spent assertion log directory: %w", err)
	}
	l.dirty = false
	return nil
}
