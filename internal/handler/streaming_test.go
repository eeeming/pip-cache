package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eeeming/pip-cache/internal/core"
	"github.com/eeeming/pip-cache/internal/config"
	"github.com/eeeming/pip-cache/internal/proxy"
	"github.com/sirupsen/logrus"
)

// TestStreamingWhileCaching 测试一边从上游读取一边发送给客户端，同时写入缓存
func TestStreamingWhileCaching(t *testing.T) {
	// 创建测试上游服务器，慢速发送数据
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "1048576") // 1MB
		w.WriteHeader(http.StatusOK)

		// 慢速分块写入数据
		data := make([]byte, 8192) // 8KB chunks
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 128; i++ { // 128 * 8KB = 1MB
			w.Write(data)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(5 * time.Millisecond) // 模拟网络延迟
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
		CacheMaxSize: 1024 * 1024 * 100, // 100MB，足够缓存1MB文件
		SimpleTTL:    10 * time.Minute,
		PackagesTTL:  24 * time.Hour,
		DefaultTTL:   1 * time.Hour,
	}

	handler := NewHandler(cacheStorage, proxyClient, cfg, logger)

	// 创建请求
	req := httptest.NewRequest("GET", "/packages/test.whl", nil)
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
	if w.Body.Len() != 1048576 {
		t.Errorf("Expected body size 1048576, got %d", w.Body.Len())
	}

	// 验证Content-Length被保留（用于显示进度条）
	if w.Header().Get("Content-Length") != "1048576" {
		t.Errorf("Expected Content-Length 1048576, got %s", w.Header().Get("Content-Length"))
	}

	// 验证缓存成功
	cacheKey := core.GenerateKey("GET", "/packages/test.whl")
	entry, err := cacheStorage.Get(cacheKey)
	if err != nil {
		t.Fatalf("Failed to get from cache: %v", err)
	}
	if entry == nil {
		t.Error("Entry should be cached")
	}
	if len(entry.Body) != 1048576 {
		t.Errorf("Cached body size should be 1048576, got %d", len(entry.Body))
	}

	t.Logf("Streamed and cached %d bytes in %v", w.Body.Len(), duration)
}

// TestStreamingWithContentLength 测试保留Content-Length的流式传输
func TestStreamingWithContentLength(t *testing.T) {
	// 创建测试上游服务器
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "524288") // 512KB
		w.WriteHeader(http.StatusOK)
		w.Write(make([]byte, 524288))
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
	req := httptest.NewRequest("GET", "/packages/test.whl", nil)
	w := httptest.NewRecorder()

	// 处理请求
	handler.ServeHTTP(w, req)

	// 验证Content-Length被保留
	if w.Header().Get("Content-Length") != "524288" {
		t.Errorf("Expected Content-Length 524288, got %s", w.Header().Get("Content-Length"))
	}

	// 验证数据完整性
	if w.Body.Len() != 524288 {
		t.Errorf("Expected body size 524288, got %d", w.Body.Len())
	}
}

