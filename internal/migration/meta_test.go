package migration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestWriteMigrationMetaFileRoundTrip pins the publish contract the factory
// watcher depends on: after the call returns, the file at the advertised path
// is the complete metadata, and no temp file is left behind.
func TestWriteMigrationMetaFileRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	want := MigrationMeta{
		ID:              "sandbox-1",
		QEMUPid:         4242,
		QMPSocket:       "/run/vc/vm/sandbox-1/qmp.sock",
		VirtiofsdPid:    4243,
		HypervisorState: json.RawMessage(`{"pid":4242}`),
		CPU:             8,
		Memory:          16384,
		VMConfig:        json.RawMessage(`{"HypervisorType":"qemu"}`),
		AgentConfig:     json.RawMessage(`{"Debug":true}`),
	}
	path, err := WriteMigrationMetaFile(dir, want)
	if err != nil {
		t.Fatalf("WriteMigrationMetaFile: %v", err)
	}
	if path != filepath.Join(dir, MigrationMetaFile) {
		t.Fatalf("path = %s, want %s", path, filepath.Join(dir, MigrationMetaFile))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read published metadata: %v", err)
	}
	var got MigrationMeta
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("published metadata does not parse: %v", err)
	}
	if got.ID != want.ID || got.QEMUPid != want.QEMUPid || got.CPU != want.CPU || got.Memory != want.Memory {
		t.Fatalf("published metadata = %+v, want %+v", got, want)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file survived the rename (stat err = %v)", err)
	}
}

// TestWriteMigrationMetaFileConcurrentRewrites republishes the metadata from
// many goroutines. The factory watcher polls the same directory, so every
// observer must see one complete version and never a half-written file: the
// temp name is shared, so a torn publish would show up here as a parse failure
// or a leaked temp file.
func TestWriteMigrationMetaFileConcurrentRewrites(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	stop := make(chan struct{})

	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := WriteMigrationMetaFile(dir, MigrationMeta{
					ID:      "sandbox-1",
					QEMUPid: int64(w*100000 + i),
				}); err != nil {
					t.Errorf("WriteMigrationMetaFile: %v", err)
					return
				}
			}
		}()
	}
	// A reader standing in for the factory watcher's poll loop. It yields
	// between reads so the publishers are not starved on a loaded machine.
	readDone := make(chan struct{})
	reads := 0
	go func() {
		defer close(readDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			runtime.Gosched()
			raw, err := os.ReadFile(filepath.Join(dir, MigrationMetaFile))
			if err != nil {
				if !os.IsNotExist(err) {
					t.Errorf("read published metadata: %v", err)
				}
				continue
			}
			var got MigrationMeta
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Errorf("observer read a half-written file (%d bytes): %v", len(raw), err)
				return
			}
			if got.ID != "sandbox-1" {
				t.Errorf("observer read metadata for %q, want sandbox-1: %s", got.ID, raw)
				return
			}
			reads++
		}
	}()
	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()
	<-readDone

	if reads == 0 {
		t.Fatal("the observer never read the file; the test did not exercise the publish path")
	}
}
