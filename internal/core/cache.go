package core

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// MaxSingleFileRatio 单个文件最大占用缓存空间的比例
	// 设置为 0.5 表示单个文件不能超过总缓存大小的 50%
	MaxSingleFileRatio = 0.5
)

// Entry represents a cached response
type Entry struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
	ExpiresAt  time.Time
	CachedAt   time.Time
}

// MarshalJSON implements json.Marshaler for Entry
// Custom marshaling for http.Header
func (e *Entry) MarshalJSON() ([]byte, error) {
	type Alias Entry
	return json.Marshal(&struct {
		Headers map[string]string `json:"headers"`
		*Alias
	}{
		Headers: headerToMap(e.Headers),
		Alias:   (*Alias)(e),
	})
}

// UnmarshalJSON implements json.Unmarshaler for Entry
func (e *Entry) UnmarshalJSON(data []byte) error {
	type Alias Entry
	aux := &struct {
		Headers map[string]string `json:"headers"`
		*Alias
	}{
		Alias: (*Alias)(e),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	e.Headers = mapToHeader(aux.Headers)
	return nil
}

// headerToMap converts http.Header to map[string]string
func headerToMap(h http.Header) map[string]string {
	if h == nil {
		return nil
	}
	m := make(map[string]string)
	for k, v := range h {
		if len(v) > 0 {
			m[k] = v[0] // 只取第一个值
		}
	}
	return m
}

// mapToHeader converts map[string]string to http.Header
func mapToHeader(m map[string]string) http.Header {
	if m == nil {
		return nil
	}
	h := make(http.Header)
	for k, v := range m {
		h.Add(k, v)
	}
	return h
}

// Cache interface for cache operations
type Cache interface {
	Get(key string) (*Entry, error)
	Delete(key string) error
	Size() (int64, error)
	// StreamFromCache streams a cached entry to the writer
	StreamFromCache(key string, w http.ResponseWriter) error
	// StreamToCache streams data from reader to cache while writing to writer
	// upstream: 数据来源的upstream名称，会保存到metadata中
	StreamToCache(key string, r io.Reader, w http.ResponseWriter, statusCode int, headers http.Header, ttl time.Duration, upstream string) error
}

// GenerateKey generates a cache key from HTTP method and path
// Format: {method}-{path}
func GenerateKey(method, path string) string {
	// Normalize the method and path
	method = strings.ToUpper(method)
	// Remove query parameters for cache key
	if idx := strings.Index(path, "?"); idx != -1 {
		path = path[:idx]
	}
	return fmt.Sprintf("%s-%s", method, path)
}

// HashKeyWithDir generates a hashed key with directory structure for better filesystem performance
// Returns directory path and filename
// Uses first 3 characters of hash as directory name (4096 possible directories)
func HashKeyWithDir(key string) (dir, filename string) {
	hash := md5.Sum([]byte(key))
	hashStr := hex.EncodeToString(hash[:])

	// Use first 3 characters as directory (4096 possible directories)
	// This provides good distribution while keeping directory count manageable
	dir = hashStr[:3]
	filename = hashStr[3:] // Remaining 29 characters

	return dir, filename
}

// GetCachePaths returns the full paths for meta and body files using directory hashing
func GetCachePaths(baseDir, key string) (metaPath, bodyPath string) {
	dir, filename := HashKeyWithDir(key)

	// Create full directory path
	cacheDir := filepath.Join(baseDir, dir)

	// Ensure directory exists
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		// If we can't create directory, return error paths
		return "", ""
	}

	// metaPath is not used but kept for compatibility
	metaPath = filepath.Join(cacheDir, filename+".meta")
	bodyPath = filepath.Join(cacheDir, filename+".body")
	return metaPath, bodyPath
}

// IsExpired checks if a cache entry has expired
func (e *Entry) IsExpired() bool {
	return time.Now().After(e.ExpiresAt)
}

// GetTTLForPath returns the appropriate TTL based on the request path
func GetTTLForPath(path string, simpleTTL, packagesTTL, defaultTTL time.Duration) time.Duration {
	if strings.HasPrefix(path, "/simple/") {
		return simpleTTL
	}
	if strings.HasPrefix(path, "/packages/") {
		return packagesTTL
	}
	return defaultTTL
}
