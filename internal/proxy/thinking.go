package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	hardTokenCap            = 32000
	minimumHeadroom         = 1024
	headroomRatio           = 0.1
	vercelGatewayHost       = "ai-gateway.vercel.sh"
	anthropicVersion        = "2023-06-01"
	betaInterleavedThinking = "interleaved-thinking-2025-05-14"
)

// VercelGatewayConfig holds configuration for routing Claude requests through Vercel AI Gateway.
type VercelGatewayConfig struct {
	Enabled bool
	APIKey  string
}

// IsActive returns true if the Vercel gateway is enabled and has a non-empty API key.
func (v VercelGatewayConfig) IsActive() bool {
	return v.Enabled && v.APIKey != ""
}

// CodebuffConfig holds configuration for routing requests through Codebuff's API.
type CodebuffConfig struct {
	Token string
}

// IsActive returns true if a Codebuff auth token is available.
func (c CodebuffConfig) IsActive() bool {
	return c.Token != ""
}

// ThinkingProxy is an HTTP reverse proxy that intercepts requests to add extended
// thinking parameters for Claude models based on model name suffixes.
//
// Model name pattern:
//   - *-thinking-NUMBER → Custom token budget (e.g., claude-sonnet-4-5-20250929-thinking-5000)
//
// The proxy strips the suffix and adds the `thinking` parameter to the request body
// before forwarding to CLIProxyAPI.
const maxRequestBodySize = 50 << 20 // 50 MB

// codebuffModels lists the OpenRouter model IDs available through Codebuff.
// These are injected into /v1/models responses with a "codebuff/" prefix.
var codebuffModels = []string{
	"anthropic/claude-opus-4-6",
	"anthropic/claude-sonnet-4-6",
	"anthropic/claude-sonnet-4.5",
	"anthropic/claude-4-sonnet-20250522",
	"anthropic/claude-opus-4.1",
	"anthropic/claude-3.5-sonnet-20240620",
	"anthropic/claude-3.5-haiku-20241022",
	"openai/gpt-5.1",
	"openai/gpt-4o-2024-11-20",
	"openai/gpt-4o-mini-2024-07-18",
	"openai/o3-mini-2025-01-31",
	"openai/gpt-4.1-nano",
	"google/gemini-2.5-pro",
	"google/gemini-2.5-flash",
	"x-ai/grok-4-07-09",
}

type ThinkingProxy struct {
	ProxyPort      int
	BackendPort    int
	VercelConfig   VercelGatewayConfig
	CodebuffConfig CodebuffConfig

	server           *http.Server
	isRunning        bool
	mu               sync.RWMutex
	ampClient        *http.Client
	vercelClient     *http.Client
	codebuffClient   *http.Client
	backendTransport *http.Transport
}

// IsRunning reports whether the proxy server is currently running.
func (tp *ThinkingProxy) IsRunning() bool {
	tp.mu.RLock()
	defer tp.mu.RUnlock()
	return tp.isRunning
}

// Start begins listening on the proxy port and serving requests in a background goroutine.
func (tp *ThinkingProxy) Start() error {
	tp.mu.Lock()
	if tp.isRunning {
		tp.mu.Unlock()
		log.Println("[ThinkingProxy] Already running")
		return nil
	}
	tp.mu.Unlock()

	// Initialize shared HTTP clients for connection pooling.
	tlsTransport := &http.Transport{
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}
	tp.ampClient = &http.Client{
		Transport: tlsTransport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	tp.vercelClient = &http.Client{
		Transport: tlsTransport,
		Timeout:   0, // No timeout for streaming
	}
	tp.codebuffClient = &http.Client{
		Transport: tlsTransport,
		Timeout:   0, // No timeout for streaming
	}
	tp.backendTransport = &http.Transport{
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 50,
		IdleConnTimeout:     90 * time.Second,
	}

	tp.server = &http.Server{
		Addr:         fmt.Sprintf(":%d", tp.ProxyPort),
		Handler:      tp,
		WriteTimeout: 0,
	}

	listener, err := net.Listen("tcp", tp.server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", tp.server.Addr, err)
	}

	tp.mu.Lock()
	tp.isRunning = true
	tp.mu.Unlock()
	log.Printf("[ThinkingProxy] Listening on port %d", tp.ProxyPort)

	go func() {
		if err := tp.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("[ThinkingProxy] Server error: %v", err)
		}
		tp.mu.Lock()
		tp.isRunning = false
		tp.mu.Unlock()
		log.Println("[ThinkingProxy] Stopped")
	}()

	return nil
}

// Stop gracefully shuts down the proxy server.
func (tp *ThinkingProxy) Stop() {
	tp.mu.Lock()
	if !tp.isRunning {
		tp.mu.Unlock()
		return
	}
	srv := tp.server
	tp.isRunning = false
	tp.mu.Unlock()

	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("[ThinkingProxy] Shutdown error: %v", err)
		}
	}

	log.Println("[ThinkingProxy] Stopped")
}

// ServeHTTP implements the http.Handler interface and contains all routing logic.
func (tp *ThinkingProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	log.Printf("[ThinkingProxy] Incoming request: %s %s", r.Method, path)

	// 0. Detect codebuff session from /cb/ path prefix (set by claudevibe for codebuff models).
	// Strip the prefix so all downstream routing works normally.
	isCodebuffSession := false
	if strings.HasPrefix(path, "/cb/") {
		isCodebuffSession = true
		path = strings.TrimPrefix(path, "/cb") // /cb/v1/messages → /v1/messages
		r.URL.Path = path
		log.Printf("[ThinkingProxy] Codebuff session detected, path: %s", path)
	}

	// 1. Redirect Amp CLI login directly to ampcode.com to preserve auth state cookies
	if strings.HasPrefix(path, "/auth/cli-login") || strings.HasPrefix(path, "/api/auth/cli-login") {
		loginPath := path
		if strings.HasPrefix(path, "/api/") {
			loginPath = path[4:] // strip /api prefix
		}
		redirectURL := "https://ampcode.com" + loginPath
		log.Printf("[ThinkingProxy] Redirecting Amp CLI login to: %s", redirectURL)
		http.Redirect(w, r, redirectURL, http.StatusFound)
		return
	}

	// 2. Rewrite Amp CLI paths: /provider/* → /api/provider/*
	rewrittenPath := path
	if strings.HasPrefix(path, "/provider/") {
		rewrittenPath = "/api" + path
		log.Printf("[ThinkingProxy] Rewriting Amp provider path: %s -> %s", path, rewrittenPath)
	}

	// 3. Check if this is an Amp management request (not targeting provider or /v1)
	isProviderPath := strings.HasPrefix(rewrittenPath, "/api/provider/")
	isCliProxyPath := strings.HasPrefix(rewrittenPath, "/v1/") || strings.HasPrefix(rewrittenPath, "/api/v1/")
	if !isProviderPath && !isCliProxyPath {
		log.Printf("[ThinkingProxy] Amp management request detected, forwarding to ampcode.com: %s", rewrittenPath)
		tp.forwardToAmp(w, r, rewrittenPath)
		return
	}

	// 4. Intercept GET /v1/models to inject Codebuff models when active.
	if r.Method == http.MethodGet && tp.CodebuffConfig.IsActive() {
		if path == "/v1/models" || path == "/api/v1/models" {
			tp.handleModelsWithCodebuff(w, r, isCodebuffSession)
			return
		}
	}

	// 5. Process thinking parameter for POST requests with body
	var bodyBytes []byte
	var modifiedBody []byte
	var thinkingEnabled bool

	if r.Method == http.MethodPost && r.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize))
		r.Body.Close()
		if err != nil {
			http.Error(w, "Failed to read request body", http.StatusBadRequest)
			return
		}

		// 5a. Auto-rewrite model for claudevibe codebuff sessions.
		// When the request came via /cb/ path prefix, resolve the clean model name
		// (e.g. "claude-opus-4-6") back to its full codebuff path
		// (e.g. "codebuff/anthropic/claude-opus-4-6") so the existing routing picks it up.
		if tp.CodebuffConfig.IsActive() && isCodebuffSession && len(bodyBytes) > 0 {
			bodyBytes = rewriteModelForCodebuff(bodyBytes)
		}

		// 5b. Check for Codebuff-routed requests (model starts with "codebuff/")
		if tp.CodebuffConfig.IsActive() && len(bodyBytes) > 0 {
			if isCodebuff, codebuffBody := stripCodebuffModelPrefix(bodyBytes); isCodebuff {
				log.Println("[ThinkingProxy] Routing request via Codebuff backend")
				tp.forwardToCodebuff(w, r, codebuffBody)
				return
			}
		}

		if len(bodyBytes) > 0 {
			modifiedBody, thinkingEnabled = tp.processThinkingParameter(bodyBytes)
		} else {
			modifiedBody = bodyBytes
		}
	}

	// 6. Route Claude requests through Vercel AI Gateway when configured
	if tp.VercelConfig.IsActive() && r.Method == http.MethodPost && isClaudeModelRequest(modifiedBody) {
		log.Println("[ThinkingProxy] Routing Claude request via Vercel AI Gateway")
		tp.forwardToVercel(w, r, modifiedBody, thinkingEnabled)
		return
	}

	// 7. Default: forward to CLIProxyAPI backend
	tp.forwardToBackend(w, r, rewrittenPath, modifiedBody, thinkingEnabled)
}

// isClaudeModelRequest checks if the JSON body contains a Claude model.
func isClaudeModelRequest(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	model, ok := payload["model"].(string)
	if !ok {
		return false
	}
	return strings.HasPrefix(model, "claude-") || strings.HasPrefix(model, "gemini-claude-")
}

// processThinkingParameter processes the JSON body to add thinking parameter if model
// name has a thinking suffix. Returns (modifiedBody, thinkingEnabled).
func (tp *ThinkingProxy) processThinkingParameter(body []byte) ([]byte, bool) {
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, false
	}

	model, ok := payload["model"].(string)
	if !ok {
		return body, false
	}

	// Only process Claude models (including gemini-claude variants)
	if !strings.HasPrefix(model, "claude-") && !strings.HasPrefix(model, "gemini-claude-") {
		return body, false // Not Claude, pass through
	}

	// Check for thinking suffix pattern: -thinking-NUMBER
	thinkingPrefix := "-thinking-"
	if idx := strings.LastIndex(model, thinkingPrefix); idx >= 0 {
		afterPrefix := model[idx+len(thinkingPrefix):]

		// For gemini-claude-* models, preserve "-thinking" and only strip the number
		// e.g. gemini-claude-opus-4-5-thinking-10000 -> gemini-claude-opus-4-5-thinking
		// For claude-* models, strip the entire suffix
		// e.g. claude-opus-4-5-20251101-thinking-10000 -> claude-opus-4-5-20251101
		var cleanModel string
		if strings.HasPrefix(model, "gemini-claude-") {
			cleanModel = model[:idx+len(thinkingPrefix)-1] // Keep "-thinking", drop trailing "-"
		} else {
			cleanModel = model[:idx]
		}
		payload["model"] = cleanModel

		// Only add thinking parameter if it's a valid integer
		budget, err := strconv.Atoi(afterPrefix)
		if err == nil && budget > 0 {
			effectiveBudget := budget
			if effectiveBudget > hardTokenCap-1 {
				effectiveBudget = hardTokenCap - 1
			}
			if effectiveBudget != budget {
				log.Printf("[ThinkingProxy] Adjusted thinking budget from %d to %d to stay within limits", budget, effectiveBudget)
			}

			// Add thinking parameter
			payload["thinking"] = map[string]interface{}{
				"type":          "enabled",
				"budget_tokens": effectiveBudget,
			}

			// Ensure max token limits are greater than the thinking budget
			// Claude requires: max_output_tokens (or legacy max_tokens) > thinking.budget_tokens
			tokenHeadroom := int(float64(effectiveBudget) * headroomRatio)
			if tokenHeadroom < minimumHeadroom {
				tokenHeadroom = minimumHeadroom
			}
			desiredMaxTokens := effectiveBudget + tokenHeadroom
			requiredMaxTokens := desiredMaxTokens
			if requiredMaxTokens > hardTokenCap {
				requiredMaxTokens = hardTokenCap
			}
			if requiredMaxTokens <= effectiveBudget {
				requiredMaxTokens = effectiveBudget + 1
				if requiredMaxTokens > hardTokenCap {
					requiredMaxTokens = hardTokenCap
				}
			}

			_, hasMaxOutputTokens := payload["max_output_tokens"]
			adjusted := false

			if currentMaxTokens, ok := payload["max_tokens"]; ok {
				if val, ok := toInt(currentMaxTokens); ok && val <= effectiveBudget {
					payload["max_tokens"] = requiredMaxTokens
				}
				adjusted = true
			}

			if currentMaxOutputTokens, ok := payload["max_output_tokens"]; ok {
				if val, ok := toInt(currentMaxOutputTokens); ok && val <= effectiveBudget {
					payload["max_output_tokens"] = requiredMaxTokens
				}
				adjusted = true
			}

			if !adjusted {
				if hasMaxOutputTokens {
					payload["max_output_tokens"] = requiredMaxTokens
				} else {
					payload["max_tokens"] = requiredMaxTokens
				}
			}

			log.Printf("[ThinkingProxy] Transformed model '%s' → '%s' with thinking budget %d", model, cleanModel, effectiveBudget)
		} else {
			// Invalid number - just strip suffix and use vanilla model
			log.Printf("[ThinkingProxy] Stripped invalid thinking suffix from '%s' → '%s' (no thinking)", model, cleanModel)
		}

		// Convert back to JSON
		modifiedData, err := json.Marshal(payload)
		if err != nil {
			return body, false
		}
		return modifiedData, true

	} else if strings.HasSuffix(model, "-thinking") || strings.Contains(model, "-thinking(") {
		// Model ends with -thinking or uses -thinking(budget) syntax
		// Enable beta header but don't modify body - let backend handle thinking budget
		log.Printf("[ThinkingProxy] Detected thinking model '%s' - enabling beta header, passing through to backend", model)
		return body, true
	}

	return body, false // No transformation needed
}

// toInt converts a JSON number value to int.
func toInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	}
	return 0, false
}

// forwardToAmp forwards Amp management requests to ampcode.com with header/cookie rewriting.
func (tp *ThinkingProxy) forwardToAmp(w http.ResponseWriter, r *http.Request, ampPath string) {
	targetURL := "https://ampcode.com" + ampPath
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		log.Printf("[ThinkingProxy] Error creating Amp request: %v", err)
		http.Error(w, "Bad Gateway - Could not connect to ampcode.com", http.StatusBadGateway)
		return
	}

	// Forward most headers, excluding some that need to be overridden
	excludedHeaders := map[string]bool{
		"host":              true,
		"connection":        true,
		"transfer-encoding": true,
	}
	for name, values := range r.Header {
		if excludedHeaders[strings.ToLower(name)] {
			continue
		}
		for _, v := range values {
			proxyReq.Header.Add(name, v)
		}
	}
	proxyReq.Header.Set("Host", "ampcode.com")
	proxyReq.Header.Set("Connection", "close")

	resp, err := tp.ampClient.Do(proxyReq)
	if err != nil {
		log.Printf("[ThinkingProxy] Connection to ampcode.com failed: %v", err)
		http.Error(w, "Bad Gateway - Could not connect to ampcode.com", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers with Location and cookie rewriting
	for name, values := range resp.Header {
		for _, v := range values {
			lowerName := strings.ToLower(name)
			if lowerName == "location" {
				v = rewriteLocationHeader(v)
			}
			if lowerName == "set-cookie" {
				v = rewriteCookieDomain(v)
			}
			w.Header().Add(name, v)
		}
	}

	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// rewriteLocationHeader rewrites Location header values for Amp proxying.
func rewriteLocationHeader(location string) string {
	// Rewrite absolute URLs first
	if strings.HasPrefix(location, "https://ampcode.com/") {
		return "/api/" + location[len("https://ampcode.com/"):]
	}
	if strings.HasPrefix(location, "http://ampcode.com/") {
		return "/api/" + location[len("http://ampcode.com/"):]
	}
	// Rewrite relative paths: / → /api/
	if strings.HasPrefix(location, "/") {
		return "/api" + location
	}
	return location
}

// rewriteCookieDomain rewrites cookie domains from ampcode.com to localhost.
func rewriteCookieDomain(cookie string) string {
	cookie = strings.ReplaceAll(cookie, "Domain=.ampcode.com", "Domain=localhost")
	cookie = strings.ReplaceAll(cookie, "Domain=ampcode.com", "Domain=localhost")
	// Case-insensitive variants
	cookie = strings.ReplaceAll(cookie, "domain=.ampcode.com", "Domain=localhost")
	cookie = strings.ReplaceAll(cookie, "domain=ampcode.com", "Domain=localhost")
	return cookie
}

// forwardToVercel forwards Claude requests to Vercel AI Gateway.
func (tp *ThinkingProxy) forwardToVercel(w http.ResponseWriter, r *http.Request, body []byte, thinkingEnabled bool) {
	targetURL := "https://" + vercelGatewayHost + "/v1/messages"

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("[ThinkingProxy] Error creating Vercel request: %v", err)
		http.Error(w, "Bad Gateway - Could not connect to Vercel AI Gateway", http.StatusBadGateway)
		return
	}

	// Build headers for Vercel - only forward specific headers
	excludedHeaders := map[string]bool{
		"host":              true,
		"content-length":    true,
		"connection":        true,
		"transfer-encoding": true,
		"authorization":     true,
		"x-api-key":         true,
		"anthropic-beta":    true,
	}

	var existingBetaHeader string
	for name, values := range r.Header {
		lower := strings.ToLower(name)
		if excludedHeaders[lower] {
			if lower == "anthropic-beta" && len(values) > 0 {
				existingBetaHeader = values[0]
			}
			continue
		}
		for _, v := range values {
			proxyReq.Header.Add(name, v)
		}
	}

	// Set Vercel-specific headers
	proxyReq.Header.Set("x-api-key", tp.VercelConfig.APIKey)
	proxyReq.Header.Set("anthropic-version", anthropicVersion)
	proxyReq.Header.Set("Content-Type", "application/json")
	proxyReq.Header.Set("Host", vercelGatewayHost)
	proxyReq.Header.Set("Connection", "close")

	// Thinking beta header
	if thinkingEnabled {
		betaValue := betaInterleavedThinking
		if existingBetaHeader != "" && !strings.Contains(existingBetaHeader, betaInterleavedThinking) {
			betaValue = existingBetaHeader + "," + betaInterleavedThinking
		}
		proxyReq.Header.Set("anthropic-beta", betaValue)
	} else if existingBetaHeader != "" {
		proxyReq.Header.Set("anthropic-beta", existingBetaHeader)
	}

	proxyReq.ContentLength = int64(len(body))

	resp, err := tp.vercelClient.Do(proxyReq)
	if err != nil {
		log.Printf("[ThinkingProxy] Vercel connection failed: %v", err)
		http.Error(w, "Bad Gateway - Could not connect to Vercel AI Gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers
	for name, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Stream the response body
	tp.streamResponse(w, resp.Body)
}

// forwardToBackend forwards requests to the CLIProxyAPI backend using httputil.ReverseProxy.
func (tp *ThinkingProxy) forwardToBackend(w http.ResponseWriter, r *http.Request, rewrittenPath string, body []byte, thinkingEnabled bool) {
	backendURL := &url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("127.0.0.1:%d", tp.BackendPort),
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = backendURL.Scheme
			req.URL.Host = backendURL.Host
			req.URL.Path = rewrittenPath
			req.Host = backendURL.Host

			// Handle body replacement if we have modified body
			if body != nil {
				req.Body = io.NopCloser(bytes.NewReader(body))
				req.ContentLength = int64(len(body))
				req.Header.Set("Content-Length", strconv.Itoa(len(body)))
			}

			// Remove hop-by-hop headers
			req.Header.Del("Transfer-Encoding")

			// Handle anthropic-beta header for thinking
			existingBeta := req.Header.Get("anthropic-beta")
			if thinkingEnabled {
				betaValue := betaInterleavedThinking
				if existingBeta != "" {
					if !strings.Contains(existingBeta, betaInterleavedThinking) {
						betaValue = existingBeta + "," + betaInterleavedThinking
					} else {
						betaValue = existingBeta
					}
				}
				req.Header.Set("anthropic-beta", betaValue)
				log.Println("[ThinkingProxy] Added interleaved thinking beta header")
			}
		},
		Transport:     tp.backendTransport,
		FlushInterval: -1, // Flush immediately for streaming
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("[ThinkingProxy] Backend proxy error: %v", err)
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		},
	}

	proxy.ServeHTTP(w, r)
}

// stripCodebuffModelPrefix checks if the JSON body has a model starting with "codebuff/".
// If so, it strips the prefix and returns (true, modifiedBody). Otherwise (false, original).
func stripCodebuffModelPrefix(body []byte) (bool, []byte) {
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false, body
	}
	model, ok := payload["model"].(string)
	if !ok || !strings.HasPrefix(model, "codebuff/") {
		return false, body
	}
	payload["model"] = strings.TrimPrefix(model, "codebuff/")
	modified, err := json.Marshal(payload)
	if err != nil {
		return false, body
	}
	log.Printf("[ThinkingProxy] Codebuff model: %s → %s", model, payload["model"])
	return true, modified
}

// rewriteModelForCodebuff resolves a clean model name (e.g. "claude-opus-4-6") to its
// full codebuff-prefixed path (e.g. "codebuff/anthropic/claude-opus-4-6") by matching
// against the known codebuffModels list. Used for claudevibe sessions where the clean
// model name is passed to Claude Code but needs codebuff routing in the proxy.
func rewriteModelForCodebuff(body []byte) []byte {
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	model, ok := payload["model"].(string)
	if !ok || model == "" || strings.HasPrefix(model, "codebuff/") {
		return body
	}
	model = normalizeCodebuffModelAlias(model)
	payload["model"] = model
	for _, cm := range codebuffModels {
		if cm == model {
			payload["model"] = "codebuff/" + cm
			modified, err := json.Marshal(payload)
			if err != nil {
				return body
			}
			log.Printf("[ThinkingProxy] Codebuff auto-rewrite: %s → %s", model, payload["model"])
			return modified
		}
		idx := strings.Index(cm, "/")
		if idx >= 0 && cm[idx+1:] == model {
			payload["model"] = "codebuff/" + cm
			modified, err := json.Marshal(payload)
			if err != nil {
				return body
			}
			log.Printf("[ThinkingProxy] Codebuff auto-rewrite: %s → %s", model, payload["model"])
			return modified
		}
	}
	if strings.HasPrefix(model, "claude-") {
		payload["model"] = "codebuff/anthropic/" + model
		modified, err := json.Marshal(payload)
		if err != nil {
			return body
		}
		log.Printf("[ThinkingProxy] Codebuff auto-rewrite (default anthropic): %s → %s", model, payload["model"])
		return modified
	}
	log.Printf("[ThinkingProxy] Codebuff auto-rewrite: no match found for model %q, passing through", model)
	return body
}

func normalizeCodebuffModelAlias(model string) string {
	switch model {
	case "claude-opus-4.6":
		return "claude-opus-4-6"
	case "anthropic/claude-opus-4.6":
		return "anthropic/claude-opus-4-6"
	case "claude-sonnet-4.6":
		return "claude-sonnet-4-6"
	case "anthropic/claude-sonnet-4.6":
		return "anthropic/claude-sonnet-4-6"
	default:
		return model
	}
}

// createCodebuffAgentRun creates a new agent run on the Codebuff API and returns the runId.
func (tp *ThinkingProxy) createCodebuffAgentRun() (string, error) {
	reqBody, err := json.Marshal(map[string]string{"action": "START", "agentId": "base"})
	if err != nil {
		return "", fmt.Errorf("marshal agent-run request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://www.codebuff.com/api/v1/agent-runs", bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("create agent-run request: %w", err)
	}
	req.Header.Set("x-codebuff-api-key", tp.CodebuffConfig.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := tp.codebuffClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("agent-run request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("agent-run returned status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		RunID string `json:"runId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode agent-run response: %w", err)
	}
	if result.RunID == "" {
		return "", fmt.Errorf("empty runId in agent-run response")
	}
	return result.RunID, nil
}

// forwardToCodebuff forwards requests to Codebuff's OpenAI-compatible API.
// It transparently creates an agent run and injects codebuff_metadata when needed.
func (tp *ThinkingProxy) forwardToCodebuff(w http.ResponseWriter, r *http.Request, body []byte) {
	// Ensure codebuff_metadata.run_id is present in the request body.
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("[ThinkingProxy] Error parsing Codebuff request body: %v", err)
		http.Error(w, "Bad Request - Invalid JSON body", http.StatusBadRequest)
		return
	}

	needsRunID := true
	if meta, ok := payload["codebuff_metadata"].(map[string]interface{}); ok {
		if rid, ok := meta["run_id"].(string); ok && rid != "" {
			needsRunID = false
		}
	}

	if needsRunID {
		runID, err := tp.createCodebuffAgentRun()
		if err != nil {
			log.Printf("[ThinkingProxy] Failed to create Codebuff agent run: %v", err)
			http.Error(w, "Bad Gateway - Failed to create Codebuff agent run", http.StatusBadGateway)
			return
		}
		log.Printf("[ThinkingProxy] Created Codebuff agent run: %s", runID)
		payload["codebuff_metadata"] = map[string]interface{}{
			"run_id":    runID,
			"cost_mode": "normal",
		}
		body, err = json.Marshal(payload)
		if err != nil {
			log.Printf("[ThinkingProxy] Error re-marshaling Codebuff request body: %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
	}

	// Map /v1/... → /api/v1/... for Codebuff's endpoint structure.
	codebuffPath := r.URL.Path
	if !strings.HasPrefix(codebuffPath, "/api/") {
		codebuffPath = "/api" + codebuffPath
	}
	targetURL := "https://www.codebuff.com" + codebuffPath
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("[ThinkingProxy] Error creating Codebuff request: %v", err)
		http.Error(w, "Bad Gateway - Could not connect to Codebuff", http.StatusBadGateway)
		return
	}

	excludedHeaders := map[string]bool{
		"host":               true,
		"content-length":     true,
		"connection":         true,
		"transfer-encoding":  true,
		"authorization":      true,
		"x-api-key":          true,
		"x-codebuff-api-key": true,
	}
	for name, values := range r.Header {
		if excludedHeaders[strings.ToLower(name)] {
			continue
		}
		for _, v := range values {
			proxyReq.Header.Add(name, v)
		}
	}

	proxyReq.Header.Set("x-codebuff-api-key", tp.CodebuffConfig.Token)
	proxyReq.Header.Set("Content-Type", "application/json")
	proxyReq.Header.Set("Host", "www.codebuff.com")
	proxyReq.Header.Set("Connection", "close")
	proxyReq.ContentLength = int64(len(body))

	resp, err := tp.codebuffClient.Do(proxyReq)
	if err != nil {
		log.Printf("[ThinkingProxy] Codebuff connection failed: %v", err)
		http.Error(w, "Bad Gateway - Could not connect to Codebuff", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for name, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	tp.streamResponse(w, resp.Body)
}

// codebuffModelEntries returns Codebuff models as json.RawMessage entries ready to append.
// During claudevibe sessions, it exposes clean Claude model IDs so Claude Code sees
// the same ID it sends while the proxy rewrites requests back to Codebuff routes.
func codebuffModelEntries(isCodebuffSession bool) []json.RawMessage {
	now := time.Now().Unix()
	entries := make([]json.RawMessage, 0, len(codebuffModels))
	for _, m := range codebuffModels {
		id := "codebuff/" + m
		if isCodebuffSession {
			if !strings.HasPrefix(m, "anthropic/claude-") {
				continue
			}
			id = cleanCodebuffModelID(m)
		}
		entry, _ := json.Marshal(map[string]interface{}{
			"id":       id,
			"object":   "model",
			"created":  now,
			"owned_by": "codebuff",
		})
		entries = append(entries, entry)
	}
	return entries
}

func cleanCodebuffModelID(model string) string {
	if idx := strings.LastIndex(model, "/"); idx >= 0 {
		return model[idx+1:]
	}
	return model
}

func appendUniqueModelEntries(existing, additional []json.RawMessage) []json.RawMessage {
	seen := make(map[string]struct{}, len(existing)+len(additional))
	for _, entry := range existing {
		if id := extractModelID(entry); id != "" {
			seen[id] = struct{}{}
		}
	}

	merged := append([]json.RawMessage{}, existing...)
	for _, entry := range additional {
		id := extractModelID(entry)
		if id != "" {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
		}
		merged = append(merged, entry)
	}

	return merged
}

func extractModelID(entry json.RawMessage) string {
	var model struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(entry, &model); err != nil {
		return ""
	}
	return model.ID
}

// writeCodebuffOnlyModels writes a /v1/models response containing only Codebuff models.
// Used as a graceful fallback when the backend is unreachable.
func writeCodebuffOnlyModels(w http.ResponseWriter, isCodebuffSession bool) {
	type modelsResponse struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}
	resp := modelsResponse{Object: "list", Data: codebuffModelEntries(isCodebuffSession)}
	data, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	w.Write(data)
	log.Printf("[ThinkingProxy] Returned %d Codebuff-only models (backend unavailable)", len(resp.Data))
}

// handleModelsWithCodebuff intercepts GET /v1/models, forwards to the backend,
// and appends Codebuff models (with "codebuff/" prefix) to the response.
func (tp *ThinkingProxy) handleModelsWithCodebuff(w http.ResponseWriter, r *http.Request, isCodebuffSession bool) {
	backendURL := fmt.Sprintf("http://127.0.0.1:%d/v1/models", tp.BackendPort)

	backendReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, backendURL, nil)
	if err != nil {
		log.Printf("[ThinkingProxy] Error creating backend models request: %v", err)
		writeCodebuffOnlyModels(w, isCodebuffSession)
		return
	}
	for name, values := range r.Header {
		for _, v := range values {
			backendReq.Header.Add(name, v)
		}
	}
	backendReq.Header.Set("Host", backendReq.URL.Host)

	resp, err := tp.backendTransport.RoundTrip(backendReq)
	if err != nil {
		log.Printf("[ThinkingProxy] Backend models request failed, returning Codebuff-only: %v", err)
		writeCodebuffOnlyModels(w, isCodebuffSession)
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[ThinkingProxy] Error reading backend models response: %v", err)
		writeCodebuffOnlyModels(w, isCodebuffSession)
		return
	}

	// Parse using json.RawMessage to preserve all original fields in backend model entries.
	type modelsResponse struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}

	var parsed modelsResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil || parsed.Object == "" {
		log.Printf("[ThinkingProxy] Could not parse backend models response, returning as-is")
		for name, values := range resp.Header {
			for _, v := range values {
				w.Header().Add(name, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		return
	}

	parsed.Data = appendUniqueModelEntries(parsed.Data, codebuffModelEntries(isCodebuffSession))

	merged, err := json.Marshal(parsed)
	if err != nil {
		log.Printf("[ThinkingProxy] Error marshaling merged models response: %v", err)
		for name, values := range resp.Header {
			for _, v := range values {
				w.Header().Add(name, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		return
	}

	for name, values := range resp.Header {
		if strings.ToLower(name) == "content-length" {
			continue
		}
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(merged)))
	w.WriteHeader(resp.StatusCode)
	w.Write(merged)

	log.Printf("[ThinkingProxy] Injected %d Codebuff models into /v1/models response", len(parsed.Data))
}

// streamResponse copies data from reader to the ResponseWriter, flushing after each chunk.
func (tp *ThinkingProxy) streamResponse(w http.ResponseWriter, reader io.Reader) {
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 32*1024) // 32KB buffer
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			_, writeErr := w.Write(buf[:n])
			if writeErr != nil {
				log.Printf("[ThinkingProxy] Write error during streaming: %v", writeErr)
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("[ThinkingProxy] Read error during streaming: %v", err)
			}
			return
		}
	}
}
