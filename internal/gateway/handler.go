package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
)

type Config struct {
	UpstreamURL string
	MaxBodySize int64
	HTTPClient  *http.Client
	Logger      *log.Logger
}

type Handler struct {
	upstreamURL string
	maxBodySize int64
	client      *http.Client
	logger      *log.Logger
}

func New(config Config) (*Handler, error) {
	parsed, err := url.Parse(config.UpstreamURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid upstream URL %q", config.UpstreamURL)
	}
	if config.MaxBodySize <= 0 {
		config.MaxBodySize = 32 << 20
	}
	if config.HTTPClient == nil {
		config.HTTPClient = http.DefaultClient
	}
	if config.Logger == nil {
		config.Logger = log.Default()
	}
	return &Handler{
		upstreamURL: parsed.String(),
		maxBodySize: config.MaxBodySize,
		client:      config.HTTPClient,
		logger:      config.Logger,
	}, nil
}

func ChatCompletionsURL(base string) string {
	return strings.TrimRight(base, "/") + "/chat/completions"
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/healthz":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/responses":
		h.createResponse(w, r)
	default:
		writeAPIError(w, http.StatusNotFound, "route not found", "invalid_request_error", nil)
	}
}

func (h *Handler) createResponse(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "request body is too large", "invalid_request_error", nil)
			return
		}
		writeAPIError(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error", nil)
		return
	}

	chatRequest, meta, err := convertRequest(body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", nil)
		return
	}
	upstreamBody, err := json.Marshal(chatRequest)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "failed to encode upstream request", "server_error", nil)
		return
	}

	upstreamRequest, err := http.NewRequestWithContext(r.Context(), http.MethodPost, h.upstreamURL, bytes.NewReader(upstreamBody))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "failed to create upstream request", "server_error", nil)
		return
	}
	copyEndToEndHeaders(upstreamRequest.Header, r.Header)
	upstreamRequest.Header.Set("Content-Type", "application/json")
	if meta.stream {
		upstreamRequest.Header.Set("Accept", "text/event-stream")
	} else {
		upstreamRequest.Header.Set("Accept", "application/json")
	}

	upstreamResponse, err := h.client.Do(upstreamRequest)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		h.logger.Printf("upstream request failed: %v", err)
		writeAPIError(w, http.StatusBadGateway, "upstream request failed", "upstream_error", nil)
		return
	}
	defer upstreamResponse.Body.Close()
	copyEndToEndHeaders(w.Header(), upstreamResponse.Header)

	if upstreamResponse.StatusCode < 200 || upstreamResponse.StatusCode >= 300 {
		h.proxyError(w, upstreamResponse)
		return
	}
	if meta.stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(upstreamResponse.StatusCode)
		if err := convertStream(w, upstreamResponse.Body, meta); err != nil && r.Context().Err() == nil {
			h.logger.Printf("stream conversion failed: %v", err)
		}
		return
	}

	responseBody, err := io.ReadAll(upstreamResponse.Body)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "failed to read upstream response", "upstream_error", nil)
		return
	}
	response, err := convertResponse(responseBody, meta)
	if err != nil {
		h.logger.Printf("response conversion failed: %v", err)
		writeAPIError(w, http.StatusBadGateway, err.Error(), "upstream_error", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(upstreamResponse.StatusCode)
	_ = json.NewEncoder(w).Encode(response)
}

func (h *Handler) proxyError(w http.ResponseWriter, response *http.Response) {
	body, err := io.ReadAll(response.Body)
	if err == nil && json.Valid(body) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(body)
		return
	}
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = response.Status
	}
	writeAPIError(w, response.StatusCode, message, "upstream_error", nil)
}

func writeAPIError(w http.ResponseWriter, status int, message, errorType string, code any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errorType,
			"param":   nil,
			"code":    code,
		},
	})
}

var hopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
	"Content-Length":      {},
}

func copyEndToEndHeaders(dst, src http.Header) {
	blockedHeaders := make(map[string]struct{}, len(hopByHopHeaders)+4)
	for key := range hopByHopHeaders {
		blockedHeaders[key] = struct{}{}
	}
	for _, connection := range src.Values("Connection") {
		for _, token := range strings.Split(connection, ",") {
			if token = strings.TrimSpace(token); token != "" {
				blockedHeaders[http.CanonicalHeaderKey(token)] = struct{}{}
			}
		}
	}
	for key, values := range src {
		canonical := http.CanonicalHeaderKey(key)
		if _, blocked := blockedHeaders[canonical]; blocked {
			continue
		}
		dst.Del(canonical)
		for _, value := range values {
			dst.Add(canonical, value)
		}
	}
}
