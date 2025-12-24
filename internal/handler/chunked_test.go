package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eeeming/pip-cache/internal/core"
	"github.com/eeeming/pip-cache/internal/config"
	"github.com/eeeming/pip-cache/internal/proxy"
	"github.com/sirupsen/logrus"
)

// TestChunkedTransferEncoding 测试分块传输编码
func TestChunkedTransferEncoding(t *testing.T) {
	// 创建测试上游服务器，返回大文件
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "1048576") // 1MB
		w.WriteHeader(http.StatusOK)

		// 分块写入数据
		data := make([]byte, 1024)
		for i := 0; i < 1024; i++ {
			w.Write(data)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}))
	defer upstreamServer.Close()

	// 创建handler
	tmpDir := t.TempDir()
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)
	cacheStorage, _ := core.NewFileStorageWithMemoryMeta(tmpDir, 1024*1024*100, 1000, 0.9, 0.7, logger)

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 5,
		},
	}
	primary := proxy.NewUpstream("test", upstreamServer.URL, "", client)
	fallback := proxy.NewUpstream("fallback", upstreamServer.URL, "", client)
	proxyClient := proxy.NewProxyClient(primary, fallback, logger)
	proxyClient.LargeFileTimeout = 10 * time.Minute

	cfg := &config.Config{
		CacheMaxSize: 1024 * 1024 * 50, // 50MB，小于文件大小，触发流式传输
		SimpleTTL:    10 * time.Minute,
		PackagesTTL:  24 * time.Hour,
		DefaultTTL:   1 * time.Hour,
	}

	handler := NewHandler(cacheStorage, proxyClient, cfg, logger)

	// 创建请求
	req := httptest.NewRequest("GET", "/packages/large.whl", nil)

	// 创建一个可以检查Transfer-Encoding的ResponseWriter
	w := &chunkedResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
		headers:          make(http.Header),
	}

	// 处理请求
	handler.ServeHTTP(w, req)

	// 验证响应
	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	// 验证数据完整性
	if w.Body.Len() != 1048576 {
		t.Errorf("Expected body size 1048576, got %d", w.Body.Len())
	}

	// 注意：httptest.ResponseRecorder在测试中可能会自动设置Content-Length
	// 但在实际HTTP响应中，copyHeadersWithoutContentLength会移除Content-Length
	// 让Go的http包自动使用chunked encoding
	// 这里主要验证数据能正确传输
	t.Logf("Successfully transferred %d bytes", w.Body.Len())
}

// TestChunkedTransferLargeFile 测试大文件的分块传输
func TestChunkedTransferLargeFile(t *testing.T) {
	// 创建测试上游服务器，返回10MB文件
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "10485760") // 10MB
		w.WriteHeader(http.StatusOK)

		// 分块写入数据
		data := make([]byte, 64*1024) // 64KB chunks
		for i := 0; i < 160; i++ {    // 160 * 64KB = 10MB
			w.Write(data)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}))
	defer upstreamServer.Close()

	// 创建handler
	tmpDir := t.TempDir()
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)
	cacheStorage, _ := core.NewFileStorageWithMemoryMeta(tmpDir, 1024*1024*100, 1000, 0.9, 0.7, logger)

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 5,
		},
	}
	primary := proxy.NewUpstream("test", upstreamServer.URL, "", client)
	fallback := proxy.NewUpstream("fallback", upstreamServer.URL, "", client)
	proxyClient := proxy.NewProxyClient(primary, fallback, logger)
	proxyClient.LargeFileTimeout = 10 * time.Minute

	cfg := &config.Config{
		CacheMaxSize: 1024 * 1024 * 50, // 50MB，小于文件大小
		SimpleTTL:    10 * time.Minute,
		PackagesTTL:  24 * time.Hour,
		DefaultTTL:   1 * time.Hour,
	}

	handler := NewHandler(cacheStorage, proxyClient, cfg, logger)

	// 创建请求
	req := httptest.NewRequest("GET", "/packages/very-large.whl", nil)
	w := httptest.NewRecorder()

	// 处理请求
	start := time.Now()
	handler.ServeHTTP(w, req)
	duration := time.Since(start)

	// 验证响应
	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	// 验证数据完整性
	if w.Body.Len() != 10485760 {
		t.Errorf("Expected body size 10485760, got %d", w.Body.Len())
	}

	// 验证响应时间合理（不应该太慢）
	if duration > 5*time.Second {
		t.Errorf("Response took too long: %v", duration)
	}

	t.Logf("Transferred 10MB in %v", duration)
}

// TestChunkedTransferWithoutContentLength 测试没有Content-Length的分块传输
func TestChunkedTransferWithoutContentLength(t *testing.T) {
	// 创建测试上游服务器，不设置Content-Length
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		// 不设置Content-Length
		w.WriteHeader(http.StatusOK)

		// 写入数据
		data := make([]byte, 1024)
		for i := 0; i < 100; i++ {
			w.Write(data)
		}
	}))
	defer upstreamServer.Close()

	// 创建handler
	tmpDir := t.TempDir()
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)
	cacheStorage, _ := core.NewFileStorageWithMemoryMeta(tmpDir, 1024*1024*100, 1000, 0.9, 0.7, logger)

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 5,
		},
	}
	primary := proxy.NewUpstream("test", upstreamServer.URL, "", client)
	fallback := proxy.NewUpstream("fallback", upstreamServer.URL, "", client)
	proxyClient := proxy.NewProxyClient(primary, fallback, logger)

	cfg := &config.Config{
		CacheMaxSize: 1024 * 1024 * 100,
		SimpleTTL:    10 * time.Minute,
		PackagesTTL:  24 * time.Hour,
		DefaultTTL:   1 * time.Hour,
	}

	handler := NewHandler(cacheStorage, proxyClient, cfg, logger)

	// 创建请求
	req := httptest.NewRequest("GET", "/simple/test/", nil)
	w := httptest.NewRecorder()

	// 处理请求
	handler.ServeHTTP(w, req)

	// 验证响应
	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	// 验证数据完整性
	if w.Body.Len() != 102400 {
		t.Errorf("Expected body size 102400, got %d", w.Body.Len())
	}
}

// chunkedResponseWriter 用于测试的ResponseWriter，可以检查headers
type chunkedResponseWriter struct {
	*httptest.ResponseRecorder
	headers http.Header
}

func (w *chunkedResponseWriter) Header() http.Header {
	return w.headers
}

func (w *chunkedResponseWriter) Write(data []byte) (int, error) {
	return w.ResponseRecorder.Write(data)
}

func (w *chunkedResponseWriter) WriteHeader(code int) {
	w.Code = code
}

// TestCopyHeadersWithoutContentLength 测试copyHeadersWithoutContentLength函数
func TestCopyHeadersWithoutContentLength(t *testing.T) {
	src := make(http.Header)
	src.Set("Content-Type", "application/octet-stream")
	src.Set("Content-Length", "1048576")
	src.Set("X-Custom-Header", "test")

	dst := make(http.Header)
	copyHeadersWithoutContentLength(dst, src)

	// 验证Content-Length被移除
	if dst.Get("Content-Length") != "" {
		t.Errorf("Content-Length should be removed, got %s", dst.Get("Content-Length"))
	}

	// 验证其他headers被保留
	if dst.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("Content-Type should be preserved, got %s", dst.Get("Content-Type"))
	}

	if dst.Get("X-Custom-Header") != "test" {
		t.Errorf("X-Custom-Header should be preserved, got %s", dst.Get("X-Custom-Header"))
	}
}

// TestChunkedTransferWithSlowReader 测试慢速读取器的分块传输
func TestChunkedTransferWithSlowReader(t *testing.T) {
	// 创建慢速读取器
	slowReader := &slowReader{
		data:      make([]byte, 1048576), // 1MB
		delay:     10 * time.Millisecond,
		chunkSize: 1024,
	}

	// 创建一个实现Flusher的ResponseWriter
	w := &flushingResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
	}

	// 测试分块传输
	ctx := context.Background()
	written, err := copyWithContextChunked(ctx, w, slowReader, true, w)

	if err != nil {
		t.Errorf("copyWithContextChunked error: %v", err)
	}

	if written != 1048576 {
		t.Errorf("Expected written 1048576, got %d", written)
	}
}

// flushingResponseWriter 实现http.Flusher接口的ResponseWriter
type flushingResponseWriter struct {
	*httptest.ResponseRecorder
}

func (w *flushingResponseWriter) Flush() {
	// httptest.ResponseRecorder不需要实际flush
}

// slowReader 是一个慢速读取器，用于测试
type slowReader struct {
	data      []byte
	pos       int
	delay     time.Duration
	chunkSize int
}

func (r *slowReader) Read(p []byte) (n int, err error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}

	// 每次读取一小块
	toRead := r.chunkSize
	if toRead > len(p) {
		toRead = len(p)
	}
	if r.pos+toRead > len(r.data) {
		toRead = len(r.data) - r.pos
	}

	copy(p, r.data[r.pos:r.pos+toRead])
	r.pos += toRead

	// 模拟慢速网络
	time.Sleep(r.delay)

	return toRead, nil
}
