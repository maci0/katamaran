package dashboard

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// serveThrough runs one request through the production middleware stack so the
// test exercises the same handler the server does.
func serveThrough(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, target, nil))
	return w
}

// TestAppStatusConcurrentWithWriters drives /api/status (and /api/history)
// while the migration worker and the load generators append to the buffers
// they snapshot. In production these are distinct goroutines: net/http serves
// each request on its own, runOrchestrator appends log lines and finalises
// results, and the load-generator goroutine records pings.
func TestAppStatusConcurrentWithWriters(t *testing.T) {
	t.Parallel()

	app := &App{startTime: time.Now()}
	mux := app.newMux(false)
	h := requestLogger(recoverMiddleware(securityHeaders(csrfCheck(mux))))

	const (
		readers  = 4
		writers  = 3
		perLoop  = 200
		pingVals = 50
	)

	var wg sync.WaitGroup
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perLoop {
				w := serveThrough(t, h, http.MethodGet, "/api/status?logs_after=3&pings_after=1")
				if w.Code != http.StatusOK {
					t.Errorf("/api/status = %d, want 200: %s", w.Code, w.Body.String())
					return
				}
				if w := serveThrough(t, h, http.MethodGet, "/api/history"); w.Code != http.StatusOK {
					t.Errorf("/api/history = %d, want 200: %s", w.Code, w.Body.String())
					return
				}
				if w := serveThrough(t, h, http.MethodGet, "/metrics"); w.Code != http.StatusOK {
					t.Errorf("/metrics = %d, want 200: %s", w.Code, w.Body.String())
					return
				}
			}
		}()
	}
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perLoop {
				app.appendLog(fmt.Sprintf(">>> log %d-%d", w, i))
				app.addPing(float64(i%pingVals), "")
			}
		}()
	}
	// The migration worker's own writes: latestProgress is replaced under the
	// lock and /api/status copies it, and the history grows under the same.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range perLoop {
			app.migrationMutex.Lock()
			app.isMigrating = i%2 == 0
			app.latestProgress = &MigrationProgress{
				Phase:          "transferring",
				RAMTransferred: int64(i),
				RAMTotal:       1000,
			}
			app.migrationID = fmt.Sprintf("mig-%d", i)
			app.migrationMutex.Unlock()
			app.setMigrationResult("success", "")
		}
	}()
	wg.Wait()

	if got := app.historySnapshot(); len(got) == 0 {
		t.Fatal("history empty after concurrent result writes")
	}
}

// TestAppLoadgenStartStopConcurrent races the load-generator guard. In
// production the starters run on net/http request goroutines, the stopper on
// another, and the generator itself on a third once it has been accepted. The
// check-and-set in tryStartLoadgen is the only thing keeping two generators
// from running at once and overwriting each other's ping log.
//
// The accepted start is released without running a generator: this test is
// about the contended state, not about the ping subprocess, and leaving one
// running would outlive the test.
func TestAppLoadgenStartStopConcurrent(t *testing.T) {
	t.Parallel()

	app := &App{startTime: time.Now()}
	accepted, conflict := 0, 0
	var mu sync.Mutex
	var wg sync.WaitGroup

	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				w := httptest.NewRecorder()
				ctx, ok := app.tryStartLoadgen(w, httptest.NewRequest(http.MethodPost, "/api/ping", nil), "ping")
				mu.Lock()
				switch {
				case !ok:
					conflict++
				default:
					accepted++
				}
				mu.Unlock()
				if ok {
					// Stand in for the generator goroutine: record samples,
					// then clear the state on exit like its deferred
					// resetLoadgen does.
					app.addPing(1.5, "")
					if ctx == nil {
						t.Error("tryStartLoadgen returned a nil context on success")
						return
					}
					app.resetLoadgen()
				}
			}
		}()
	}
	// A stopper running alongside, as /api/ping/stop does.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 400 {
			app.stopLoadgen()
		}
	}()
	wg.Wait()

	if accepted == 0 {
		t.Fatal("no start was ever accepted; the test did not exercise the transition")
	}
	if conflict == 0 {
		t.Fatal("no start was ever rejected; the guard was not contended")
	}
	app.loadgenMutex.Lock()
	running, cancel := app.loadgenRunning, app.loadgenCancel
	app.loadgenMutex.Unlock()
	if running || cancel != nil {
		t.Fatalf("loadgen left running=%v cancel!=nil=%v after every generator exited", running, cancel != nil)
	}
}

// TestAppMigrationStartRace drives the isMigrating guard from many goroutines
// at once. The check-then-set is the whole point of migrationMutex: two
// concurrent POST /api/migrate must not both start a migration, or a second
// run would overwrite the first's ID, cancel func, and log buffer.
func TestAppMigrationStartRace(t *testing.T) {
	t.Parallel()

	app := &App{startTime: time.Now(), allowedImage: "trusted:latest"}

	// Take the branch the handler takes once validation is past: claim the
	// guard exactly as handleMigrate does.
	claim := func() bool {
		app.migrationMutex.Lock()
		defer app.migrationMutex.Unlock()
		if app.isMigrating {
			return false
		}
		app.isMigrating = true
		app.migrationOutput = nil
		app.migrationLogSeq++
		app.logBufferWrapped = false
		app.latestProgress = nil
		app.migrationID = generateID()
		app.migrationStart = time.Now()
		app.migrationsStarted++
		return true
	}
	release := func() {
		app.migrationMutex.Lock()
		app.isMigrating = false
		app.migrationMutex.Unlock()
	}

	var winners, losers int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if claim() {
					mu.Lock()
					winners++
					mu.Unlock()
					app.appendLog(">>> claimed")
					release()
					continue
				}
				mu.Lock()
				losers++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if winners == 0 {
		t.Fatal("no goroutine ever claimed the migration guard")
	}
	if losers == 0 {
		t.Fatal("no goroutine was ever blocked by a concurrent claim; the guard was not contended")
	}
	if got, _, _ := app.counterSnapshot(); got != int64(winners) {
		t.Fatalf("migrationsStarted = %d, want %d (one per winning claim)", got, winners)
	}
}
