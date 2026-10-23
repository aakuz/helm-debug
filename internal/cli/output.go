package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
	"sigs.k8s.io/yaml"
)

const (
	red    = "\033[31m"
	green  = "\033[32m"
	yellow = "\033[33m"
	cyan   = "\033[36m"
	name   = "\033[1;35m" // bold magenta: resource names
	reset  = "\033[0m"
)

// print renders v as json/yaml, or calls table for the default human format.
func (o *Options) print(v any, table func(w io.Writer)) error {
	switch o.Output {
	case "json":
		enc := json.NewEncoder(o.Out)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	case "yaml":
		b, err := yaml.Marshal(v)
		if err != nil {
			return err
		}
		_, err = o.Out.Write(b)
		return err
	default:
		table(o.Out)
		return nil
	}
}

func (o *Options) colorEnabled() bool {
	if o.Color != nil {
		return *o.Color
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := o.Out.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func (o *Options) progressEnabled() bool {
	if o.Progress != nil {
		return *o.Progress
	}
	f, ok := o.ErrOut.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// progressFunc returns a callback that rewrites one status line on stderr, or nil when progress is off.
// The returned done function erases the line.
func (o *Options) progressFunc(what string) (progress func(done, total int, item string), done func()) {
	if !o.progressEnabled() {
		return nil, func() {}
	}
	shown := false
	return func(d, total int, item string) {
			shown = true
			fmt.Fprintf(o.ErrOut, "\r\033[KScanning %d/%d %s (%s)", d, total, what, item)
		}, func() {
			if shown {
				fmt.Fprint(o.ErrOut, "\r\033[K")
			}
		}
}

func (o *Options) paint(s, color string) string {
	if color == "" || !o.colorEnabled() {
		return s
	}
	return color + s + reset
}

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

// table aligns columns by their visible width, so colored cells line up.
type table struct {
	w    io.Writer
	rows [][]string
}

func newTable(w io.Writer) *table { return &table{w: w} }

func (t *table) add(cols ...string) {
	for i, c := range cols {
		if c == "" {
			cols[i] = "-"
		}
	}
	t.rows = append(t.rows, cols)
}

func (t *table) flush() {
	var widths []int
	for _, r := range t.rows {
		for i, c := range r {
			if i == len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], utf8.RuneCountInString(ansi.ReplaceAllString(c, "")))
		}
	}
	for _, r := range t.rows {
		var b strings.Builder
		for i, c := range r {
			b.WriteString(c)
			if i < len(r)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(ansi.ReplaceAllString(c, ""))+2))
			}
		}
		fmt.Fprintln(t.w, b.String())
	}
}

func statusColor(status string) string {
	switch status {
	case "deployed", "present", "ok", "healthy", "in-sync", "added", "unchanged":
		return green
	case "failed", "unhealthy", "absent", "error", "unknown-kind", "mismatch", "missing", "removed", "not-served":
		return red
	case "pending-install", "pending-upgrade", "pending-rollback", "uninstalling", "drifted", "changed", "deprecated":
		return yellow
	}
	return ""
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}

func formatAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// parallel runs fn for every index in [0,n) using at most limit goroutines.
func parallel(n, limit int, fn func(i int)) {
	if limit < 1 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}()
	}
	wg.Wait()
}
