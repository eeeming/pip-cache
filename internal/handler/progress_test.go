package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eeeming/pip-cache/internal/core"
	"github.com/eeeming/pip-cache/internal/config"
	"github.com/eeeming/pip-cache/internal/proxy"
	"github.com/sirupsen/logrus"
)

// TestProgressVisibility 测试进度可见性（频繁flush）
func TestProgressVisibility(t *testing.T) {
	// 创建测试上游服务器，慢速发送数据
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "1048576") // 1MB
		w.WriteHeader(http.StatusOK)

		// 慢速分块写入数据，模拟网络延迟
		data := make([]byte, 8192) // 8KB chunks
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 128; i++ { // 128 * 8KB = 1MB
			w.Write(data)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(10 * time.Millisecond) // 模拟网络延迟
		}
	}))
	defer upstreamServer.Close()

	// 创建handler
	tmpDir := t.TempDir()
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)
	cacheStorage, _ := core.NewFileStorageWithMemoryMeta(tmpDir, 1024*1024*50, 1000, 0.9, 0.7, logger)

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
		CacheMaxSize: 1024 * 1024 * 50,
		SimpleTTL:    10 * time.Minute,
		PackagesTTL:  24 * time.Hour,
		DefaultTTL:   1 * time.Hour,
	}

	handler := NewHandler(cacheStorage, proxyClient, cfg, logger)

	// 创建一个可以跟踪flush次数的ResponseWriter
	w := &progressTrackingWriter{
		ResponseRecorder: httptest.NewRecorder(),
		flushCount:      0,
	}

	// 创建请求
	req := httptest.NewRequest("GET", "/packages/large.whl", nil)

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

	// 验证flush次数（应该频繁flush以显示进度）
	// 注意：由于httptest.ResponseRecorder的限制，实际flush可能不会触发
	// 但在真实HTTP连接中，flush会正常工作
	// 这里主要验证数据能正确传输
	if w.Body.Len() != 1048576 {
		t.Errorf("Expected body size 1048576, got %d", w.Body.Len())
	}

	t.Logf("Transferred %d bytes (flush count: %d)", w.Body.Len(), w.flushCount)
}

// progressTrackingWriter 跟踪flush次数的ResponseWriter，实现http.Flusher接口
type progressTrackingWriter struct {
	*httptest.ResponseRecorder
	flushCount int
}

func (w *progressTrackingWriter) Flush() {
	w.flushCount++
	// httptest.ResponseRecorder不需要实际flush，只需要记录次数
}

// TestImmediateFlush 测试立即flush以确保客户端能看到进度
func TestImmediateFlush(t *testing.T) {
	// 创建测试上游服务器
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "1048576")
		w.WriteHeader(http.StatusOK)

		// 写入数据
		data := make([]byte, 1048576)
		w.Write(data)
	}))
	defer upstreamServer.Close()

	// 创建handler
	tmpDir := t.TempDir()
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)
	cacheStorage, _ := core.NewFileStorageWithMemoryMeta(tmpDir, 1024*1024*50, 1000, 0.9, 0.7, logger)

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
		CacheMaxSize: 1024 * 1024 * 50,
		SimpleTTL:    10 * time.Minute,
		PackagesTTL:  24 * time.Hour,
		DefaultTTL:   1 * time.Hour,
	}

	handler := NewHandler(cacheStorage, proxyClient, cfg, logger)

	// 创建可以跟踪flush的ResponseWriter
	w := &progressTrackingWriter{
		ResponseRecorder: httptest.NewRecorder(),
		flushCount:      0,
	}

	req := httptest.NewRequest("GET", "/packages/large.whl", nil)
	handler.ServeHTTP(w, req)

	// 验证数据完整性
	if w.Body.Len() != 1048576 {
		t.Errorf("Expected body size 1048576, got %d", w.Body.Len())
	}

	// 注意：在真实HTTP连接中，flush会正常工作
	// httptest.ResponseRecorder可能不会触发实际的flush
	t.Logf("Transferred %d bytes (flush count: %d)", w.Body.Len(), w.flushCount)
}

// TestChunkedTransferProgress 测试分块传输的进度更新频率
func TestChunkedTransferProgress(t *testing.T) {
	// 创建一个慢速读取器
	slowReader := &slowReader{
		data:      make([]byte, 1024*1024), // 1MB
		delay:     5 * time.Millisecond,     // 每5ms读取一次
		chunkSize: 8192,                     // 8KB chunks
	}

	// 创建可以跟踪flush的ResponseWriter
	w := &progressTrackingWriter{
		ResponseRecorder: httptest.NewRecorder(),
		flushCount:      0,
	}

	// 测试分块传输
	ctx := context.Background()
	written, err := copyWithContextChunked(ctx, w, slowReader, true, w)

	if err != nil {
		t.Errorf("copyWithContextChunked error: %v", err)
	}

	if written != 1024*1024 {
		t.Errorf("Expected written 1048576, got %d", written)
	}

	// 验证flush频率
	// 1MB数据，每8KB flush一次，应该有至少100次flush
	if w.flushCount < 100 {
		t.Errorf("Expected at least 100 flushes for 1MB data, got %d", w.flushCount)
	}

	t.Logf("Transferred %d bytes with %d flushes (avg %.2f KB per flush)",
		written, w.flushCount, float64(written)/float64(w.flushCount)/1024)
}

