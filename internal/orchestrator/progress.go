package orchestrator

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/maci0/katamaran/internal/logging"
	"github.com/maci0/katamaran/internal/migration"
)

// tailProgress watches the source pod's logs for KATAMARAN_PROGRESS and
// KATAMARAN_RESULT markers emitted by the source binary. PROGRESS markers
// are re-emitted as PhaseTransferring StatusUpdates with RAMTransferred /
// RAMTotal populated. The RESULT marker (one-shot, post-completion) is
// stashed on run for the reconciler to attach to PhaseSucceeded.
//
// Exit condition: a RESULT marker, a failed/cancelled progress status,
// or ctx cancel. Plain `status=completed` is NOT terminal here (the
// RESULT line lands a few ms after), so we keep polling until RESULT
// arrives or the run is torn down.
func (n *native) tailProgress(ctx context.Context, id MigrationID, run *nativeRun) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("tailProgress panic", "migration_id", id, "panic", rec, "stack", string(debug.Stack()))
		}
	}()
	srcPod, err := n.firstSourcePod(ctx, run.srcJob, run.podWaitTimeoutSeconds)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("Progress tail unavailable: source pod was not found", "migration_id", id, "source_job", run.srcJob, "namespace", n.namespace, "error", err)
		}
		return // source pod never appeared; poll will surface the failure
	}
	// The KATAMARAN_* markers below are the wire protocol printed by the
	// source binary; reference the shared constants in internal/migration
	// so scraper and emitter cannot drift apart.
	const (
		progressMarker      = migration.ProgressMarker
		resultMarker        = migration.ResultMarker
		downtimeLimitMarker = migration.DowntimeLimitMarker
		phaseMarker         = migration.PhaseMarker
		// The log is consumed as ONE followed stream instead of re-fetching
		// a rolling SinceSeconds window every tick: polling a 30s window at
		// a 2s cadence re-transfers every log byte ~15x through the
		// apiserver-to-kubelet proxy and sets up a fresh HTTP stream per
		// tick. With Follow=true each marker is delivered exactly once as
		// the source emits it.
		//
		// SinceSeconds bounds the initial backfill on (re)connect;
		// LimitBytes bounds the total stream size. Both also cap what a
		// reconnect re-downloads after an error, so worst-case behavior
		// degrades to today's polling cost rather than exceeding it.
		logFetchOverlapSec int64 = 30
		logFetchLimitBytes int64 = 4 * 1024 * 1024
		// logStreamTTL bounds how long one followed stream is trusted. A
		// kubelet/apiserver hiccup can leave a stream silently stalled;
		// closing and redialing periodically guarantees tailProgress keeps
		// up with the source and cannot block forever on a dead stream.
		logStreamTTL = time.Minute
	)
	seen := map[string]bool{} // dedupe identical marker lines across reconnects
	ticker := time.NewTicker(jobPollInterval)
	defer ticker.Stop()
	// Reused scanner buffer: avoids allocating 64KB per (re)connect over multi-hour migrations.
	scanBuf := make([]byte, 0, logScannerInitBuf)
	send := func(u StatusUpdate) bool {
		select {
		case <-ctx.Done():
			return false
		case <-run.finished:
			return false
		default:
		}
		run.send(u)
		return true
	}
	overlap := logFetchOverlapSec
	limitBytes := logFetchLimitBytes
	// Hoisted outside the loop: same value every connect, no need to re-allocate.
	logOpts := &corev1.PodLogOptions{Container: "katamaran", Follow: true, SinceSeconds: &overlap, LimitBytes: &limitBytes}
	var consecStreamErrors int
	for {
		select {
		case <-ctx.Done():
			return
		case <-run.finished:
			return
		case <-ticker.C:
		}
		// Cap the dedup map: only markers from within the reconnect
		// backfill window can recur, so anything beyond it is dead weight.
		// Resetting periodically keeps memory bounded on multi-hour migrations.
		if len(seen) > 1024 {
			clear(seen)
		}
		req := n.client.CoreV1().Pods(n.namespace).GetLogs(srcPod, logOpts)
		streamCtx, streamCancel := context.WithTimeout(ctx, logStreamTTL)
		stream, err := req.Stream(streamCtx)
		if err != nil {
			streamCancel()
			if ctx.Err() == nil {
				consecStreamErrors++
				attrs := []any{"migration_id", id, "pod", srcPod, "error", err, "consecutive_errors", consecStreamErrors}
				// Persistent failures (apiserver flapping, RBAC drop) leave the
				// caller without progress markers indefinitely; escalate so the
				// blind window is visible without raising the global log level.
				slog.Log(ctx, logging.TransientLevel(consecStreamErrors),
					"tailProgress: opening source pod log stream failing repeatedly", attrs...)
			}
			continue
		}
		consecStreamErrors = 0
		// Stream line-by-line instead of materializing the log window as a
		// single string + slice; the backfill can be hundreds of KB on
		// chatty migrations.
		scanner := bufio.NewScanner(stream)
		scanner.Buffer(scanBuf, logScannerMaxBuf)
		done := false
		for scanner.Scan() {
			line := scanner.Text()
			// Fast path: chatty pods produce many non-marker lines per tick
			// (a 4MB / 30s window can be tens of thousands of lines). Skip
			// them with a single substring scan before the dedup map lookup
			// and three per-marker Index calls below.
			if !strings.Contains(line, "KATAMARAN_") {
				continue
			}
			if seen[line] {
				continue
			}
			if i := strings.Index(line, resultMarker); i >= 0 {
				fields := parseProgressFields(line[i+len(resultMarker):])
				run.resultMu.Lock()
				run.resultDowntime, run.resultRAMTransferred, run.resultRAMTotal = parseResultFields(fields)
				run.resultCaptured = true
				run.resultMu.Unlock()
				done = true
				break
			}
			if i := strings.Index(line, downtimeLimitMarker); i >= 0 {
				seen[line] = true
				fields := parseProgressFields(line[i+len(downtimeLimitMarker):])
				applied := parseInt64(fields["applied_ms"])
				rttMS := parseInt64(fields["rtt_ms"])
				autoFlag := fields["auto"] == "true"
				run.resultMu.Lock()
				run.appliedDowntime = applied
				run.rttMS = rttMS
				run.autoDowntime = autoFlag
				run.downtimeCaptured = true
				run.resultMu.Unlock()
				msg := fmt.Sprintf("downtime limit applied: %dms", applied)
				if autoFlag {
					msg += fmt.Sprintf(" (auto from %dms RTT)", rttMS)
				}
				if !send(StatusUpdate{
					ID:                id,
					Phase:             PhaseTransferring,
					When:              time.Now(),
					Message:           msg,
					AppliedDowntimeMS: applied,
					RTTMS:             rttMS,
					AutoDowntime:      autoFlag,
				}) {
					done = true
					break
				}
				continue
			}
			if i := strings.Index(line, phaseMarker); i >= 0 {
				fields := parseProgressFields(line[i+len(phaseMarker):])
				ph, ok := phaseFromMarker(fields["phase"])
				if !ok {
					continue
				}
				seen[line] = true
				if !send(StatusUpdate{
					ID:      id,
					Phase:   ph,
					When:    time.Now(),
					Message: "source reported " + string(ph),
				}) {
					done = true
					break
				}
				continue
			}
			i := strings.Index(line, progressMarker)
			if i < 0 {
				continue
			}
			seen[line] = true
			fields := parseProgressFields(line[i+len(progressMarker):])
			if !send(StatusUpdate{
				ID:             id,
				Phase:          PhaseTransferring,
				When:           time.Now(),
				Message:        "status=" + fields["status"],
				RAMTransferred: parseInt64(fields["ram_transferred"]),
				RAMTotal:       parseInt64(fields["ram_total"]),
			}) {
				done = true
				break
			}
			if fields["status"] == "failed" || fields["status"] == "cancelled" {
				done = true
				break
			}
		}
		if scanErr := scanner.Err(); scanErr != nil && ctx.Err() == nil && streamCtx.Err() == nil {
			slog.Debug("tailProgress: reading source pod log stream failed", "migration_id", id, "pod", srcPod, "error", scanErr)
		}
		streamCancel()
		_ = stream.Close()
		if done {
			return
		}
	}
}

// parseProgressFields parses key=value pairs separated by spaces.
func parseProgressFields(s string) map[string]string {
	out := make(map[string]string, 8)
	for _, kv := range strings.Fields(s) {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		out[kv[:eq]] = kv[eq+1:]
	}
	return out
}

// phaseFromMarker validates the phase= value of a KATAMARAN_PHASE marker
// emitted by the source/dest binaries. Only non-terminal lifecycle phases
// may be fabricated from log markers: terminal outcomes are owned by the
// Job-condition poll (its outcome matrix), never by a scraped line.
func phaseFromMarker(s string) (StatusPhase, bool) {
	switch StatusPhase(s) {
	case PhaseTransferring, PhaseCutover:
		return StatusPhase(s), true
	}
	return "", false
}

// succeededUpdate builds the final PhaseSucceeded StatusUpdate, attaching
// captured downtime / RAM totals from tailProgress when available.
//
// poll fires PhaseSucceeded as soon as it sees the dest Job reach
// Complete; that can race with tailProgress's 2s ticker, leaving the
// KATAMARAN_RESULT marker unscraped even though it's already in the
// source pod's log. To close that gap we do one synchronous final scrape
// here when the result hasn't been captured yet.
func (n *native) succeededUpdate(ctx context.Context, id MigrationID, run *nativeRun) StatusUpdate {
	u := StatusUpdate{ID: id, Phase: PhaseSucceeded, When: time.Now()}
	run.resultMu.Lock()
	captured := run.resultCaptured
	if captured {
		u.DowntimeMS = run.resultDowntime
		u.RAMTransferred = run.resultRAMTransferred
		u.RAMTotal = run.resultRAMTotal
	}
	if run.downtimeCaptured {
		u.AppliedDowntimeMS = run.appliedDowntime
		u.RTTMS = run.rttMS
		u.AutoDowntime = run.autoDowntime
	}
	run.resultMu.Unlock()
	if captured {
		return u
	}
	// Final synchronous scrape, bounded so a wedged apiserver never holds
	// up the terminal status update.
	scrapeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if down, transferred, total, ok := n.scrapeResultMarker(scrapeCtx, run.srcJob); ok {
		run.resultMu.Lock()
		run.resultCaptured = true
		run.resultDowntime = down
		run.resultRAMTransferred = transferred
		run.resultRAMTotal = total
		run.resultMu.Unlock()
		u.DowntimeMS = down
		u.RAMTransferred = transferred
		u.RAMTotal = total
	}
	return u
}

// scrapeResultMarker does a one-shot bounded fetch of the source pod's recent
// log tail and returns the latest KATAMARAN_RESULT marker's downtime /
// transferred / total fields. Returns ok=false if no source pod exists, the
// log stream fails, or no marker is present in the captured log window.
func (n *native) scrapeResultMarker(ctx context.Context, srcJob string) (downtimeMS, ramTransferred, ramTotal int64, ok bool) {
	pod, err := n.firstSourcePod(ctx, srcJob, 0)
	if err != nil {
		return 0, 0, 0, false
	}
	tailLines := int64(200)
	limitBytes := int64(1024 * 1024)
	stream, err := n.client.CoreV1().Pods(n.namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container:  "katamaran",
		TailLines:  &tailLines,
		LimitBytes: &limitBytes,
	}).Stream(ctx)
	if err != nil {
		return 0, 0, 0, false
	}
	defer func() { _ = stream.Close() }()
	const marker = migration.ResultMarker
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, logScannerInitBuf), logScannerMaxBuf)
	for scanner.Scan() {
		line := scanner.Text()
		i := strings.Index(line, marker)
		if i < 0 {
			continue
		}
		downtimeMS, ramTransferred, ramTotal = parseResultFields(parseProgressFields(line[i+len(marker):]))
		ok = true
		// Don't break: take the LAST marker, which is what tailProgress
		// would have picked up too.
	}
	return downtimeMS, ramTransferred, ramTotal, ok
}

// parseInt64 decodes an integer field from a scraped KATAMARAN_* marker.
// Missing fields already yield 0 via the map lookup; parse failures and
// out-of-range values must yield 0 too. strconv.ParseInt clamps out-of-range
// input to MaxInt64/MinInt64 alongside its error; letting that clamp through
// would persist bogus byte/downtime counts into Migration CR status and
// overflow downstream math like (ramTransferred * 100).
func parseInt64(s string) int64 {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// parseResultFields decodes the three KATAMARAN_RESULT marker counters.
// Shared by tailProgress (live capture) and scrapeResultMarker (final
// synchronous scrape) so the field names cannot drift between the two paths.
func parseResultFields(fields map[string]string) (downtimeMS, ramTransferred, ramTotal int64) {
	return parseInt64(fields["downtime_ms"]),
		parseInt64(fields["ram_transferred"]),
		parseInt64(fields["ram_total"])
}
