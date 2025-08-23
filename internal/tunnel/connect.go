/*
Copyright 2025 The KubeLB Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tunnel

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/http2"

	"k8c.io/kubelb-cli/internal/config"
	"k8c.io/kubelb-cli/internal/logger"
	"k8c.io/kubelb-cli/internal/output"
	"k8c.io/kubelb-cli/internal/ui"
	kubelbce "k8c.io/kubelb/api/ee/kubelb.k8c.io/v1alpha1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func Connect(ctx context.Context, k8s client.Client, cfg *config.Config, tunnelName string, port int) error {
	if cfg.IsCE() {
		return ErrTunnelNotAvailable
	}

	log := logger.WithTunnel(tunnelName).WithOperation("connect")

	log.Debug("starting tunnel connection",
		"tunnel", tunnelName,
		"port", port,
		"tenant", cfg.TenantNamespace,
	)

	if port <= 0 || port > 65535 {
		log.Error("invalid port specified", "port", port)
		return fmt.Errorf("invalid port: %d (must be between 1 and 65535)", port)
	}

	log.Debug("fetching tunnel resource from kubernetes")
	tunnel := &kubelbce.Tunnel{}
	if err := k8s.Get(ctx, client.ObjectKey{
		Namespace: cfg.TenantNamespace,
		Name:      tunnelName,
	}, tunnel); err != nil {
		if apierrors.IsNotFound(err) {
			log.Error("tunnel not found", "tunnel", tunnelName, "namespace", cfg.TenantNamespace)
			ui.Error("Tunnel %q not found", tunnelName)
			return fmt.Errorf("tunnel %q not found", tunnelName)
		}
		log.Error("failed to get tunnel", "error", err)
		return fmt.Errorf("failed to get tunnel: %w", err)
	}

	log.Debug("tunnel resource retrieved", "status", tunnel.Status.Phase, "url", tunnel.Status.URL)

	if tunnel.Status.Phase != kubelbce.TunnelPhaseReady {
		log.Warn("tunnel is not ready", "status", tunnel.Status.Phase)
		ui.Error("Tunnel is not ready (status: %s)", tunnel.Status.Phase)
		return fmt.Errorf("tunnel is not ready (status: %s)", tunnel.Status.Phase)
	}

	if tunnel.Status.ConnectionManagerURL == "" {
		log.Error("connection manager URL not available")
		ui.Error("Tunnel connection manager URL not available")
		return fmt.Errorf("tunnel connection manager URL not available")
	}

	log.Debug("loading tunnel authentication")
	// Load tunnel authentication
	auth, err := LoadTunnelAuth(ctx, k8s, cfg.TenantNamespace, tunnelName)
	if err != nil {
		log.Error("failed to load tunnel auth", "error", err)
		return fmt.Errorf("failed to load tunnel auth: %w", err)
	}

	log.Debug("creating tunnel client", "manager_url", tunnel.Status.ConnectionManagerURL)
	client, err := NewClient(auth, tunnel.Status.ConnectionManagerURL, tunnelName, tunnel.Status.Hostname, cfg.TenantNamespace, fmt.Sprintf("%d", port), cfg.InsecureSkipVerify)
	if err != nil {
		log.Error("failed to create tunnel client", "error", err)
		return fmt.Errorf("failed to create tunnel client: %w", err)
	}
	defer client.Close()

	// Display connection information
	ui.Info("%s", output.FormatConnectionInfo(tunnel.Status.URL, fmt.Sprintf("%d", port)))
	if cfg.InsecureSkipVerify {
		ui.Warning("TLS verification disabled")
		log.Warn("TLS verification disabled", "insecure_skip_verify", true)
	}

	// Create a fresh context for the long-running tunnel connection
	// Don't inherit timeout from the parent context to avoid 4-minute death sentence
	tunnelCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle Ctrl+C gracefully
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Info("received shutdown signal")
		ui.Disconnection("Disconnecting tunnel...")
		cancel()
	}()

	return client.EstablishTunnel(tunnelCtx)
}

// HTTPRequest represents an incoming HTTP request to be forwarded
type HTTPRequest struct {
	RequestID string            `json:"request_id"`
	Method    string            `json:"method"`
	Path      string            `json:"path"`
	Headers   map[string]string `json:"headers"`
	Body      string            `json:"body"` // base64 encoded
}

// HTTPResponse represents the response from the local service
type HTTPResponse struct {
	RequestID  string            `json:"request_id"`
	StatusCode int               `json:"status_code"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"` // base64 encoded
}

// ConnectionState represents the current state of the tunnel connection
type ConnectionState int32

const (
	StateDisconnected ConnectionState = iota
	StateConnecting
	StateConnected
	StateReconnecting
	StateFailed
)

func (s ConnectionState) String() string {
	switch s {
	case StateDisconnected:
		return "disconnected"
	case StateConnecting:
		return "connecting"
	case StateConnected:
		return "connected"
	case StateReconnecting:
		return "reconnecting"
	case StateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// Client manages the HTTP/2 connection to the tunnel service
type Client struct {
	httpClient *http.Client // For regular HTTP requests (with timeout)
	sseClient  *http.Client // For SSE connections (no timeout)
	baseURL    string
	auth       *Auth
	tunnelName string // Tunnel resource name (e.g., "my-app")
	hostname   string // Tunnel hostname (e.g., "my-app.example.com")
	tenantName string // Tenant namespace for security isolation
	targetPort string

	// Connection state management
	state          int32          // atomic ConnectionState
	reconnectCount int32          // atomic reconnection counter
	requestWg      sync.WaitGroup // track in-flight requests
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
	needsReconnect int32 // atomic flag indicating reconnection needed

	// HTTP/2 fallback management
	useHTTP1 int32 // atomic flag to use HTTP/1.1 instead of HTTP/2
}

// NewClient creates a new tunnel client with HTTP/2
func NewClient(auth *Auth, connectionManagerURL, tunnelName, hostname, tenantName, targetPort string, insecureSkipVerify bool) (*Client, error) {
	baseURL, err := parseConnectionURL(connectionManagerURL)
	if err != nil {
		return nil, fmt.Errorf("invalid connection manager URL: %w", err)
	}

	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: insecureSkipVerify,
	}
	transport := &http2.Transport{
		TLSClientConfig: tlsConfig,
		// Allow multiple connections
		AllowHTTP: false,
	}
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	sseClient := &http.Client{
		Transport: transport,
	}

	// Create shutdown context for coordinating graceful shutdown
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())

	client := &Client{
		httpClient:     httpClient,
		sseClient:      sseClient,
		baseURL:        baseURL,
		auth:           auth,
		tunnelName:     tunnelName,
		hostname:       hostname,
		tenantName:     tenantName,
		targetPort:     targetPort,
		shutdownCtx:    shutdownCtx,
		shutdownCancel: shutdownCancel,
	}

	// Initialize connection state
	atomic.StoreInt32(&client.state, int32(StateDisconnected))
	atomic.StoreInt32(&client.reconnectCount, 0)
	atomic.StoreInt32(&client.needsReconnect, 0)
	atomic.StoreInt32(&client.useHTTP1, 0)

	return client, nil
}

// recreateClientsWithHTTP1 recreates HTTP clients using HTTP/1.1 instead of HTTP/2
func (tc *Client) recreateClientsWithHTTP1() {
	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: tc.httpClient.Transport.(*http2.Transport).TLSClientConfig.InsecureSkipVerify,
	}

	// Use regular HTTP transport instead of HTTP/2
	transport := &http.Transport{
		TLSClientConfig:   tlsConfig,
		MaxIdleConns:      10,
		IdleConnTimeout:   30 * time.Second,
		DisableKeepAlives: false,
	}

	tc.httpClient = &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	tc.sseClient = &http.Client{
		Transport: transport,
		// No timeout for SSE connections
	}
}

func (tc *Client) Close() error {
	tc.shutdownCancel()

	done := make(chan struct{})
	go func() {
		tc.requestWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// All requests completed
	case <-time.After(10 * time.Second):
		// Force shutdown after timeout
		fmt.Printf("⚠️  Force shutdown after timeout\n")
	}
	atomic.StoreInt32(&tc.state, int32(StateDisconnected))

	return nil
}

// EstablishTunnel creates SSE connection with automatic reconnection
func (tc *Client) EstablishTunnel(ctx context.Context) error {
	const maxRetries = 10
	const baseDelay = 1 * time.Second
	const maxDelay = 60 * time.Second

	log := logger.WithTunnel(tc.tunnelName).WithOperation("establish_tunnel")

	log.Debug("starting tunnel establishment",
		"max_retries", maxRetries,
		"base_delay", baseDelay,
		"max_delay", maxDelay,
	)

	for attempt := 0; attempt < maxRetries; attempt++ {
		attemptLog := log.WithFields("attempt", attempt+1, "max_retries", maxRetries)

		// Check if we should stop retrying
		select {
		case <-ctx.Done():
			attemptLog.Info("context cancelled, stopping tunnel establishment")
			return ctx.Err()
		case <-tc.shutdownCtx.Done():
			attemptLog.Info("client shutdown requested, stopping tunnel establishment")
			return fmt.Errorf("client shutdown")
		default:
		}

		// Reset reconnection flag
		atomic.StoreInt32(&tc.needsReconnect, 0)

		// Update connection state
		if attempt == 0 {
			atomic.StoreInt32(&tc.state, int32(StateConnecting))
			ui.Progress("Connecting to tunnel...")
			attemptLog.Debug("initial connection attempt")
		} else {
			atomic.StoreInt32(&tc.state, int32(StateReconnecting))
			atomic.AddInt32(&tc.reconnectCount, 1)

			// Calculate exponential backoff delay with jitter
			multiplier := 1 << uint(attempt-1) // 2^(attempt-1)
			delay := time.Duration(int64(baseDelay) * int64(multiplier))
			if delay > maxDelay {
				delay = maxDelay
			}
			// Add jitter (±25%)
			jitter := time.Duration(rand.Float64() * float64(delay) * 0.5)
			delay = delay - jitter/2 + time.Duration(rand.Float64()*float64(jitter))

			attemptLog.Debug("calculating reconnection delay", "delay", delay, "jitter", jitter)
			ui.Progress("Reconnection attempt %d/%d in %v...", attempt+1, maxRetries, delay.Round(time.Second))

			select {
			case <-time.After(delay):
			case <-ctx.Done():
				attemptLog.Info("context cancelled during delay")
				return ctx.Err()
			case <-tc.shutdownCtx.Done():
				attemptLog.Info("client shutdown during delay")
				return fmt.Errorf("client shutdown")
			}
		}

		// Attempt to establish connection
		attemptLog.Debug("attempting to establish connection")
		err := tc.establishSingleConnection(ctx)
		if err == nil {
			// This should never happen since handleSSEEvents blocks until error
			attemptLog.Warn("establish single connection returned nil error unexpectedly")
			return nil
		}

		// Check if this is a graceful shutdown (context canceled)
		if errors.Is(err, context.Canceled) {
			attemptLog.Info("connection cancelled gracefully")
			return err
		}

		// Check if this is a grace period error - retry immediately without delay
		if tc.isGracePeriodError(err) {
			attemptLog.Info("Grace period detected, retrying immediately")
			continue // Skip delay and try again immediately
		}

		// Check if this is an HTTP/2 stream error and we should fallback to HTTP/1.1
		if tc.isHTTP2StreamError(err) && attempt >= 3 {
			attemptLog.Info("Multiple HTTP/2 stream errors detected, falling back to HTTP/1.1")
			atomic.StoreInt32(&tc.useHTTP1, 1)
			ui.Info("Switching to HTTP/1.1 due to connection issues...")
		}

		// Log connection failure for actual errors
		attemptLog.Warn("connection attempt failed", "error", err)
		ui.Error("Connection attempt %d failed: %v", attempt+1, err)

		// Check if error is non-recoverable
		if tc.isNonRecoverableError(err) {
			atomic.StoreInt32(&tc.state, int32(StateFailed))
			attemptLog.Error("non-recoverable error encountered", "error", err)
			return fmt.Errorf("non-recoverable error: %w", err)
		}
	}

	// All retries exhausted
	atomic.StoreInt32(&tc.state, int32(StateFailed))
	log.Error("all retry attempts exhausted", "attempts", maxRetries)
	ui.Error("Failed to establish tunnel after %d attempts", maxRetries)
	return fmt.Errorf("failed to establish tunnel after %d attempts", maxRetries)
}

// establishSingleConnection attempts to create a single SSE connection
func (tc *Client) establishSingleConnection(ctx context.Context) error {
	log := logger.WithTunnel(tc.tunnelName).WithOperation("establish_single_connection")

	// Switch to HTTP/1.1 if flagged due to HTTP/2 issues
	if atomic.LoadInt32(&tc.useHTTP1) == 1 {
		tc.recreateClientsWithHTTP1()
		log.Info("switched to HTTP/1.1 transport for connection stability")
	}

	connectURL := tc.baseURL + "/tunnel/connect"
	log.Debug("creating SSE request", "url", connectURL)

	req, err := http.NewRequestWithContext(ctx, "GET", connectURL, nil)
	if err != nil {
		log.Error("failed to create SSE request", "error", err)
		return fmt.Errorf("failed to create SSE request: %w", err)
	}
	authHeader := "Bearer " + tc.auth.Token

	// Validate headers contain only valid HTTP header characters
	log.Debug("validating HTTP headers")
	if !isValidHTTPHeaderValue(authHeader) {
		var invalidChars []rune
		for _, c := range authHeader {
			if c < 0x20 || (c >= 0x7F && c < 0x80) {
				invalidChars = append(invalidChars, c)
			}
		}
		log.Error("invalid characters in auth token", "invalid_chars", invalidChars, "token_length", len(tc.auth.Token))
		return fmt.Errorf("invalid characters in auth token: %v (token length: %d)", invalidChars, len(tc.auth.Token))
	}
	if !isValidHTTPHeaderValue(tc.tunnelName) {
		log.Error("invalid characters in tunnel name", "tunnel_name", tc.tunnelName)
		return fmt.Errorf("invalid characters in tunnel name: %q", tc.tunnelName)
	}
	if !isValidHTTPHeaderValue(tc.tenantName) {
		log.Error("invalid characters in tenant name", "tenant_name", tc.tenantName)
		return fmt.Errorf("invalid characters in tenant name: %q", tc.tenantName)
	}

	req.Header.Set("Authorization", authHeader)
	req.Header.Set("X-Tunnel-Hostname", tc.hostname)
	req.Header.Set("X-Target-Port", tc.targetPort)
	req.Header.Set("X-Tunnel-Name", tc.tunnelName)
	req.Header.Set("X-Tenant-Name", tc.tenantName)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	log.Debug("sending SSE connection request",
		"hostname", tc.hostname,
		"target_port", tc.targetPort,
		"tunnel_name", tc.tunnelName,
		"tenant_name", tc.tenantName,
	)

	// Make the request using SSE client (no timeout)
	resp, err := tc.sseClient.Do(req)
	if err != nil {
		log.Error("failed to connect to tunnel server", "error", err, "url", connectURL)
		return fmt.Errorf("failed to connect to tunnel server: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		bodyStr := string(body)

		// Check if this is a grace period response (503 Service Unavailable)
		if resp.StatusCode == http.StatusServiceUnavailable && strings.Contains(bodyStr, "grace period") {
			retryAfter := resp.Header.Get("Retry-After")
			if retryAfter != "" {
				if seconds, err := strconv.Atoi(retryAfter); err == nil {
					log.Info("Tunnel in grace period, will retry immediately",
						"retry_after_seconds", seconds,
						"response", bodyStr)
					ui.Info("Tunnel reconnecting (was in grace period)...")

					// Return special error for immediate retry (no delay)
					return fmt.Errorf("grace_period_active: %s", bodyStr)
				}
			}

			log.Info("Tunnel in grace period, retrying", "response", bodyStr)
			return fmt.Errorf("grace_period_active: %s", bodyStr)
		}

		log.Error("tunnel connection failed", "status", resp.StatusCode, "body", bodyStr)
		return fmt.Errorf("tunnel connection failed: %s (status: %d)", bodyStr, resp.StatusCode)
	}

	// Connection successful - transition to connected state
	atomic.StoreInt32(&tc.state, int32(StateConnected))
	log.Info("tunnel connection established successfully",
		"tunnel_name", tc.tunnelName,
		"hostname", tc.hostname,
		"target_port", tc.targetPort,
		"connection_manager_url", tc.baseURL)
	ui.Success("Connected! Tunnel is ready to receive traffic")
	ui.Info("Press Ctrl+C to disconnect")

	// Handle SSE events - this blocks until connection dies or context is cancelled
	return tc.handleSSEEvents(ctx, resp.Body)
}

// isGracePeriodError determines if an error indicates tunnel is in grace period
func (tc *Client) isGracePeriodError(err error) bool {
	errStr := err.Error()
	return strings.Contains(errStr, "grace_period_active") ||
		strings.Contains(errStr, "grace period")
}

// isHTTP2StreamError determines if an error is related to HTTP/2 stream issues
func (tc *Client) isHTTP2StreamError(err error) bool {
	errStr := err.Error()
	return strings.Contains(errStr, "stream error") ||
		strings.Contains(errStr, "stream ID") ||
		strings.Contains(errStr, "NO_ERROR; received from peer")
}

// isNonRecoverableError determines if an error should stop reconnection attempts
func (tc *Client) isNonRecoverableError(err error) bool {
	errStr := err.Error()
	return strings.Contains(errStr, "401") || // Unauthorized
		strings.Contains(errStr, "403") || // Forbidden
		strings.Contains(errStr, "invalid token") ||
		strings.Contains(errStr, "authentication failed")
}

// handleSSEEvents handles incoming SSE events from the server
func (tc *Client) handleSSEEvents(ctx context.Context, body io.Reader) error {
	log := logger.WithTunnel(tc.tunnelName).WithOperation("handle_sse_events")
	log.Debug("starting SSE event handling")

	scanner := bufio.NewScanner(body)
	eventCount := 0
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			log.Info("context cancelled, stopping SSE event handling")
			return ctx.Err()
		case <-tc.shutdownCtx.Done():
			log.Info("client shutdown, stopping SSE event handling")
			return fmt.Errorf("client shutdown")
		default:
		}

		line := scanner.Text()
		if line == "" {
			continue // Skip empty lines
		}

		// Parse SSE event
		if strings.HasPrefix(line, "event: ") {
			eventType := strings.TrimPrefix(line, "event: ")

			// Read the data line
			if !scanner.Scan() {
				break
			}
			dataLine := scanner.Text()
			if !strings.HasPrefix(dataLine, "data: ") {
				continue
			}
			data := strings.TrimPrefix(dataLine, "data: ")

			eventCount++
			eventLog := log.WithFields("event_type", eventType, "event_count", eventCount)

			// Show all SSE events at trace level
			eventLog.Debug("received SSE event", "data_preview", truncateString(data, 100))

			// Handle different event types
			switch eventType {
			case "request":
				eventLog.Debug("handling request event")
				go tc.handleRequestEventWithTracking(data)
			case "error":
				eventLog.Error("received error event from server", "error", data)
				ui.Error("Server error: %s", data)
				return fmt.Errorf("server error: %s", data)
			default:
				eventLog.Warn("received unknown event type", "event_type", eventType, "data", data)
			}

		}
	}

	if err := scanner.Err(); err != nil {
		// Don't return error for graceful shutdown
		if errors.Is(err, context.Canceled) {
			log.Info("scanner cancelled gracefully")
			return context.Canceled
		}
		log.Error("error reading SSE stream", "error", err, "events_processed", eventCount)
		return fmt.Errorf("error reading SSE stream: %w", err)
	}

	log.Warn("SSE stream ended unexpectedly", "events_processed", eventCount)
	return fmt.Errorf("SSE stream ended unexpectedly")
}

// truncateString truncates a string to the specified length.
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// handleRequestEventWithTracking processes incoming HTTP request events
func (tc *Client) handleRequestEventWithTracking(data string) {
	log := logger.WithTunnel(tc.tunnelName).WithOperation("handle_request")

	var req HTTPRequest
	if err := json.Unmarshal([]byte(data), &req); err != nil {
		log.Error("failed to parse request", "error", err, "data_preview", truncateString(data, 200))
		ui.Error("Failed to parse request: %v", err)
		return
	}

	log.Debug("parsed request",
		"request_id", req.RequestID,
		"method", req.Method,
		"path", req.Path,
		"headers_count", len(req.Headers),
	)

	tc.forwardToLocalService(&req)
}

// Create a shared HTTP client with connection pooling (optimized for fast response)
var localHTTPClient = &http.Client{
	Timeout: 8 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:          50,
		MaxIdleConnsPerHost:   5,
		IdleConnTimeout:       30 * time.Second,
		DisableCompression:    false,
		ResponseHeaderTimeout: 5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

// forwardToLocalService forwards HTTP request to local application
func (tc *Client) forwardToLocalService(req *HTTPRequest) {
	log := logger.WithTunnel(tc.tunnelName).WithOperation("forward_request").
		WithFields("request_id", req.RequestID, "method", req.Method, "path", req.Path)

	startTime := time.Now()
	log.Debug("forwarding request to local service", "target_port", tc.targetPort)

	body, err := base64.StdEncoding.DecodeString(req.Body)
	if err != nil {
		log.Error("failed to decode request body", "error", err)
		tc.sendError(req.RequestID, fmt.Sprintf("Invalid request body: %v", err))
		return
	}

	localURL := fmt.Sprintf("http://localhost:%s%s", tc.targetPort, req.Path)
	log.Debug("sending request to local URL", "url", localURL, "body_size", len(body))

	// Create simple HTTP request (timeout handled by client)
	httpReq, err := http.NewRequestWithContext(context.Background(), req.Method, localURL, bytes.NewReader(body))
	if err != nil {
		log.Error("failed to create HTTP request", "error", err)
		tc.sendError(req.RequestID, fmt.Sprintf("Invalid request: %v", err))
		return
	}

	// Set headers
	for key, value := range req.Headers {
		httpReq.Header.Set(key, value)
	}
	log.Debug("request headers set", "header_count", len(req.Headers))

	resp, err := localHTTPClient.Do(httpReq)
	if err != nil {
		log.Error("local request failed", "error", err, "duration", time.Since(startTime))
		tc.sendError(req.RequestID, fmt.Sprintf("Request failed: %v", err))
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Error("failed to read response body", "error", err)
		tc.sendError(req.RequestID, fmt.Sprintf("Failed to read response: %v", err))
		return
	}

	duration := time.Since(startTime)
	log.Debug("request forwarded successfully",
		"status_code", resp.StatusCode,
		"response_size", len(respBody),
		"duration", duration,
	)

	// Hop-by-hop headers that should not be forwarded
	hopByHopHeaders := map[string]bool{
		"Connection":          true,
		"Keep-Alive":          true,
		"Proxy-Authenticate":  true,
		"Proxy-Authorization": true,
		"Te":                  true,
		"Trailers":            true,
		"Transfer-Encoding":   true,
		"Upgrade":             true,
	}

	// Convert headers
	headers := make(map[string]string)
	for key, values := range resp.Header {
		// Skip hop-by-hop headers
		if hopByHopHeaders[key] {
			continue
		}
		if len(values) > 0 {
			// Join multiple values with comma for proper header handling
			headers[key] = strings.Join(values, ", ")
		}
	}

	log.Debug("response headers processed", "filtered_header_count", len(headers))

	httpResp := &HTTPResponse{
		RequestID:  req.RequestID,
		StatusCode: resp.StatusCode,
		Headers:    headers,
		Body:       base64.StdEncoding.EncodeToString(respBody),
	}

	// Send response back to server
	if err := tc.sendResponse(httpResp); err != nil {
		// Check if we're shutting down before logging error
		select {
		case <-tc.shutdownCtx.Done():
			log.Debug("not logging send error due to shutdown")
			return
		default:
			// Don't log if it's a known 404 issue (tunnel deregistered)
			if !strings.Contains(err.Error(), "reconnection triggered") {
				log.Error("failed to send response", "error", err)
				ui.Error("Failed to send response for %s %s: %v", req.Method, req.Path, err)
			} else {
				log.Debug("response send failed due to tunnel deregistration", "error", err)
			}
		}
	} else {
		log.Debug("response sent successfully")
	}
}

// sendResponse sends an HTTP response back to the server
func (tc *Client) sendResponse(resp *HTTPResponse) error {
	respURL := tc.baseURL + "/tunnel/response"
	respData, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("failed to marshal response: %w", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), "POST", respURL, bytes.NewReader(respData))
	if err != nil {
		return fmt.Errorf("failed to create response request: %w", err)
	}

	authHeader := "Bearer " + tc.auth.Token

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("X-Tunnel-Name", tc.tunnelName)
	req.Header.Set("X-Tenant-Name", tc.tenantName)

	// Send response using HTTP client with built-in timeout
	httpResp, err := tc.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send response: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode == 404 {
		// This indicates the tunnel is no longer registered on the server
		body, _ := io.ReadAll(httpResp.Body)
		log := logger.WithTunnel(tc.tunnelName).WithOperation("send_response")
		log.Error("tunnel not found on server",
			"status", httpResp.StatusCode,
			"response_body", string(body),
			"request_id", resp.RequestID,
			"connection_state", ConnectionState(atomic.LoadInt32(&tc.state)).String())
		// Set flag for reconnection needed
		atomic.StoreInt32(&tc.needsReconnect, 1)
		return fmt.Errorf("tunnel not found on server (404), may need reconnection")
	}

	if httpResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(httpResp.Body)
		return fmt.Errorf("server returned error: %d - %s", httpResp.StatusCode, string(body))
	}

	return nil
}

func (tc *Client) sendError(requestID, message string) {
	errorResp := &HTTPResponse{
		RequestID:  requestID,
		StatusCode: 500,
		Headers:    map[string]string{"Content-Type": "text/plain"},
		Body:       base64.StdEncoding.EncodeToString([]byte(message)),
	}

	if err := tc.sendResponse(errorResp); err != nil {
		// Check if we're shutting down before logging
		select {
		case <-tc.shutdownCtx.Done():
			return
		default:
			fmt.Printf("❌ Failed to send error response: %v\n", err)
		}
	}
}

// isValidHTTPHeaderValue checks if a string contains only valid HTTP header characters
func isValidHTTPHeaderValue(value string) bool {
	for _, c := range value {
		// HTTP header values can contain: VCHAR, WSP, and obs-text
		// VCHAR = %x21-7E (visible ASCII chars)
		// WSP = SP / HTAB (space or horizontal tab)
		// obs-text = %x80-FF (for backward compatibility)
		if c < 0x20 || (c >= 0x7F && c < 0x80) {
			return false
		}
	}
	return true
}

// parseConnectionURL parses the connection manager URL and returns base URL
func parseConnectionURL(connectionURL string) (string, error) {
	// If the URL doesn't have a scheme, add HTTPS
	if !strings.Contains(connectionURL, "://") {
		connectionURL = "https://" + connectionURL
	}

	parsedURL, err := url.Parse(connectionURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse URL: %w", err)
	}

	if parsedURL.Host == "" {
		return "", fmt.Errorf("no host found in URL")
	}
	baseURL := fmt.Sprintf("%s://%s", parsedURL.Scheme, parsedURL.Host)
	return baseURL, nil
}
