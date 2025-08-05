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
	"k8c.io/kubelb-cli/internal/output"
	kubelbce "k8c.io/kubelb/api/ce/kubelb.k8c.io/v1alpha1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func Connect(ctx context.Context, k8s client.Client, cfg *config.Config, tunnelName string, port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("invalid port: %d (must be between 1 and 65535)", port)
	}

	tunnel := &kubelbce.Tunnel{}
	if err := k8s.Get(ctx, client.ObjectKey{
		Namespace: cfg.TenantNamespace,
		Name:      tunnelName,
	}, tunnel); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("tunnel %q not found", tunnelName)
		}
		return fmt.Errorf("failed to get tunnel: %w", err)
	}

	if tunnel.Status.Phase != kubelbce.TunnelPhaseReady {
		return fmt.Errorf("tunnel is not ready (status: %s)", tunnel.Status.Phase)
	}

	if tunnel.Status.ConnectionManagerURL == "" {
		return fmt.Errorf("tunnel connection manager URL not available")
	}

	// Load tunnel authentication
	auth, err := LoadTunnelAuth(ctx, k8s, cfg.TenantNamespace, tunnelName)
	if err != nil {
		return fmt.Errorf("failed to load tunnel auth: %w", err)
	}

	client, err := NewClient(auth, tunnel.Status.ConnectionManagerURL, tunnelName, tunnel.Status.Hostname, cfg.TenantNamespace, fmt.Sprintf("%d", port), cfg.InsecureSkipVerify)
	if err != nil {
		return fmt.Errorf("failed to create tunnel client: %w", err)
	}
	defer client.Close()

	fmt.Printf("\n🔗 Tunnel Connection\n")
	fmt.Printf("%s\n", output.FormatConnectionInfo(tunnel.Status.URL, fmt.Sprintf("%d", port)))
	if cfg.InsecureSkipVerify {
		fmt.Printf("   ⚠️  TLS verification disabled\n")
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
		fmt.Printf("\n💔 Disconnecting tunnel...\n")
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
	lastPing       int64          // atomic unix timestamp of last ping from server
	reconnectCount int32          // atomic reconnection counter
	requestWg      sync.WaitGroup // track in-flight requests
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
	needsReconnect int32 // atomic flag indicating reconnection needed
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
	atomic.StoreInt64(&client.lastPing, 0)
	atomic.StoreInt32(&client.reconnectCount, 0)
	atomic.StoreInt32(&client.needsReconnect, 0)

	return client, nil
}

func (tc *Client) Close() error {
	// Signal shutdown to all goroutines
	tc.shutdownCancel()

	// Wait for in-flight requests to complete with timeout
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

	// Update connection state
	atomic.StoreInt32(&tc.state, int32(StateDisconnected))

	return nil
}

// EstablishTunnel creates SSE connection with automatic reconnection
func (tc *Client) EstablishTunnel(ctx context.Context) error {
	const maxRetries = 10
	const baseDelay = 1 * time.Second
	const maxDelay = 60 * time.Second

	for attempt := 0; attempt < maxRetries; attempt++ {
		// Check if we should stop retrying
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tc.shutdownCtx.Done():
			return fmt.Errorf("client shutdown")
		default:
		}

		// Reset reconnection flag
		atomic.StoreInt32(&tc.needsReconnect, 0)

		// Update connection state
		if attempt == 0 {
			atomic.StoreInt32(&tc.state, int32(StateConnecting))
			fmt.Printf("🔄 Connecting to tunnel...\n")
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

			fmt.Printf("🔄 Reconnection attempt %d/%d in %v...\n", attempt+1, maxRetries, delay.Round(time.Second))

			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			case <-tc.shutdownCtx.Done():
				return fmt.Errorf("client shutdown")
			}
		}

		// Attempt to establish connection
		err := tc.establishSingleConnection(ctx)
		if err == nil {
			// This should never happen since handleSSEEvents blocks until error
			return nil
		}

		// Check if this is a graceful shutdown (context canceled)
		if errors.Is(err, context.Canceled) {
			// Don't log error for graceful shutdown
			return err
		}

		// Log connection failure for actual errors
		fmt.Printf("❌ Connection attempt %d failed: %v\n", attempt+1, err)

		// Check if error is non-recoverable
		if tc.isNonRecoverableError(err) {
			atomic.StoreInt32(&tc.state, int32(StateFailed))
			return fmt.Errorf("non-recoverable error: %w", err)
		}
	}

	// All retries exhausted
	atomic.StoreInt32(&tc.state, int32(StateFailed))
	return fmt.Errorf("failed to establish tunnel after %d attempts", maxRetries)
}

// establishSingleConnection attempts to create a single SSE connection
func (tc *Client) establishSingleConnection(ctx context.Context) error {
	connectURL := tc.baseURL + "/tunnel/connect"
	req, err := http.NewRequestWithContext(ctx, "GET", connectURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create SSE request: %w", err)
	}
	authHeader := "Bearer " + tc.auth.Token

	// Validate headers contain only valid HTTP header characters
	if !isValidHTTPHeaderValue(authHeader) {
		var invalidChars []rune
		for _, c := range authHeader {
			if c < 0x20 || (c >= 0x7F && c < 0x80) {
				invalidChars = append(invalidChars, c)
			}
		}
		return fmt.Errorf("invalid characters in auth token: %v (token length: %d)", invalidChars, len(tc.auth.Token))
	}
	if !isValidHTTPHeaderValue(tc.tunnelName) {
		return fmt.Errorf("invalid characters in tunnel name: %q", tc.tunnelName)
	}
	if !isValidHTTPHeaderValue(tc.tenantName) {
		return fmt.Errorf("invalid characters in tenant name: %q", tc.tenantName)
	}

	req.Header.Set("Authorization", authHeader)
	req.Header.Set("X-Tunnel-Hostname", tc.hostname)
	req.Header.Set("X-Target-Port", tc.targetPort)
	req.Header.Set("X-Tunnel-Name", tc.tunnelName)
	req.Header.Set("X-Tenant-Name", tc.tenantName)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	// Make the request using SSE client (no timeout)
	resp, err := tc.sseClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to tunnel server: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("tunnel connection failed: %s (status: %d)", string(body), resp.StatusCode)
	}

	// Connection successful - transition to connected state
	atomic.StoreInt32(&tc.state, int32(StateConnected))
	fmt.Printf("   ✅ Connected! Tunnel is ready to receive traffic\n")
	fmt.Printf("   💡 Press Ctrl+C to disconnect\n\n")

	// Handle SSE events - this blocks until connection dies or context is cancelled
	return tc.handleSSEEvents(ctx, resp.Body)
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
	scanner := bufio.NewScanner(body)

	// Start connection health monitoring (server → client pings only)
	go tc.monitorConnectionHealth(ctx)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tc.shutdownCtx.Done():
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

			// Debug: Show all SSE events
			// fmt.Printf("🔍 SSE Event: type=%q, data=%q\n", eventType, data[:minInt(50, len(data))])

			// Handle different event types
			switch eventType {
			case "request":
				// DEBUG: Show all request events
				// fmt.Printf("📨 Received request event!\n")
				go tc.handleRequestEventWithTracking(data)
			case "ping":
				// Update last ping timestamp - proves server connection is alive
				atomic.StoreInt64(&tc.lastPing, time.Now().Unix())

				// Reduce noise: only log keepalive pings every minute (every 60th ping)
				if strings.HasPrefix(data, "keepalive-") {
					if pingCount := strings.TrimPrefix(data, "keepalive-"); pingCount != "" {
						if num, _ := strconv.Atoi(pingCount); num%60 == 0 {
							fmt.Printf("   🏓 Connection healthy (ping #%s)\n", pingCount)
						}
					}
				}
			case "pong":
				// Server acknowledgment - suppress this message to reduce noise
				// if data == "auth-ack" { ... }
			case "error":
				fmt.Printf("   ❌ Server error: %s\n", data)
				return fmt.Errorf("server error: %s", data)
			default:
				// Suppress unknown event types to reduce noise
				// fmt.Printf("⚠️  Unknown event type: %s, data: %s\n", eventType, data)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		// Don't return error for graceful shutdown
		if errors.Is(err, context.Canceled) {
			return context.Canceled
		}
		return fmt.Errorf("error reading SSE stream: %w", err)
	}

	return fmt.Errorf("SSE stream ended unexpectedly")
}

// monitorConnectionHealth monitors the health of the tunnel connection
func (tc *Client) monitorConnectionHealth(ctx context.Context) {
	const pingTimeout = 90

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	healthLogTicker := time.NewTicker(5 * time.Minute)
	defer healthLogTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tc.shutdownCtx.Done():
			return
		case <-ticker.C:
			now := time.Now().Unix()
			lastPing := atomic.LoadInt64(&tc.lastPing)
			// Check if reconnection was requested due to 404 errors
			if atomic.LoadInt32(&tc.needsReconnect) == 1 {
				fmt.Printf("   🔄 Connection lost, reconnecting...\n")
				return
			}

			// Check if we haven't received a ping in too long (connection issue)
			if lastPing > 0 && now-lastPing > pingTimeout {
				fmt.Printf("   ⚠️  Connection timeout (%d seconds), reconnecting...\n", now-lastPing)
				// Connection is dead, trigger reconnection by returning
				return
			}
		case <-healthLogTicker.C:
			// Suppress periodic health logs to reduce noise
			// Only show health when there are issues or reconnections
			// state := ConnectionState(atomic.LoadInt32(&tc.state))
			// reconnects := atomic.LoadInt32(&tc.reconnectCount)
			// fmt.Printf("📊 Connection healthy (state: %s, reconnects: %d)\n", state, reconnects)
		}
	}
}

// handleRequestEventWithTracking processes incoming HTTP request events
func (tc *Client) handleRequestEventWithTracking(data string) {
	var req HTTPRequest
	if err := json.Unmarshal([]byte(data), &req); err != nil {
		fmt.Printf("❌ Failed to parse request: %v\n", err)
		return
	}
	tc.forwardToLocalService(&req)
}

// Create a shared HTTP client with connection pooling (optimized for fast response)
var localHTTPClient = &http.Client{
	Timeout: 8 * time.Second, // Match server timeout expectations
	Transport: &http.Transport{
		MaxIdleConns:          50,
		MaxIdleConnsPerHost:   5,
		IdleConnTimeout:       30 * time.Second,
		DisableCompression:    false, // Allow compression for local connections
		ResponseHeaderTimeout: 5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

// forwardToLocalService forwards HTTP request to local application
func (tc *Client) forwardToLocalService(req *HTTPRequest) {
	body, err := base64.StdEncoding.DecodeString(req.Body)
	if err != nil {
		tc.sendError(req.RequestID, fmt.Sprintf("Invalid request body: %v", err))
		return
	}

	localURL := fmt.Sprintf("http://localhost:%s%s", tc.targetPort, req.Path)

	// Create simple HTTP request (timeout handled by client)
	httpReq, err := http.NewRequestWithContext(context.Background(), req.Method, localURL, bytes.NewReader(body))
	if err != nil {
		tc.sendError(req.RequestID, fmt.Sprintf("Invalid request: %v", err))
		return
	}
	for key, value := range req.Headers {
		httpReq.Header.Set(key, value)
	}
	resp, err := localHTTPClient.Do(httpReq)
	if err != nil {
		tc.sendError(req.RequestID, fmt.Sprintf("Request failed: %v", err))
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		tc.sendError(req.RequestID, fmt.Sprintf("Failed to read response: %v", err))
		return
	}

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
			// Don't log errors during shutdown
			return
		default:
			// Don't log if it's a known 404 issue (tunnel deregistered)
			if !strings.Contains(err.Error(), "reconnection triggered") {
				fmt.Printf("❌ Failed to send response for %s %s: %v\n", req.Method, req.Path, err)
			}
		}
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
