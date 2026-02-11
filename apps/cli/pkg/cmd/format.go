package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kernel/hypeman-go"
	"golang.org/x/term"
)

// TableWriter provides simple table formatting for CLI output with
// terminal-width-aware column sizing.
type TableWriter struct {
	w       io.Writer
	headers []string
	widths  []int    // natural widths (max of header and cell values)
	rows    [][]string

	// TruncOrder specifies column indices in truncation priority order.
	// The first index in the slice is truncated first when the table is
	// too wide for the terminal. Columns not listed are never truncated.
	TruncOrder []int
}

const columnGap = 2 // spaces between columns

// NewTableWriter creates a new table writer
func NewTableWriter(w io.Writer, headers ...string) *TableWriter {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	return &TableWriter{
		w:       w,
		headers: headers,
		widths:  widths,
	}
}

// AddRow adds a row to the table
func (t *TableWriter) AddRow(cells ...string) {
	// Pad or truncate to match header count
	row := make([]string, len(t.headers))
	for i := range row {
		if i < len(cells) {
			row[i] = cells[i]
		}
		if len(row[i]) > t.widths[i] {
			t.widths[i] = len(row[i])
		}
	}
	t.rows = append(t.rows, row)
}

// getTerminalWidth returns the terminal width. It tries the stdout
// file descriptor first, then the COLUMNS env var, then defaults to 80.
func getTerminalWidth() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		return w
	}
	if cols := os.Getenv("COLUMNS"); cols != "" {
		if w, err := strconv.Atoi(cols); err == nil && w > 0 {
			return w
		}
	}
	return 80
}

// renderWidths computes the final column widths, shrinking columns in
// TruncOrder as needed to fit within the terminal width.
func (t *TableWriter) renderWidths() []int {
	n := len(t.headers)
	widths := make([]int, n)
	copy(widths, t.widths)

	termWidth := getTerminalWidth()

	// Total space: column widths + gaps (no trailing gap on last column)
	total := func() int {
		s := 0
		for _, w := range widths {
			s += w
		}
		s += columnGap * (n - 1)
		return s
	}

	if total() <= termWidth {
		return widths
	}

	// Shrink columns in TruncOrder until the table fits
	for _, col := range t.TruncOrder {
		if col < 0 || col >= n {
			continue
		}
		excess := total() - termWidth
		if excess <= 0 {
			break
		}
		// Minimum width: at least the header length, but no less than 5
		minW := len(t.headers[col])
		if minW < 5 {
			minW = 5
		}
		canShrink := widths[col] - minW
		if canShrink <= 0 {
			continue
		}
		shrink := excess
		if shrink > canShrink {
			shrink = canShrink
		}
		widths[col] -= shrink
	}

	return widths
}

// Render outputs the table, dynamically fitting columns to the terminal width.
func (t *TableWriter) Render() {
	widths := t.renderWidths()
	last := len(t.headers) - 1

	// Print headers
	for i, h := range t.headers {
		cell := truncateCell(h, widths[i])
		if i < last {
			fmt.Fprintf(t.w, "%-*s", widths[i]+columnGap, cell)
		} else {
			fmt.Fprint(t.w, cell)
		}
	}
	fmt.Fprintln(t.w)

	// Print rows
	for _, row := range t.rows {
		for i, cell := range row {
			cell = truncateCell(cell, widths[i])
			if i < last {
				fmt.Fprintf(t.w, "%-*s", widths[i]+columnGap, cell)
			} else {
				fmt.Fprint(t.w, cell)
			}
		}
		fmt.Fprintln(t.w)
	}
}

// truncateCell truncates s to fit within maxWidth, appending "..." if needed.
func truncateCell(s string, maxWidth int) string {
	if len(s) <= maxWidth {
		return s
	}
	if maxWidth <= 3 {
		return s[:maxWidth]
	}
	return s[:maxWidth-3] + "..."
}

// FormatTimeAgo formats a time as "X ago" string
func FormatTimeAgo(t time.Time) string {
	if t.IsZero() {
		return "N/A"
	}

	d := time.Since(t)

	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d seconds ago", int(d.Seconds()))
	case d < time.Hour:
		mins := int(d.Minutes())
		if mins == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", mins)
	case d < 24*time.Hour:
		hours := int(d.Hours())
		if hours == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	}
}

// TruncateID truncates an ID to 12 characters (like Docker)
func TruncateID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// TruncateString truncates a string to max length with ellipsis
func TruncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

// GenerateInstanceName generates a name from image reference
func GenerateInstanceName(image string) string {
	// Extract image name without registry/tag
	name := image

	// Remove registry prefix
	if idx := strings.LastIndex(name, "/"); idx != -1 {
		name = name[idx+1:]
	}

	// Remove tag/digest
	if idx := strings.Index(name, ":"); idx != -1 {
		name = name[:idx]
	}
	if idx := strings.Index(name, "@"); idx != -1 {
		name = name[:idx]
	}

	// Add random suffix
	suffix := randomSuffix(4)
	return fmt.Sprintf("%s-%s", name, suffix)
}

// randomSuffix generates a random alphanumeric suffix
func randomSuffix(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		// Simple pseudo-random using time
		b[i] = chars[(time.Now().UnixNano()+int64(i))%int64(len(chars))]
	}
	return string(b)
}

// ResolveInstance resolves an instance identifier to a full instance ID.
// It supports:
// - Full instance ID (exact match)
// - Partial instance ID (prefix match)
// - Instance name (exact match)
// Returns an error if the identifier is ambiguous or not found.
func ResolveInstance(ctx context.Context, client *hypeman.Client, identifier string) (string, error) {
	// List all instances
	instances, err := client.Instances.List(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to list instances: %w", err)
	}

	var matches []hypeman.Instance

	for _, inst := range *instances {
		// Exact ID match - return immediately
		if inst.ID == identifier {
			return inst.ID, nil
		}
		// Exact name match - return immediately
		if inst.Name == identifier {
			return inst.ID, nil
		}
		// Partial ID match (prefix)
		if strings.HasPrefix(inst.ID, identifier) {
			matches = append(matches, inst)
		}
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no instance found matching %q", identifier)
	case 1:
		return matches[0].ID, nil
	default:
		// Ambiguous - show matching IDs
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = TruncateID(m.ID)
		}
		return "", fmt.Errorf("ambiguous instance identifier %q matches: %s", identifier, strings.Join(ids, ", "))
	}
}

