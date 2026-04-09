package main

import (
	"bufio"
	"context"
	"io"
	"log/slog"
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
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":        "Load generator already running",
			"loadgen_type": runningType,
		})
		return nil, false
	}
	a.loadgenRunning = true
	a.loadgenType = loadgenType
	a.pingLog = a.pingLog[:0]
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

func (a *App) handlePingStart(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	if target == "" {
		jsonError(w, "Target required", http.StatusBadRequest)
		return
	}
	if !validTarget(target) {
		slog.Warn("Rejected invalid ping target", "target", target, "request_id", requestIDFromContext(r.Context()))
		jsonError(w, "Invalid target", http.StatusBadRequest)
		return
	}

	ctx, ok := a.tryStartLoadgen(w, r, "ping")
	if !ok {
		return
	}

	// Strip port if present — ping only accepts host/IP, not host:port.
	pingTarget := target
	if h, _, err := net.SplitHostPort(target); err == nil {
		pingTarget = h
	}

	reqID := requestIDFromContext(r.Context())
	slog.Info("Ping load generator started", "target", pingTarget, "request_id", reqID)

	go func() {
		defer a.resetLoadgen()

		cmd := exec.CommandContext(ctx, "ping", "-i", "0.2", pingTarget)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			slog.Error("Failed to create ping stdout pipe", "target", pingTarget, "error", err)
			return
		}
		if err := cmd.Start(); err != nil {
			slog.Error("Failed to start ping process", "target", pingTarget, "error", err)
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
					slog.Debug("Failed to parse ping latency", "raw", matches[1], "error", parseErr)
				}
				a.addPing(lat, "")
			} else if strings.Contains(line, "Unreachable") || strings.Contains(line, "timeout") {
				a.addPing(0, "Timeout/Unreachable")
			}
		}
		if scanErr := scanner.Err(); scanErr != nil {
			slog.Warn("Ping output scanner error", "target", target, "error", scanErr)
		}
		if err := cmd.Wait(); err != nil && ctx.Err() == nil {
			slog.Warn("Ping command finished with error", "target", target, "error", err)
		} else {
			slog.Info("Ping load generator stopped", "target", pingTarget, "request_id", reqID)
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{"message": "Ping started", "target": pingTarget})
}

func (a *App) addPing(lat float64, errStr string) {
	a.loadgenMutex.Lock()
	defer a.loadgenMutex.Unlock()
	a.pingLog = append(a.pingLog, PingData{
		Time:    time.Now().Format(time.RFC3339Nano),
		Latency: lat,
		Error:   errStr,
	})
	if len(a.pingLog) > maxPingLines {
		a.pingLog = slices.Delete(a.pingLog, 0, len(a.pingLog)-maxPingLines)
	}
}

func (a *App) handleHTTPStart(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	if target == "" {
		jsonError(w, "Target required", http.StatusBadRequest)
		return
	}
	if !validTarget(target) {
		slog.Warn("Rejected invalid HTTP target", "target", target, "request_id", requestIDFromContext(r.Context()))
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
				resp.Body.Close()
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
