package migration

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

// A pipe read can end inside a multibyte character, so one logical line
// arrives in two Write calls. The writer must hold the partial line and log
// it once, as valid UTF-8, rather than one record per chunk.
func TestLogWriterReassemblesLineSplitAcrossWrites(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })

	line := "disk: 磁盘.img"
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	w := &logWriter{process: "qemu", stream: "stderr"}
	for cut := 1; cut < len(line); cut++ {
		logs.Reset()
		if n, err := w.Write([]byte(line[:cut])); n != cut || err != nil {
			t.Fatalf("first Write = %d, %v; want %d, nil", n, err, cut)
		}
		if logs.Len() != 0 {
			t.Fatalf("cut %d: logged %q before the line was complete", cut, logs.String())
		}
		rest := line[cut:] + "\n"
		if n, err := w.Write([]byte(rest)); n != len(rest) || err != nil {
			t.Fatalf("second Write = %d, %v; want %d, nil", n, err, len(rest))
		}
		var got struct {
			Output    string `json:"output"`
			Truncated bool   `json:"truncated"`
		}
		if err := json.Unmarshal(logs.Bytes(), &got); err != nil {
			t.Fatalf("cut %d: %v", cut, err)
		}
		if got.Output != line || got.Truncated {
			t.Fatalf("cut %d: output = %q, truncated = %v", cut, got.Output, got.Truncated)
		}
	}

	logs.Reset()
	w.flush()
	if logs.Len() != 0 {
		t.Fatalf("flush after a complete line logged %q", logs.String())
	}
	tail := "no newline"
	if n, err := w.Write([]byte(tail)); n != len(tail) || err != nil {
		t.Fatalf("partial Write = %d, %v", n, err)
	}
	w.flush()
	var tailGot struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(logs.Bytes(), &tailGot); err != nil {
		t.Fatal(err)
	}
	if tailGot.Output != tail {
		t.Fatalf("flushed tail = %q, want %q", tailGot.Output, tail)
	}
}
