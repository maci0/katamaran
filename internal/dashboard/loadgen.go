package dashboard

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var pingRe = regexp.MustCompile(`time=([0-9.]+) ms`)

func safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := lookupSafeTargetIPs(ctx, host)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{}
	var lastErr error
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// stopLoadgen cancels the currently running load generator context, if any.
func (a *App) stopLoadgen() {
	a.loadgenMutex.Lock()
	defer a.loadgenMutex.Unlock()
	if a.loadgenCancel != nil {
		a.loadgenCancel()
	}
}

// tryStartLoadgen atomically checks if a load generator is already running,
// and if not, marks it as started and returns a cancellable context.
// On conflict it writes the HTTP 409 response and returns false.
func (a *App) tryStartLoadgen(w http.ResponseWriter, r *http.Request, loadgenType string) (context.Context, bool) {
	a.loadgenMutex.Lock()
	if a.loadgenRunning {
		runningType := a.loadgenType
		a.loadgenMutex.Unlock()
		slog.Warn("Load generator request rejected: already running", "running_type", runningType, "requested_type", loadgenType, "request_id", requestIDFromContext(r.Context()))
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":        "Load generator already running",
			"loadgen_type": runningType,
		})
		return nil, false
	}
	a.loadgenRunning = true
	a.loadgenType = loadgenType
	a.pingLog = a.pingLog[:0]
	a.pingSeq++
	ctx, cancel := context.WithCancel(context.Background())
	a.loadgenCancel = cancel
	a.loadgenMutex.Unlock()
	return ctx, true
}

// resetLoadgen clears the load generator state after its goroutine exits.
func (a *App) resetLoadgen() {
	a.loadgenMutex.Lock()
	a.loadgenRunning = false
	a.loadgenType = ""
	a.loadgenCancel = nil
	a.loadgenMutex.Unlock()
}

// handleLoadgenStop processes a request to stop the currently running load generator.
func (a *App) handleLoadgenStop(w http.ResponseWriter, r *http.Request) {
	a.loadgenMutex.Lock()
	wasRunning := a.loadgenRunning
	loadgenType := a.loadgenType
	a.loadgenMutex.Unlock()
	a.stopLoadgen()
	if wasRunning {
		slog.Info("Load generator stop requested", "loadgen_type", loadgenType, "remote_addr", r.RemoteAddr, "request_id", requestIDFromContext(r.Context()))
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": "Load generator stop requested", "stopped": wasRunning, "loadgen_type": loadgenType})
}

// handlePingStart processes a request to start the ping load generator.
func (a *App) handlePingStart(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mediaType, _, err := mime.ParseMediaType(ct)
		if err != nil || mediaType != "application/x-www-form-urlencoded" {
			jsonError(w, "Content-Type must be application/x-www-form-urlencoded", http.StatusUnsupportedMediaType)
			return
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	if err := r.ParseForm(); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			jsonError(w, "Request body too large", http.StatusRequestEntityTooLarge)
		} else {
			jsonError(w, "Invalid request body", http.StatusBadRequest)
		}
		return
	}
	target := r.FormValue("target")
	if target == "" {
		jsonError(w, "Target required", http.StatusBadRequest)
		return
	}
	if !validTarget(target) {
		slog.Warn("Rejected invalid target", "target", target, "request_id", requestIDFromContext(r.Context()))
		jsonError(w, "Invalid target", http.StatusBadRequest)
		return
	}

	// Resolve once and pass ping an IP literal so it cannot resolve to a
	// different address after validation.
	pingTarget, ok := resolvedTargetIP(target)
	if !ok {
		slog.Warn("Failed to resolve safe ping target", "target", target, "request_id", requestIDFromContext(r.Context()))
		jsonError(w, "Invalid target", http.StatusBadRequest)
		return
	}

	ctx, ok := a.tryStartLoadgen(w, r, "ping")
	if !ok {
		return
	}

	reqID := requestIDFromContext(r.Context())
	slog.Info("Ping load generator started", "target", pingTarget, "request_id", reqID)

	go func() {
		defer a.resetLoadgen()

		cmd := exec.CommandContext(ctx, "ping", "-i", "0.2", pingTarget)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			slog.Error("Failed to create ping stdout pipe", "target", pingTarget, "error", err, "request_id", reqID)
			return
		}
		if err := cmd.Start(); err != nil {
			slog.Error("Failed to start ping process", "target", pingTarget, "error", err, "request_id", reqID)
			return
		}

		scanner := bufio.NewScanner(stdout)
		buf := make([]byte, 0, scannerInitBuf)
		scanner.Buffer(buf, scannerMaxSize)
		for scanner.Scan() {
			line := scanner.Text()
			matches := pingRe.FindStringSubmatch(line)
			if len(matches) > 1 {
				lat, parseErr := strconv.ParseFloat(matches[1], 64)
				if parseErr != nil {
					slog.Warn("Failed to parse ping latency", "raw", matches[1], "error", parseErr, "request_id", reqID)
					a.addPing(0, "Parse error")
				} else {
					a.addPing(lat, "")
				}
			} else if strings.Contains(line, "Unreachable") || strings.Contains(line, "timeout") {
				a.addPing(0, "Timeout/Unreachable")
			}
		}
		if scanErr := scanner.Err(); scanErr != nil {
			slog.Warn("Ping output scanner error", "target", pingTarget, "error", scanErr, "request_id", reqID)
		}
		if err := cmd.Wait(); err != nil && ctx.Err() == nil {
			slog.Warn("Ping command finished with error", "target", pingTarget, "error", err, "request_id", reqID)
		} else {
			slog.Info("Ping load generator stopped", "target", pingTarget, "request_id", reqID)
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{"message": "Ping started", "target": pingTarget})
}

// addPing records a ping latency sample or error to the loadgen log buffer.
func (a *App) addPing(lat float64, errStr string) {
	a.loadgenMutex.Lock()
	defer a.loadgenMutex.Unlock()
	a.pingSeq++
	a.pingLog = append(a.pingLog, PingData{
		Time:    time.Now().Format(time.RFC3339Nano),
		Latency: lat,
		Error:   errStr,
	})
	if len(a.pingLog) > maxPingLines {
		a.pingLog = slices.Delete(a.pingLog, 0, len(a.pingLog)-maxPingLines)
	}
}

// handleHTTPStart processes a request to start the HTTP load generator.
func (a *App) handleHTTPStart(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mediaType, _, err := mime.ParseMediaType(ct)
		if err != nil || mediaType != "application/x-www-form-urlencoded" {
			jsonError(w, "Content-Type must be application/x-www-form-urlencoded", http.StatusUnsupportedMediaType)
			return
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	if err := r.ParseForm(); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			jsonError(w, "Request body too large", http.StatusRequestEntityTooLarge)
		} else {
			jsonError(w, "Invalid request body", http.StatusBadRequest)
		}
		return
	}
	target := r.FormValue("target")
	if target == "" {
		jsonError(w, "Target required", http.StatusBadRequest)
		return
	}
	if !validTarget(target) {
		slog.Warn("Rejected invalid target", "target", target, "request_id", requestIDFromContext(r.Context()))
		jsonError(w, "Invalid target", http.StatusBadRequest)
		return
	}

	ctx, ok := a.tryStartLoadgen(w, r, "http")
	if !ok {
		return
	}

	reqID := requestIDFromContext(r.Context())
	slog.Info("HTTP load generator started", "target", target, "request_id", reqID)

	go func() {
		defer func() {
			slog.Info("HTTP load generator stopped", "target", target, "request_id", reqID)
			a.resetLoadgen()
		}()

		client := &http.Client{
			Timeout: httpClientTimeout,
			Transport: &http.Transport{
				Proxy:       nil,
				DialContext: safeDialContext,
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// Block redirects to prevent SSRF bypass: an attacker-controlled
				// target could redirect to internal/metadata IPs, bypassing the
				// validTarget check on the original URL.
				return http.ErrUseLastResponse
			},
		}
		ticker := time.NewTicker(httpLoadInterval)
		defer ticker.Stop()

		// Wrap bare IPv6 addresses in brackets for valid URL construction.
		// Without brackets, "http://2001:db8::1" is malformed (colons are
		// ambiguous with port separators in URLs).
		urlTarget := target
		host := target
		port := ""
		if h, p, err := net.SplitHostPort(target); err == nil {
			host = h
			port = p
		}
		if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
			if port != "" {
				urlTarget = net.JoinHostPort(host, port)
			} else {
				urlTarget = "[" + host + "]"
			}
		}
		targetURL := "http://" + urlTarget
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			start := time.Now()
			req, err := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
			if err != nil {
				a.addPing(0, "Request error: "+err.Error())
				return
			}
			resp, err := client.Do(req)
			lat := float64(time.Since(start).Milliseconds())

			if err != nil {
				if ctx.Err() != nil {
					return
				}
				a.addPing(0, err.Error())
			} else {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseDiscard))
				_ = resp.Body.Close()
				a.addPing(lat, "")
			}

			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{"message": "HTTP load generator started", "target": target})
}
