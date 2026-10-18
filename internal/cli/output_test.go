package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func boolPtr(b bool) *bool { return &b }

func TestFormatAge(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{59 * time.Second, "59s"},
		{59*time.Second + 999*time.Millisecond, "59s"},
		{time.Minute, "1m"},
		{59*time.Minute + 59*time.Second, "59m"},
		{time.Hour, "1h"},
		{47*time.Hour + 59*time.Minute, "47h"},
		{48 * time.Hour, "2d"},
		{71 * time.Hour, "2d"},
		{72 * time.Hour, "3d"},
		{400 * 24 * time.Hour, "400d"},
	} {
		if got := formatAge(tc.d); got != tc.want {
			t.Errorf("formatAge(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestFormatTime(t *testing.T) {
	if got := formatTime(time.Time{}); got != "" {
		t.Fatalf("zero time = %q, want empty", got)
	}
	tz := time.FixedZone("X", 3*3600)
	in := time.Date(2026, 10, 18, 1, 4, 5, 0, tz)
	if got, want := formatTime(in), "2026-10-17 22:04:05"; got != want {
		t.Fatalf("formatTime = %q, want %q (UTC)", got, want)
	}
	if got := formatTime(now); got != "2026-10-18 12:00:00" {
		t.Fatalf("formatTime(now) = %q", got)
	}
}

func TestStatusColor(t *testing.T) {
	for status, want := range map[string]string{
		"deployed": green, "present": green, "ok": green, "in-sync": green, "added": green, "unchanged": green,
		"failed": red, "absent": red, "error": red, "unknown-kind": red, "mismatch": red, "missing": red, "removed": red,
		"pending-install": yellow, "pending-upgrade": yellow, "pending-rollback": yellow,
		"uninstalling": yellow, "drifted": yellow, "changed": yellow,
		"superseded": "", "": "", "DEPLOYED": "",
	} {
		if got := statusColor(status); got != want {
			t.Errorf("statusColor(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestColorEnabled(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	for _, tc := range []struct {
		name    string
		color   *bool
		noColor string
		out     any
		want    bool
	}{
		{"forced on", boolPtr(true), "", &bytes.Buffer{}, true},
		{"forced on beats NO_COLOR", boolPtr(true), "1", &bytes.Buffer{}, true},
		{"forced off", boolPtr(false), "", f, false},
		{"NO_COLOR", nil, "1", f, false},
		{"non-file writer", nil, "", &bytes.Buffer{}, false},
		{"regular file is not a terminal", nil, "", f, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NO_COLOR", tc.noColor)
			o := &Options{Color: tc.color}
			o.Out = tc.out.(interface{ Write([]byte) (int, error) })
			if got := o.colorEnabled(); got != tc.want {
				t.Fatalf("colorEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPaint(t *testing.T) {
	on := &Options{Color: boolPtr(true)}
	off := &Options{Color: boolPtr(false)}
	if got, want := on.paint("x", red), red+"x"+reset; got != want {
		t.Fatalf("paint on = %q, want %q", got, want)
	}
	if got := on.paint("x", ""); got != "x" {
		t.Fatalf("empty color must not wrap, got %q", got)
	}
	if got := off.paint("x", red); got != "x" {
		t.Fatalf("paint off = %q", got)
	}
	if got := on.paint("", green); got != green+reset {
		t.Fatalf("empty string still wrapped: %q", got)
	}
}

func flushed(rows ...[]string) string {
	var b bytes.Buffer
	tb := newTable(&b)
	for _, r := range rows {
		tb.add(r...)
	}
	tb.flush()
	return b.String()
}

func TestTableAlignment(t *testing.T) {
	got := flushed(
		[]string{"NAME", "STATUS", "AGE"},
		[]string{"a", "deployed", "1d"},
		[]string{"longer-name", "x", "2h"},
	)
	want := "NAME         STATUS    AGE\n" +
		"a            deployed  1d\n" +
		"longer-name  x         2h\n"
	if got != want {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
}

func TestTableEmptyCellsBecomeDash(t *testing.T) {
	got := flushed([]string{"A", "B", "C"}, []string{"", "x", ""})
	want := "A  B  C\n-  x  -\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTableAnsiCellsAlignByVisibleWidth(t *testing.T) {
	o := &Options{Color: boolPtr(true)}
	got := flushed(
		[]string{"STATUS", "NEXT"},
		[]string{o.paint("ok", green), "n1"},
		[]string{"unchanged", "n2"},
	)
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %q", got)
	}
	if want := green + "ok" + reset + "         n1"; lines[1] != want {
		t.Fatalf("colored cell not padded by visible width: %q, want %q", lines[1], want)
	}
	plain := ansi.ReplaceAllString(got, "")
	if want := "STATUS     NEXT\nok         n1\nunchanged  n2\n"; plain != want {
		t.Fatalf("plain text = %q, want %q", plain, want)
	}
}

func TestTableMultibyteAndRaggedRows(t *testing.T) {
	got := flushed([]string{"é", "x"}, []string{"abc"}, []string{"d", "e", "f"})
	want := "é    x\nabc\nd    e  f\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTableEmptyFlush(t *testing.T) {
	if got := flushed(); got != "" {
		t.Fatalf("empty table printed %q", got)
	}
}

func TestTableLastColumnNotPadded(t *testing.T) {
	for _, l := range strings.Split(strings.TrimSuffix(flushed([]string{"a", "b"}, []string{"cc", "dddd"}), "\n"), "\n") {
		if strings.HasSuffix(l, " ") {
			t.Fatalf("trailing space in %q", l)
		}
	}
}

type sample struct {
	Name  string `json:"name"`
	Count int    `json:"count,omitempty"`
}

func TestPrint(t *testing.T) {
	v := sample{Name: "api", Count: 2}
	for _, tc := range []struct {
		output string
		want   string
		table  bool
	}{
		{"json", "{\n  \"name\": \"api\",\n  \"count\": 2\n}\n", false},
		{"yaml", "count: 2\nname: api\n", false},
		{"table", "TABLE\n", true},
		{"", "TABLE\n", true},
	} {
		t.Run("output="+tc.output, func(t *testing.T) {
			var b bytes.Buffer
			called := false
			o := &Options{Output: tc.output, Out: &b}
			err := o.print(v, func(w io.Writer) {
				called = true
				fmt.Fprintln(w, "TABLE")
			})
			if err != nil {
				t.Fatal(err)
			}
			if called != tc.table {
				t.Fatalf("table callback called = %v, want %v", called, tc.table)
			}
			if b.String() != tc.want {
				t.Fatalf("got %q, want %q", b.String(), tc.want)
			}
		})
	}
}

func TestPrintMarshalErrors(t *testing.T) {
	bad := map[string]any{"f": func() {}}
	for _, format := range []string{"json", "yaml"} {
		var b bytes.Buffer
		o := &Options{Output: format, Out: &b}
		if err := o.print(bad, func(io.Writer) { t.Fatal("table must not run") }); err == nil {
			t.Errorf("%s: want marshal error", format)
		}
		if b.Len() != 0 {
			t.Errorf("%s: partial output %q", format, b.String())
		}
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestPrintWriteErrors(t *testing.T) {
	for _, format := range []string{"json", "yaml"} {
		o := &Options{Output: format, Out: failWriter{}}
		if err := o.print(sample{Name: "x"}, nil); err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Errorf("%s: want write error, got %v", format, err)
		}
	}
}

func TestParallelRunsEveryIndexOnce(t *testing.T) {
	for _, limit := range []int{-5, 0, 1, 3, 100} {
		const n = 25
		var mu sync.Mutex
		seen := map[int]int{}
		parallel(n, limit, func(i int) {
			mu.Lock()
			seen[i]++
			mu.Unlock()
		})
		if len(seen) != n {
			t.Fatalf("limit %d: ran %d indexes, want %d", limit, len(seen), n)
		}
		for i, c := range seen {
			if c != 1 {
				t.Fatalf("limit %d: index %d ran %d times", limit, i, c)
			}
		}
	}
}

func TestParallelNothingToDo(t *testing.T) {
	parallel(0, 4, func(int) { t.Fatal("must not run") })
}

func TestParallelRespectsLimit(t *testing.T) {
	for _, tc := range []struct{ limit, want int }{{1, 1}, {0, 1}, {-1, 1}, {3, 3}} {
		var cur, peak atomic.Int32
		reached := make(chan struct{})
		var once sync.Once
		parallel(20, tc.limit, func(int) {
			n := cur.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			if int(n) == tc.want {
				once.Do(func() { close(reached) })
			}
			// Hold every worker until the limit is reached, forcing real overlap without sleeping.
			<-reached
			cur.Add(-1)
		})
		if got := int(peak.Load()); got != tc.want {
			t.Errorf("limit %d: peak concurrency %d, want exactly %d", tc.limit, got, tc.want)
		}
	}
}

func TestParallelLimitBelowOneIsSequential(t *testing.T) {
	var order []int
	var mu sync.Mutex
	parallel(5, 0, func(i int) {
		mu.Lock()
		order = append(order, i)
		mu.Unlock()
	})
	if len(order) != 5 {
		t.Fatalf("ran %d times, want 5: %v", len(order), order)
	}
	for i, v := range order {
		if v != i {
			t.Fatalf("limit<1 must run sequentially in order, got %v", order)
		}
	}
}
