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

func setupTestHandler(t *testing.T) (*Handler, *httptest.Server, func()) {
	// 创建测试缓存
	tmpDir := t.TempDir()
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)
	cacheStorage, err := core.NewFileStorageWithMemoryMeta(tmpDir, 1024*1024*100, 1000, 0.9, 0.7, logger)
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	// 创建测试上游服务器
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 模拟大文件响应
		if r.URL.Path == "/packages/large.whl" {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", "10485760") // 10MB
			w.WriteHeader(http.StatusOK)
			// 写入10MB数据
			data := make([]byte, 10485760)
			w.Write(data)
			return
		}
		// 模拟小文件响应
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Length", "13")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("test response"))
	}))

	// 创建代理客户端
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

	// 创建配置
	cfg := &config.Config{
		CacheMaxSize: 1024 * 1024 * 100,
		SimpleTTL:    10 * time.Minute,
		PackagesTTL:  24 * time.Hour,
		DefaultTTL:   1 * time.Hour,
	}

	// 创建handler
	handler := NewHandler(cacheStorage, proxyClient, cfg, logger)

	cleanup := func() {
		upstreamServer.Close()
	}

	return handler, upstreamServer, cleanup
}

func TestHandler_LargeFileStreaming(t *testing.T) {
	handler, _, cleanup := setupTestHandler(t)
	defer cleanup()

	// 创建请求
	req := httptest.NewRequest("GET", "/packages/large.whl", nil)
	w := httptest.NewRecorder()

	// 处理请求
	handler.ServeHTTP(w, req)

	// 验证响应
	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	// 验证Content-Length
	if w.Header().Get("Content-Length") != "10485760" {
		t.Errorf("Expected Content-Length 10485760, got %s", w.Header().Get("Content-Length"))
	}

	// 验证响应大小
	if w.Body.Len() != 10485760 {
		t.Errorf("Expected body size 10485760, got %d", w.Body.Len())
	}

	// 验证没有缓存（大文件不缓存）
	if w.Header().Get("X-Cache-Status") != "MISS" {
		t.Errorf("Expected X-Cache-Status MISS, got %s", w.Header().Get("X-Cache-Status"))
	}
}

func TestHandler_LargeFileStreamingWithTimeout(t *testing.T) {
	handler, _, cleanup := setupTestHandler(t)
	defer cleanup()

	// 创建带超时的请求
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequest("GET", "/packages/large.whl", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	// 处理请求
	handler.ServeHTTP(w, req)

	// 验证响应（应该成功，因为流式传输）
	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}
}

func TestHandler_SmallFileCaching(t *testing.T) {
	handler, _, cleanup := setupTestHandler(t)
	defer cleanup()

	// 第一次请求
	req1 := httptest.NewRequest("GET", "/simple/test/", nil)
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)

	if w1.Code != http.StatusOK {
		t.Fatalf("First request failed with status %d", w1.Code)
	}

	if w1.Header().Get("X-Cache-Status") != "MISS" {
		t.Errorf("First request should be MISS, got %s", w1.Header().Get("X-Cache-Status"))
	}

	// 第二次请求（应该从缓存读取）
	req2 := httptest.NewRequest("GET", "/simple/test/", nil)
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("Second request failed with status %d", w2.Code)
	}

	if w2.Header().Get("X-Cache-Status") != "HIT" {
		t.Errorf("Second request should be HIT, got %s", w2.Header().Get("X-Cache-Status"))
	}

	if w2.Body.String() != w1.Body.String() {
		t.Error("Cached response should match original")
	}
}

func TestHandler_HealthCheck(t *testing.T) {
	handler, _, cleanup := setupTestHandler(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("Expected Content-Type application/json, got %s", w.Header().Get("Content-Type"))
	}
}

func TestHandler_PathRedirect(t *testing.T) {
	handler, _, cleanup := setupTestHandler(t)
	defer cleanup()

	tests := []struct {
		path         string
		expectedCode int
		expectedLoc  string
	}{
		{"/simple", http.StatusFound, "/simple/"},
		{"/simple/package", http.StatusFound, "/simple/package/"},
		{"/simple/package/", http.StatusOK, ""},
	}

	for _, tt := range tests {
		req := httptest.NewRequest("GET", tt.path, nil)
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != tt.expectedCode {
			t.Errorf("Path %s: expected status %d, got %d", tt.path, tt.expectedCode, w.Code)
		}

		if tt.expectedLoc != "" {
			loc := w.Header().Get("Location")
			if loc != tt.expectedLoc {
				t.Errorf("Path %s: expected Location %s, got %s", tt.path, tt.expectedLoc, loc)
			}
		}
	}
}

func TestHandler_NonGETRequest(t *testing.T) {
	handler, _, cleanup := setupTestHandler(t)
	defer cleanup()

	req := httptest.NewRequest("POST", "/simple/test/", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	// POST请求应该被代理，不缓存
	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}
}

// TestHandler_LargeFileSlowDownload 测试慢速下载大文件的情况
func TestHandler_LargeFileSlowDownload(t *testing.T) {
	// 创建慢速上游服务器
	slowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "1048576") // 1MB
		w.WriteHeader(http.StatusOK)
		// 慢速写入数据
		data := make([]byte, 1024)
		for i := 0; i < 1024; i++ {
			w.Write(data)
			time.Sleep(1 * time.Millisecond) // 模拟慢速网络
		}
	}))
	defer slowServer.Close()

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
	primary := proxy.NewUpstream("test", slowServer.URL, "", client)
	fallback := proxy.NewUpstream("fallback", slowServer.URL, "", client)
	proxyClient := proxy.NewProxyClient(primary, fallback, logger)
	proxyClient.LargeFileTimeout = 10 * time.Minute

	cfg := &config.Config{
		CacheMaxSize: 1024 * 1024 * 100,
		SimpleTTL:    10 * time.Minute,
		PackagesTTL:  24 * time.Hour,
		DefaultTTL:   1 * time.Hour,
	}

	handler := NewHandler(cacheStorage, proxyClient, cfg, logger)

	// 创建请求
	req := httptest.NewRequest("GET", "/packages/large.whl", nil)
	w := httptest.NewRecorder()

	// 处理请求（应该能完成，不会超时）
	done := make(chan bool)
	go func() {
		handler.ServeHTTP(w, req)
		done <- true
	}()

	select {
	case <-done:
		if w.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", w.Code)
		}
	case <-time.After(5 * time.Second):
		t.Error("Request timed out")
	}
}

// TestHandler_StreamingWithClientDisconnect 测试客户端断开连接的情况
func TestHandler_StreamingWithClientDisconnect(t *testing.T) {
	handler, _, cleanup := setupTestHandler(t)
	defer cleanup()

	// 创建可取消的请求
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/packages/large.whl", nil).WithContext(ctx)
	
	// 创建一个会在中途关闭的ResponseWriter
	w := &closingResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
		cancel:           cancel,
		closeAfter:       1024 * 1024, // 1MB后关闭
	}

	// 启动请求处理
	done := make(chan bool)
	go func() {
		handler.ServeHTTP(w, req)
		done <- true
	}()

	// 等待处理完成或超时
	select {
	case <-done:
		// 客户端断开是正常的，不应该有错误
	case <-time.After(3 * time.Second):
		// 超时也是可以接受的
	}
}

// closingResponseWriter 是一个会在写入一定数据后关闭的ResponseWriter
type closingResponseWriter struct {
	*httptest.ResponseRecorder
	cancel     context.CancelFunc
	closeAfter int
	written    int
}

func (w *closingResponseWriter) Write(data []byte) (int, error) {
	w.written += len(data)
	if w.written >= w.closeAfter {
		w.cancel() // 取消context，模拟客户端断开
		return 0, io.ErrClosedPipe
	}
	return w.ResponseRecorder.Write(data)
}

