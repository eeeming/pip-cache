package proxy

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// ProxyClient handles proxying requests to upstream servers
type ProxyClient struct {
	primary          *Upstream
	fallback         *Upstream
	logger           *logrus.Logger
	LargeFileTimeout time.Duration // Timeout for large file downloads
}

// ProxyResponse represents the response from an upstream server
type ProxyResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
	Upstream   string // Name of the upstream that served the response
}

// NewProxyClient creates a new proxy client
func NewProxyClient(primary, fallback *Upstream, logger *logrus.Logger) *ProxyClient {
	return &ProxyClient{
		primary:          primary,
		fallback:         fallback,
		logger:           logger,
		LargeFileTimeout: 10 * time.Minute, // Default 10 minutes for large files
	}
}

// GetContentLength extracts Content-Length from headers
func GetContentLength(headers http.Header) (int64, bool) {
	cl := headers.Get("Content-Length")
	if cl == "" {
		return 0, false
	}
	size, err := strconv.ParseInt(cl, 10, 64)
	if err != nil {
		return 0, false
	}
	return size, true
}

// Fetch fetches content from upstream with fallback support
func (p *ProxyClient) Fetch(method, path string, headers http.Header) (*ProxyResponse, error) {
	fetchStart := time.Now()

	// Try primary upstream first
	resp, err := p.fetchFromUpstream(p.primary, method, path, headers)
	if err == nil && !IsRetriableError(resp.StatusCode) {
		fetchDuration := time.Since(fetchStart)
		p.logger.WithFields(logrus.Fields{
			"method":         method,
			"path":           path,
			"upstream":       resp.Upstream,
			"fetch_duration": fetchDuration,
		}).Debugf("Fetched from primary upstream")
		return resp, nil
	}

	// Log primary failure
	if err != nil {
		p.logger.WithFields(logrus.Fields{
			"method": method,
			"path":   path,
			"error":  err.Error(),
		}).Warnf("Primary upstream failed")
	} else {
		p.logger.WithFields(logrus.Fields{
			"method":      method,
			"path":        path,
			"status_code": resp.StatusCode,
		}).Warnf("Primary upstream returned retriable error")
	}

	// Try fallback upstream
	p.logger.WithFields(logrus.Fields{
		"method": method,
		"path":   path,
	}).Infof("Trying fallback upstream")
	resp, err = p.fetchFromUpstream(p.fallback, method, path, headers)
	if err != nil {
		fetchDuration := time.Since(fetchStart)
		p.logger.WithFields(logrus.Fields{
			"method":         method,
			"path":           path,
			"fetch_duration": fetchDuration,
			"error":          err.Error(),
		}).Errorf("Fallback upstream also failed")
		return nil, fmt.Errorf("fallback upstream also failed: %w", err)
	}

	fetchDuration := time.Since(fetchStart)
	p.logger.WithFields(logrus.Fields{
		"method":         method,
		"path":           path,
		"upstream":       resp.Upstream,
		"fetch_duration": fetchDuration,
	}).Debugf("Fetched from fallback upstream")
	return resp, nil
}

// fetchFromUpstream fetches content from a specific upstream
func (p *ProxyClient) fetchFromUpstream(upstream *Upstream, method, path string, headers http.Header) (*ProxyResponse, error) {
	url := upstream.BuildURL(path)
	requestStart := time.Now()

	p.logger.WithFields(logrus.Fields{
		"upstream": upstream.Name,
		"method":   method,
		"url":      url,
	}).Debugf("Fetching from upstream")

	// Create request
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Copy headers (except Host which should be set automatically)
	for key, values := range headers {
		if strings.ToLower(key) == "host" {
			continue
		}
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	// Set appropriate Host header
	// The Host header should match the upstream server
	// It will be set automatically by the HTTP client

	// Perform request
	// Log connection pool status before request
	transport, ok := upstream.Client.Transport.(*http.Transport)
	if ok {
		p.logger.WithFields(logrus.Fields{
			"upstream": upstream.Name,
			"method":   method,
			"path":     path,
		}).Infof("🔌 Attempting upstream request (MaxConnsPerHost: %d, MaxIdleConnsPerHost: %d)",
			transport.MaxConnsPerHost, transport.MaxIdleConnsPerHost)
	}

	resp, err := upstream.Client.Do(req)
	requestDuration := time.Since(requestStart)

	if err != nil {
		p.logger.WithFields(logrus.Fields{
			"upstream":         upstream.Name,
			"method":           method,
			"path":             path,
			"request_duration": requestDuration,
			"error":            err.Error(),
		}).Warnf("❌ Upstream request failed")
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	p.logger.WithFields(logrus.Fields{
		"upstream":         upstream.Name,
		"method":           method,
		"path":             path,
		"status_code":      resp.StatusCode,
		"request_duration": requestDuration,
	}).Infof("✅ Upstream request succeeded")

	// Read response body
	// Some status codes should not have a body (304 Not Modified, 204 No Content, etc.)
	// We need to handle them specially to avoid blocking
	var body []byte
	statusCode := resp.StatusCode

	// Status codes that should not have a body according to HTTP spec
	noBodyStatusCodes := map[int]bool{
		http.StatusNoContent:    true, // 204
		http.StatusNotModified:  true, // 304
		http.StatusResetContent: true, // 205
	}

	if method == http.MethodHead || noBodyStatusCodes[statusCode] {
		// For HEAD requests or status codes that shouldn't have body:
		// discard any body data if present to ensure connection can be reused
		// Use a limited reader to prevent reading too much data
		if noBodyStatusCodes[statusCode] {
			p.logger.WithFields(logrus.Fields{
				"status_code": statusCode,
				"method":      method,
				"path":        path,
			}).Infof("⚠️ Status %d should not have body, discarding any body data", statusCode)
		}
		limitedReader := io.LimitReader(resp.Body, 1024) // Limit to 1KB for safety
		_, _ = io.Copy(io.Discard, limitedReader)
		body = []byte{} // Empty body
	} else {
		// For other methods, read the full body
		// This is a blocking operation, but necessary for non-streaming responses
		bodyReadStart := time.Now()
		body, err = io.ReadAll(resp.Body)
		bodyReadDuration := time.Since(bodyReadStart)
		if err != nil {
			p.logger.WithFields(logrus.Fields{
				"status_code":        statusCode,
				"method":             method,
				"path":               path,
				"body_read_duration": bodyReadDuration,
				"error":              err.Error(),
			}).Warnf("❌ Failed to read response body")
			return nil, fmt.Errorf("failed to read response body: %w", err)
		}
		p.logger.WithFields(logrus.Fields{
			"status_code":        statusCode,
			"method":             method,
			"path":               path,
			"body_size":          len(body),
			"body_read_duration": bodyReadDuration,
		}).Debugf("📦 Response body read: %d bytes in %v", len(body), bodyReadDuration)
	}

	// Process Location header for redirects
	if location := resp.Header.Get("Location"); location != "" {
		// If the location is an absolute URL pointing to the upstream,
		// we need to rewrite it to point to our proxy
		location = p.rewriteLocation(location, upstream.BaseURL)
		resp.Header.Set("Location", location)
	}

	requestDuration = time.Since(requestStart)
	p.logger.WithFields(logrus.Fields{
		"upstream":         upstream.Name,
		"method":           method,
		"path":             path,
		"status_code":      resp.StatusCode,
		"body_size":        len(body),
		"request_duration": requestDuration,
	}).Infof("✅ Upstream fetch completed")

	proxyResp := &ProxyResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Body:       body,
		Upstream:   upstream.Name,
	}

	return proxyResp, nil
}

// rewriteLocation rewrites redirect locations from upstream to proxy
func (p *ProxyClient) rewriteLocation(location, upstreamBase string) string {
	// If location starts with the upstream base URL, strip it
	if strings.HasPrefix(location, upstreamBase) {
		// Return just the path part
		location = strings.TrimPrefix(location, upstreamBase)
		if !strings.HasPrefix(location, "/") {
			location = "/" + location
		}
	}
	// For aliyun, also strip the /pypi prefix
	if strings.HasPrefix(location, "/pypi/") {
		location = strings.TrimPrefix(location, "/pypi")
	}
	return location
}

// doStreamingRequest 执行流式请求到指定的upstream
func (p *ProxyClient) doStreamingRequest(upstream *Upstream, method, path string, headers http.Header) (*http.Response, *Upstream, error) {
	url := upstream.BuildURL(path)
	requestStart := time.Now()

	// Create request
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		p.logger.WithFields(logrus.Fields{
			"upstream": upstream.Name,
			"url":      url,
			"error":    err.Error(),
		}).Errorf("Failed to create request")
		return nil, upstream, fmt.Errorf("failed to create request: %w", err)
	}

	// Copy headers (except Host)
	for key, values := range headers {
		if strings.ToLower(key) == "host" {
			continue
		}
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	// Use longer timeout for large files
	client := upstream.Client
	isLargeFile := IsLargeFile(path)
	if isLargeFile {
		client = &http.Client{
			Timeout:   p.LargeFileTimeout,
			Transport: upstream.Client.Transport,
		}
		p.logger.WithFields(logrus.Fields{
			"upstream": upstream.Name,
			"path":     path,
			"timeout":  p.LargeFileTimeout,
		}).Debugf("Using large file timeout")
	}

	resp, err := client.Do(req)
	requestDuration := time.Since(requestStart)

	if err != nil {
		p.logger.WithFields(logrus.Fields{
			"upstream":         upstream.Name,
			"path":             path,
			"request_duration": requestDuration,
			"error":            err.Error(),
		}).Warnf("Upstream request failed")
	} else {
		p.logger.WithFields(logrus.Fields{
			"upstream":         upstream.Name,
			"path":             path,
			"status_code":      resp.StatusCode,
			"request_duration": requestDuration,
			"is_large_file":    isLargeFile,
		}).Debugf("✅ Upstream request completed")
	}

	return resp, upstream, err
}

// processHeaders 处理响应headers，复制并重写Location
func (p *ProxyClient) processHeaders(respHeaders http.Header, upstreamBase string) http.Header {
	headers := make(http.Header)
	for key, values := range respHeaders {
		for _, value := range values {
			headers.Add(key, value)
		}
	}
	if location := respHeaders.Get("Location"); location != "" {
		headers.Set("Location", p.rewriteLocation(location, upstreamBase))
	}
	return headers
}

// GetStreamingResponse gets a streaming response from upstream
// Returns the response body reader, headers, status code, upstream name, and error
func (p *ProxyClient) GetStreamingResponse(method, path string, headers http.Header) (io.ReadCloser, http.Header, int, string, error) {
	getStreamStart := time.Now()

	// Try primary upstream first
	resp, upstream, err := p.doStreamingRequest(p.primary, method, path, headers)

	// 检查是否需要尝试 fallback
	shouldTryFallback := err != nil || (resp != nil && IsRetriableError(resp.StatusCode))

	if shouldTryFallback {
		// 记录主服务器失败原因
		if err != nil {
			p.logger.WithFields(logrus.Fields{
				"method": method,
				"path":   path,
				"error":  err.Error(),
			}).Warnf("Primary upstream failed")
		} else {
			p.logger.WithFields(logrus.Fields{
				"method":      method,
				"path":        path,
				"status_code": resp.StatusCode,
			}).Warnf("Primary upstream returned retriable error")
			resp.Body.Close()
		}

		p.logger.WithFields(logrus.Fields{
			"method": method,
			"path":   path,
		}).Infof("Trying fallback upstream")
		resp, upstream, err = p.doStreamingRequest(p.fallback, method, path, headers)
		if err != nil {
			getStreamDuration := time.Since(getStreamStart)
			p.logger.WithFields(logrus.Fields{
				"method":              method,
				"path":                path,
				"get_stream_duration": getStreamDuration,
				"error":               err.Error(),
			}).Errorf("Fallback upstream also failed")
			return nil, nil, 0, "", fmt.Errorf("fallback upstream also failed: %w", err)
		}
	}

	// Process headers
	processedHeaders := p.processHeaders(resp.Header, upstream.BaseURL)
	getStreamDuration := time.Since(getStreamStart)
	p.logger.WithFields(logrus.Fields{
		"method":              method,
		"path":                path,
		"upstream":            upstream.Name,
		"status_code":         resp.StatusCode,
		"get_stream_duration": getStreamDuration,
	}).Debugf("✅ GetStreamingResponse completed")

	return resp.Body, processedHeaders, resp.StatusCode, upstream.Name, nil
}

// IsLargeFile checks if the path represents a large file (for timeout purposes)
// Note: This is only for timeout configuration, not for caching decisions
func IsLargeFile(path string) bool {
	return strings.HasSuffix(path, ".whl") ||
		strings.HasSuffix(path, ".tar.gz") ||
		strings.HasSuffix(path, ".zip")
}
