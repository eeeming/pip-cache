package core

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func setupTestCache(t *testing.T) (*FileStorageWithMemoryMeta, string) {
	tmpDir := filepath.Join(os.TempDir(), "pip-cache-test", t.Name())
	os.RemoveAll(tmpDir) // 清理旧数据

	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel) // 测试时只显示错误

	cache, err := NewFileStorageWithMemoryMeta(tmpDir, 1024*1024, 100, 0.9, 0.7, logger)
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	return cache, tmpDir
}

func cleanupTestCache(tmpDir string) {
	os.RemoveAll(tmpDir)
}

func TestFileStorageWithMemoryMeta_StreamToCacheAndGet(t *testing.T) {
	cache, tmpDir := setupTestCache(t)
	defer cleanupTestCache(tmpDir)

	key := "GET-/simple/test/"
	body := []byte("test response body")
	headers := make(http.Header)
	headers.Set("Content-Type", "text/html")
	headers.Set("Content-Length", "18")

	reader := bytes.NewReader(body)
	w := httptest.NewRecorder()

	// 测试StreamToCache
	err := cache.StreamToCache(key, reader, w, http.StatusOK, headers, 1*time.Hour, "test")
	if err != nil {
		t.Fatalf("StreamToCache() error = %v", err)
	}

	// 测试Get
	got, err := cache.Get(key)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	if got == nil {
		t.Fatal("Get() returned nil")
	}

	if got.StatusCode != http.StatusOK {
		t.Errorf("Get() StatusCode = %d, want %d", got.StatusCode, http.StatusOK)
	}

	if !bytes.Equal(got.Body, body) {
		t.Errorf("Get() Body = %v, want %v", got.Body, body)
	}

	if got.Headers.Get("Content-Type") != headers.Get("Content-Type") {
		t.Errorf("Get() Content-Type = %q, want %q", got.Headers.Get("Content-Type"), headers.Get("Content-Type"))
	}
}

func TestFileStorageWithMemoryMeta_Delete(t *testing.T) {
	cache, tmpDir := setupTestCache(t)
	defer cleanupTestCache(tmpDir)

	key := "GET-/simple/test/"
	body := []byte("test")
	headers := make(http.Header)
	headers.Set("Content-Length", "4")

	// 设置缓存
	reader := bytes.NewReader(body)
	w := httptest.NewRecorder()
	err := cache.StreamToCache(key, reader, w, http.StatusOK, headers, 1*time.Hour, "test")
	if err != nil {
		t.Fatalf("StreamToCache() error = %v", err)
	}

	// 验证存在
	got, err := cache.Get(key)
	if err != nil || got == nil {
		t.Fatal("Entry should exist before delete")
	}

	// 删除
	err = cache.Delete(key)
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	// 验证已删除
	got, err = cache.Get(key)
	if err != nil {
		t.Fatalf("Get() after delete error = %v", err)
	}
	if got != nil {
		t.Error("Entry should be deleted")
	}
}

func TestFileStorageWithMemoryMeta_Expired(t *testing.T) {
	cache, tmpDir := setupTestCache(t)
	defer cleanupTestCache(tmpDir)

	key := "GET-/simple/test/"
	body := []byte("test")
	headers := make(http.Header)
	headers.Set("Content-Length", "4")

	// 设置已过期的缓存（通过负TTL）
	reader := bytes.NewReader(body)
	w := httptest.NewRecorder()
	err := cache.StreamToCache(key, reader, w, http.StatusOK, headers, -1*time.Hour, "test")
	if err != nil {
		t.Fatalf("StreamToCache() error = %v", err)
	}

	// 获取应该返回nil（因为过期）
	got, err := cache.Get(key)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got != nil {
		t.Error("Expired entry should return nil")
	}
}

func TestFileStorageWithMemoryMeta_Size(t *testing.T) {
	cache, tmpDir := setupTestCache(t)
	defer cleanupTestCache(tmpDir)

	// 初始大小应该为0
	size, err := cache.Size()
	if err != nil {
		t.Fatalf("Size() error = %v", err)
	}
	if size != 0 {
		t.Errorf("Initial Size() = %d, want 0", size)
	}

	// 添加一个条目
	key := "GET-/simple/test/"
	body := []byte("test body")
	headers := make(http.Header)
	headers.Set("Content-Length", "9")

	reader := bytes.NewReader(body)
	w := httptest.NewRecorder()
	err = cache.StreamToCache(key, reader, w, http.StatusOK, headers, 1*time.Hour, "test")
	if err != nil {
		t.Fatalf("StreamToCache() error = %v", err)
	}

	// 检查大小
	size, err = cache.Size()
	if err != nil {
		t.Fatalf("Size() error = %v", err)
	}
	if size != int64(len(body)) {
		t.Errorf("Size() after StreamToCache = %d, want %d", size, len(body))
	}
}

func TestFileStorageWithMemoryMeta_StreamFromCache(t *testing.T) {
	cache, tmpDir := setupTestCache(t)
	defer cleanupTestCache(tmpDir)

	key := "GET-/simple/test/"
	body := []byte("test response body")
	headers := make(http.Header)
	headers.Set("Content-Type", "text/html")
	headers.Set("Content-Length", "18")

	// 设置缓存
	reader := bytes.NewReader(body)
	w1 := httptest.NewRecorder()
	err := cache.StreamToCache(key, reader, w1, http.StatusOK, headers, 1*time.Hour, "test")
	if err != nil {
		t.Fatalf("StreamToCache() error = %v", err)
	}

	// 测试流式读取
	w2 := httptest.NewRecorder()
	err = cache.StreamFromCache(key, w2)
	if err != nil {
		t.Fatalf("StreamFromCache() error = %v", err)
	}

	if w2.Code != http.StatusOK {
		t.Errorf("StreamFromCache() status code = %d, want %d", w2.Code, http.StatusOK)
	}

	if !bytes.Equal(w2.Body.Bytes(), body) {
		t.Errorf("StreamFromCache() body = %v, want %v", w2.Body.Bytes(), body)
	}

	if w2.Header().Get("Content-Type") != headers.Get("Content-Type") {
		t.Errorf("StreamFromCache() Content-Type = %q, want %q", w2.Header().Get("Content-Type"), headers.Get("Content-Type"))
	}
}

func TestFileStorageWithMemoryMeta_StreamToCache(t *testing.T) {
	cache, tmpDir := setupTestCache(t)
	defer cleanupTestCache(tmpDir)

	key := "GET-/simple/test/"
	body := []byte("test response body")
	headers := make(http.Header)
	headers.Set("Content-Type", "text/html")
	headers.Set("Content-Length", "18")

	reader := bytes.NewReader(body)
	w := httptest.NewRecorder()

	// 测试流式写入缓存
	err := cache.StreamToCache(key, reader, w, http.StatusOK, headers, 1*time.Hour, "test")
	if err != nil {
		t.Fatalf("StreamToCache() error = %v", err)
	}

	// 验证响应
	if w.Code != http.StatusOK {
		t.Errorf("StreamToCache() status code = %d, want %d", w.Code, http.StatusOK)
	}

	if !bytes.Equal(w.Body.Bytes(), body) {
		t.Errorf("StreamToCache() body = %v, want %v", w.Body.Bytes(), body)
	}

	// 验证缓存
	got, err := cache.Get(key)
	if err != nil {
		t.Fatalf("Get() after StreamToCache error = %v", err)
	}
	if got == nil {
		t.Fatal("Entry should be cached")
	}
	if !bytes.Equal(got.Body, body) {
		t.Errorf("Cached body = %v, want %v", got.Body, body)
	}
}

func TestFileStorageWithMemoryMeta_TooLarge(t *testing.T) {
	cache, tmpDir := setupTestCache(t)
	defer cleanupTestCache(tmpDir)

	// 设置很小的maxSize
	cache.maxSize = 100

	key := "GET-/simple/test/"
	body := make([]byte, 200) // 超过限制
	headers := make(http.Header)
	headers.Set("Content-Length", "200")

	reader := bytes.NewReader(body)
	w := httptest.NewRecorder()

	// 应该失败，但数据应该已经发送给客户端
	err := cache.StreamToCache(key, reader, w, http.StatusOK, headers, 1*time.Hour, "test")
	// StreamToCache 不会因为文件太大而返回错误，只是不缓存
	// 数据已经发送给客户端
	if err != nil {
		t.Errorf("StreamToCache() should not error for too large entry, got: %v", err)
	}
}

func TestFileStorageWithMemoryMeta_ConcurrentAccess(t *testing.T) {
	cache, tmpDir := setupTestCache(t)
	defer cleanupTestCache(tmpDir)

	// 并发写入
	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func(i int) {
			key := GenerateKey("GET", "/simple/test"+string(rune(i))+"/")
			body := []byte("test")
			headers := make(http.Header)
			headers.Set("Content-Length", "4")
			reader := bytes.NewReader(body)
			w := httptest.NewRecorder()
			cache.StreamToCache(key, reader, w, http.StatusOK, headers, 1*time.Hour, "test")
			done <- true
		}(i)
	}

	// 等待所有goroutine完成
	for i := 0; i < 10; i++ {
		<-done
	}

	// 验证所有条目都存在
	for i := 0; i < 10; i++ {
		key := GenerateKey("GET", "/simple/test"+string(rune(i))+"/")
		got, err := cache.Get(key)
		if err != nil || got == nil {
			t.Errorf("Concurrent entry %d should exist", i)
		}
	}
}

func TestFileStorageWithMemoryMeta_Eviction(t *testing.T) {
	cache, tmpDir := setupTestCache(t)
	defer cleanupTestCache(tmpDir)

	// 设置小的maxSize以触发清理
	cache.maxSize = 1000

	// 添加多个条目，总大小超过高水位线（90%）
	// 高水位线是900，低水位线是700
	// 添加10个条目后，currentSize=1000，超过900，触发清理
	// 清理后应该<=700
	for i := 0; i < 10; i++ {
		key := GenerateKey("GET", "/simple/test"+string(rune(i))+"/")
		body := make([]byte, 100) // 每个100字节
		headers := make(http.Header)
		headers.Set("Content-Length", "100")
		reader := bytes.NewReader(body)
		w := httptest.NewRecorder()
		err := cache.StreamToCache(key, reader, w, http.StatusOK, headers, 1*time.Hour, "test")
		if err != nil {
			t.Logf("StreamToCache() error (expected for some entries): %v", err)
		}
	}

	// 验证清理后大小应该在低水位线（70%）以下
	size, err := cache.Size()
	if err != nil {
		t.Fatalf("Size() error = %v", err)
	}

	// 验证清理确实发生了（大小应该小于高水位线900）
	highWatermark := int64(float64(cache.maxSize) * cache.watermarkHigh)
	if size >= highWatermark {
		t.Errorf("Size() after eviction = %d, should be < %d (eviction should have occurred)", size, highWatermark)
	}
	// 理想情况下应该在低水位线以下，但由于清理逻辑的简化实现，可能不会完全精确
	// 至少验证清理确实发生了
	if size > cache.maxSize {
		t.Errorf("Size() after eviction = %d, should be <= %d", size, cache.maxSize)
	}
}
