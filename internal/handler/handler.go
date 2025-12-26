package handler

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/eeeming/pip-cache/internal/config"
	"github.com/eeeming/pip-cache/internal/core"
	"github.com/eeeming/pip-cache/internal/proxy"
	"github.com/sirupsen/logrus"
)

//go:embed help.html
var helpPageHTML string

// Handler handles HTTP requests
type Handler struct {
	cache  core.Cache
	proxy  *proxy.ProxyClient
	config *config.Config
	logger *logrus.Logger
}

// NewHandler creates a new HTTP handler
func NewHandler(c core.Cache, p *proxy.ProxyClient, cfg *config.Config, logger *logrus.Logger) *Handler {
	return &Handler{
		cache:  c,
		proxy:  p,
		config: cfg,
		logger: logger,
	}
}

// ServeHTTP implements http.Handler
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestStart := time.Now()

	// 处理健康检查
	if r.URL.Path == "/health" {
		h.handleHealth(w, r)
		return
	}

	// 处理帮助页面
	if r.URL.Path == "/" {
		h.handleHelp(w, r)
		return
	}

	// 处理路径重定向
	if h.handleRedirect(w, r) {
		return
	}

	// 只缓存GET请求
	if r.Method != http.MethodGet {
		h.handleProxy(w, r)
		return
	}

	// 生成缓存key
	cacheKey := core.GenerateKey(r.Method, r.URL.Path)

	// 尝试从缓存获取
	cacheGetStart := time.Now()
	entry, err := h.cache.Get(cacheKey)
	cacheGetDuration := time.Since(cacheGetStart)

	if err != nil {
		h.logger.WithFields(logrus.Fields{
			"cache_key":          cacheKey,
			"cache_get_duration": cacheGetDuration,
			"error":              err.Error(),
		}).Warnf("Cache get error")
	}

	// 检查缓存命中且未过期
	if entry != nil && !entry.IsExpired() {
		h.logger.WithFields(logrus.Fields{
			"cache_key":          cacheKey,
			"cache_get_duration": cacheGetDuration,
		}).Debugf("✅ Cache hit")
		h.serveFromCache(w, r, cacheKey)
		h.logger.WithFields(logrus.Fields{
			"cache_key":      cacheKey,
			"total_duration": time.Since(requestStart),
		}).Debugf("📤 Cache hit request completed")
		return
	}

	// 缓存未命中，从上游获取并缓存
	h.logger.WithFields(logrus.Fields{
		"cache_key":          cacheKey,
		"cache_get_duration": cacheGetDuration,
	}).Debugf("❌ Cache miss")
	h.handleProxyWithCache(w, r, cacheKey)

	h.logger.WithFields(logrus.Fields{
		"cache_key":      cacheKey,
		"total_duration": time.Since(requestStart),
	}).Debugf("📤 Cache miss request completed")
}

// handleHealth handles health check requests
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	cacheSize, _ := h.cache.Size()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"healthy","cache_size":%d}`, cacheSize)
}

// handleHelp handles the help page at root path
func (h *Handler) handleHelp(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, helpPageHTML)
}

// handleRedirect handles path redirects
func (h *Handler) handleRedirect(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path

	// /simple -> /simple/
	if path == "/simple" {
		http.Redirect(w, r, "/simple/", http.StatusFound)
		return true
	}

	// /simple/package -> /simple/package/
	if strings.HasPrefix(path, "/simple/") && !strings.HasSuffix(path, "/") {
		pathAfterSimple := strings.TrimPrefix(path, "/simple/")
		if pathAfterSimple != "" && !strings.Contains(pathAfterSimple, "/") {
			http.Redirect(w, r, path+"/", http.StatusFound)
			return true
		}
	}

	return false
}

// serveFromCache serves response from cache
func (h *Handler) serveFromCache(w http.ResponseWriter, r *http.Request, cacheKey string) {
	w.Header().Set("X-Cache-Status", "HIT")
	if err := h.cache.StreamFromCache(cacheKey, w); err != nil {
		h.logger.WithFields(logrus.Fields{
			"cache_key": cacheKey,
			"error":     err.Error(),
		}).Warnf("Failed to stream from cache")
		// 缓存读取失败，从上游获取
		h.handleProxyWithCache(w, r, cacheKey)
		return
	}
}

// handleProxyWithCache handles proxying with caching
func (h *Handler) handleProxyWithCache(w http.ResponseWriter, r *http.Request, cacheKey string) {
	// 从上游获取流式响应
	upstreamStart := time.Now()
	body, headers, statusCode, upstream, err := h.proxy.GetStreamingResponse(r.Method, r.URL.Path, r.Header)
	upstreamDuration := time.Since(upstreamStart)

	if err != nil {
		h.logger.WithFields(logrus.Fields{
			"path":              r.URL.Path,
			"upstream_duration": upstreamDuration,
			"error":             err.Error(),
		}).Errorf("Stream proxy error")
		http.Error(w, "Failed to fetch from upstream", http.StatusBadGateway)
		return
	}
	defer body.Close()

	h.logger.WithFields(logrus.Fields{
		"cache_key":         cacheKey,
		"upstream":          upstream,
		"status_code":       statusCode,
		"upstream_duration": upstreamDuration,
	}).Debugf("✅ Got streaming response from upstream")

	// 只缓存200状态码
	if statusCode != http.StatusOK {
		h.logger.WithFields(logrus.Fields{
			"cache_key":   cacheKey,
			"status_code": statusCode,
		}).Infof("⚠️ Not caching: status code %d is not cacheable (only 200 allowed)", statusCode)
		h.streamResponse(w, headers, statusCode, upstream, body, r.Context())
		return
	}

	// 检查Content-Length
	contentLength, hasLength := proxy.GetContentLength(headers)
	maxCacheSize := int64(float64(h.config.CacheMaxSize) * core.MaxSingleFileRatio)

	// 检查是否应该缓存
	shouldCache := !hasLength || contentLength <= maxCacheSize

	// 对于大文件或不缓存的文件，直接流式传输
	if !shouldCache {
		if !hasLength {
			h.logger.Debugf("Content-Length not available, streaming without cache for %s", cacheKey)
		} else {
			h.logger.Debugf("File too large to cache (%d > %d), streaming without cache for %s",
				contentLength, maxCacheSize, cacheKey)
		}
		// 对于已知大小的文件，保留Content-Length以显示进度条
		h.streamResponseWithContentLength(w, headers, statusCode, upstream, body, r.Context(), hasLength, contentLength)
		return
	}

	// 对于小文件，一边从上游读取一边发送给客户端，同时写入缓存
	// 确定TTL
	ttl := core.GetTTLForPath(r.URL.Path, h.config.SimpleTTL, h.config.PackagesTTL, h.config.DefaultTTL)

	// 使用StreamToCache进行流式传输（同时写入缓存和客户端）
	// StreamToCache会一边从上游读取一边发送给客户端，同时写入缓存
	// 注意：保留Content-Length以显示进度条
	err = h.cache.StreamToCache(cacheKey, body, w, statusCode, headers, ttl, upstream)
	if err != nil {
		h.logger.WithFields(logrus.Fields{
			"cache_key": cacheKey,
			"error":     err.Error(),
		}).Warnf("Failed to cache during streaming, but data was sent to client")
	}
}

// handleProxy handles proxying without caching
func (h *Handler) handleProxy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	fetchStart := time.Now()

	h.logger.WithFields(logrus.Fields{
		"method": r.Method,
		"path":   r.URL.Path,
	}).Infof("🔄 Starting proxy fetch")

	done := make(chan struct {
		resp *proxy.ProxyResponse
		err  error
	}, 1)

	go func() {
		resp, err := h.proxy.Fetch(r.Method, r.URL.Path, r.Header)
		done <- struct {
			resp *proxy.ProxyResponse
			err  error
		}{resp, err}
	}()

	select {
	case result := <-done:
		fetchDuration := time.Since(fetchStart)
		if result.err != nil {
			h.logger.WithFields(logrus.Fields{
				"path":           r.URL.Path,
				"fetch_duration": fetchDuration,
				"error":          result.err.Error(),
			}).Errorf("❌ Proxy fetch error after %v", fetchDuration)
			http.Error(w, "Failed to fetch from upstream", http.StatusBadGateway)
			return
		}

		h.logger.WithFields(logrus.Fields{
			"path":           r.URL.Path,
			"upstream":       result.resp.Upstream,
			"status_code":    result.resp.StatusCode,
			"body_size":      len(result.resp.Body),
			"fetch_duration": fetchDuration,
		}).Infof("✅ Proxy fetch completed in %v", fetchDuration)

		copyHeaders(w.Header(), result.resp.Headers)
		w.Header().Set("X-Upstream", result.resp.Upstream)
		w.WriteHeader(result.resp.StatusCode)
		w.Write(result.resp.Body)
	case <-ctx.Done():
		fetchDuration := time.Since(fetchStart)
		h.logger.WithFields(logrus.Fields{
			"path":           r.URL.Path,
			"fetch_duration": fetchDuration,
			"error":          ctx.Err().Error(),
		}).Warnf("⚠️ Proxy fetch cancelled after %v due to client disconnect", fetchDuration)
		return
	}
}

// streamResponse streams a response to the client using chunked transfer encoding
func (h *Handler) streamResponse(w http.ResponseWriter, headers http.Header, statusCode int, upstream string, body io.ReadCloser, ctx context.Context) {
	h.streamResponseWithContentLength(w, headers, statusCode, upstream, body, ctx, false, 0)
}

// streamResponseWithContentLength streams a response to the client
// If hasLength is true and contentLength > 0, preserves Content-Length header for progress display
func (h *Handler) streamResponseWithContentLength(w http.ResponseWriter, headers http.Header, statusCode int, upstream string, body io.ReadCloser, ctx context.Context, hasLength bool, contentLength int64) {
	// 复制headers
	// 对于已知大小的文件，保留Content-Length以显示进度条
	// 对于未知大小的文件，移除Content-Length以使用chunked encoding
	if hasLength && contentLength > 0 {
		// 保留Content-Length，使用流式传输但显示进度
		copyHeaders(w.Header(), headers)
	} else {
		// 移除Content-Length，使用chunked encoding
		copyHeadersWithoutContentLength(w.Header(), headers)
	}

	w.Header().Set("X-Cache-Status", "MISS")
	w.Header().Set("X-Upstream", upstream)
	w.WriteHeader(statusCode)

	// 使用Flusher确保数据及时发送
	flusher, hasFlusher := w.(http.Flusher)

	// 立即flush一次，让客户端知道连接已建立，可以开始显示进度
	if hasFlusher {
		flusher.Flush()
	}

	written, err := copyWithContextChunked(ctx, w, body, hasFlusher, flusher)
	if err != nil && err != context.Canceled {
		h.logger.WithFields(logrus.Fields{
			"written": written,
			"error":   err.Error(),
		}).Warnf("Error copying response body")
	} else if err == context.Canceled {
		h.logger.WithFields(logrus.Fields{
			"written": written,
		}).Debugf("Response copy cancelled (client disconnected)")
	}
}

// copyHeaders copies HTTP headers
func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

// copyHeadersWithoutContentLength copies HTTP headers but removes Content-Length
// This allows chunked transfer encoding to be used
func copyHeadersWithoutContentLength(dst, src http.Header) {
	for key, values := range src {
		// 跳过Content-Length，让Go自动使用chunked encoding
		if strings.ToLower(key) == "content-length" {
			continue
		}
		// 保留Transfer-Encoding头（如果上游已经设置了chunked）
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

// copyWithContextChunked copies from src to dst with chunked transfer encoding support
// It flushes data frequently to ensure timely delivery and progress visibility
func copyWithContextChunked(ctx context.Context, dst io.Writer, src io.Reader, hasFlusher bool, flusher http.Flusher) (int64, error) {
	done := make(chan error, 1)
	var written int64
	const chunkSize = 16 * 1024    // 16KB chunks (更小的chunk size以提高响应性)
	const flushInterval = 4 * 1024 // Flush every 4KB (非常频繁的flush以显示进度条)

	go func() {
		buf := make([]byte, chunkSize)
		lastFlushSize := int64(0)

		for {
			select {
			case <-ctx.Done():
				done <- ctx.Err()
				return
			default:
			}

			nr, er := src.Read(buf)
			if nr > 0 {
				nw, ew := dst.Write(buf[0:nr])
				if nw < 0 || nr < nw {
					nw = 0
					if ew == nil {
						ew = fmt.Errorf("invalid write result")
					}
				}
				written += int64(nw)

				// 频繁flush以确保数据及时发送，让客户端能看到进度
				// 对于大文件下载，客户端需要频繁的更新才能显示进度条
				// 每次写入后检查是否需要flush（每4KB flush一次）
				if hasFlusher && written-lastFlushSize >= flushInterval {
					flusher.Flush()
					lastFlushSize = written
				}

				if ew != nil {
					done <- ew
					return
				}
				if nr != nw {
					done <- io.ErrShortWrite
					return
				}
			}
			if er != nil {
				if er != io.EOF {
					done <- er
				} else {
					// 最后一次flush
					if hasFlusher {
						flusher.Flush()
					}
					done <- nil
				}
				return
			}
		}
	}()

	select {
	case err := <-done:
		return written, err
	case <-ctx.Done():
		return written, ctx.Err()
	}
}
