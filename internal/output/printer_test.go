package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// TestPrinter_EmitOneObjectOneLine: SPEC §"Output and Logging" (line
// 357) requires one JSON object per line, with a trailing newline.
func TestPrinter_EmitOneObjectOneLine(t *testing.T) {
	var buf bytes.Buffer
	p := &Printer{Stdout: &buf}
	p.Emit(map[string]any{"name": "work-personal", "enabled": true})

	got := buf.String()
	if strings.Count(got, "\n") != 1 {
		t.Errorf("expected one newline; got %d (output=%q)",
			strings.Count(got, "\n"), got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("expected trailing newline: %q", got)
	}

	var row map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(got)), &row); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if row["name"] != "work-personal" {
		t.Errorf("name = %v, want work-personal", row["name"])
	}
	if row["enabled"] != true {
		t.Errorf("enabled = %v, want true", row["enabled"])
	}
}

// TestPrinter_QuietSuppressesEmit: SPEC line 406 says --quiet/-q
// suppresses stdout. Pin it: Quiet=true means a no-op, even when Emit
// is invoked.
func TestPrinter_QuietSuppressesEmit(t *testing.T) {
	var buf bytes.Buffer
	p := &Printer{Stdout: &buf, Quiet: true}
	p.Emit(map[string]any{"x": 1})
	p.Emit(map[string]any{"x": 2})

	if buf.Len() != 0 {
		t.Errorf("Quiet=true should suppress output; got %q", buf.String())
	}
}

// TestPrinter_QuietSuppressesMeta: Quiet also suppresses the trailer.
// SPEC line 365 says _meta is always present "even on empty results",
// but --quiet is a separate (and stronger) opt-out.
func TestPrinter_QuietSuppressesMeta(t *testing.T) {
	var buf bytes.Buffer
	p := &Printer{Stdout: &buf, Quiet: true}
	p.Meta(map[string]any{"count": 0})

	if buf.Len() != 0 {
		t.Errorf("Quiet=true should suppress meta; got %q", buf.String())
	}
}

// TestPrinter_MetaShape: SPEC §"Output and Logging" (line 362) pins
// the trailer as `{"_meta":{...}}`. The body wraps as the value at the
// `_meta` key.
func TestPrinter_MetaShape(t *testing.T) {
	var buf bytes.Buffer
	p := &Printer{Stdout: &buf}
	p.Meta(map[string]any{"count": 2})

	got := strings.TrimSpace(buf.String())
	want := `{"_meta":{"count":2}}`
	if got != want {
		t.Errorf("meta = %q, want %q", got, want)
	}
}

// TestPrinter_MetaWithEmptyStruct: SPEC requires _meta even on empty
// results. Pin the wire shape for the empty body case so a future
// `omitempty` accident on metaWrapper would fail this test.
func TestPrinter_MetaWithEmptyStruct(t *testing.T) {
	var buf bytes.Buffer
	p := &Printer{Stdout: &buf}
	p.Meta(struct{}{})

	got := strings.TrimSpace(buf.String())
	want := `{"_meta":{}}`
	if got != want {
		t.Errorf("meta(empty struct) = %q, want %q", got, want)
	}
}

// TestPrinter_MetaWithCountAndHasMore: SPEC §"calendar-sync mirror list"
// (line 671) shows `{"_meta":{"count":N,"has_more":bool}}`. Pin that
// the wrapper renders multi-field bodies correctly.
func TestPrinter_MetaWithCountAndHasMore(t *testing.T) {
	var buf bytes.Buffer
	p := &Printer{Stdout: &buf}
	p.Meta(struct {
		Count   int  `json:"count"`
		HasMore bool `json:"has_more"`
	}{Count: 3, HasMore: true})

	var outer map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &outer); err != nil {
		t.Fatalf("decode: %v", err)
	}
	meta, ok := outer["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("_meta not an object: %v", outer)
	}
	if meta["count"].(float64) != 3 {
		t.Errorf("count = %v, want 3", meta["count"])
	}
	if meta["has_more"] != true {
		t.Errorf("has_more = %v, want true", meta["has_more"])
	}
}

// TestPrinter_NilStdoutIsSafe: the proposed surface tolerates a nil
// Stdout (e.g. when a caller hasn't wired it up yet). Pin it so we
// don't regress into a nil-deref.
func TestPrinter_NilStdoutIsSafe(t *testing.T) {
	p := &Printer{}
	p.Emit(map[string]any{"x": 1})
	p.Meta(map[string]any{"count": 0})
	// No assertion needed: reaching this line without panic is success.
}

// TestPrinter_NilReceiverIsSafe: a nil Printer is also a no-op. The
// daemon/dryrun pattern installs nil printers in some test paths; this
// keeps that idiom available here.
func TestPrinter_NilReceiverIsSafe(t *testing.T) {
	var p *Printer
	p.Emit(map[string]any{"x": 1})
	p.Meta(map[string]any{"count": 0})
}

// TestPrinter_EmitMultipleLines: a typical command emits N body lines
// then one trailer; pin the count and ordering.
func TestPrinter_EmitMultipleLines(t *testing.T) {
	var buf bytes.Buffer
	p := &Printer{Stdout: &buf}
	p.Emit(map[string]any{"id": 1})
	p.Emit(map[string]any{"id": 2})
	p.Meta(map[string]any{"count": 2})

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines; got %d (output=%q)", len(lines), buf.String())
	}
	// Trailer is always last.
	if !strings.Contains(lines[2], `"_meta"`) {
		t.Errorf("last line should be the meta trailer; got %q", lines[2])
	}
}

// TestPrinter_EmitEmptySlice: a body shaped as a slice should round-trip
// without becoming null. SPEC outputs (e.g. mirror list) emit each row
// individually rather than as an array, but tests also pass slice
// fields inside payloads, and we want to lock in slice handling.
func TestPrinter_EmitEmptySlice(t *testing.T) {
	var buf bytes.Buffer
	p := &Printer{Stdout: &buf}
	p.Emit(struct {
		Items []string `json:"items"`
	}{Items: []string{}})

	got := strings.TrimSpace(buf.String())
	if !strings.Contains(got, `"items":[]`) {
		t.Errorf("empty slice should render as []; got %q", got)
	}
}

// TestPrinter_ConcurrentEmitNoCorruption: writes from multiple
// goroutines must not interleave bytes. Each line should be a parseable
// JSON object on its own.
func TestPrinter_ConcurrentEmitNoCorruption(t *testing.T) {
	var buf bytes.Buffer
	p := &Printer{Stdout: &buf}
	const goroutines = 8
	const perGoroutine = 25

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				p.Emit(map[string]any{"id": id, "j": j})
			}
		}(i)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != goroutines*perGoroutine {
		t.Errorf("expected %d lines; got %d", goroutines*perGoroutine, len(lines))
	}
	for i, line := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Errorf("line %d not parseable JSON: %v (line=%q)", i, err, line)
		}
	}
}
