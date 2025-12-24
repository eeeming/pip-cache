package core

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// Metadata represents cache entry metadata
type Metadata struct {
	Key        string            `json:"key"`
	StatusCode int               `json:"status_code"`
	Headers    map[string]string `json:"headers"` // 存储为 map 以便序列化
	ExpiresAt  time.Time         `json:"expires_at"`
	CachedAt   time.Time         `json:"cached_at"`
	BodySize   int64             `json:"body_size"`
	Upstream   string            `json:"upstream"` // 记录数据来源
}

// toHTTPHeader converts metadata headers map to http.Header
func (m *Metadata) toHTTPHeader() http.Header {
	if m.Headers == nil {
		return nil
	}
	h := make(http.Header)
	for k, v := range m.Headers {
		h.Add(k, v)
	}
	return h
}

// fromHTTPHeader sets metadata headers from http.Header
func (m *Metadata) fromHTTPHeader(h http.Header) {
	if h == nil {
		m.Headers = nil
		return
	}
	m.Headers = make(map[string]string)
	for k, v := range h {
		if len(v) > 0 {
			m.Headers[k] = v[0]
		}
	}
}

// FileStorageWithMemoryMeta implements Cache interface using file system with in-memory metadata
type FileStorageWithMemoryMeta struct {
	baseDir       string
	maxSize       int64
	maxEntries    int
	watermarkHigh float64 // High watermark (0.0-1.0), triggers eviction
	watermarkLow  float64 // Low watermark (0.0-1.0), target for eviction
	mu            sync.RWMutex
	currentSize   int64
	entryCount    int
	logger        *logrus.Logger

	// 内存中的meta信息
	metaMap sync.Map // map[string]*Metadata

	// 正在写入的文件集合，防止清理时删除正在写入的文件
	writingFiles sync.Map // map[string]bool
}

// NewFileStorageWithMemoryMeta creates a new file-based cache storage with in-memory metadata
func NewFileStorageWithMemoryMeta(baseDir string, maxSize int64, maxEntries int, watermarkHigh, watermarkLow float64, logger *logrus.Logger) (*FileStorageWithMemoryMeta, error) {
	// 创建基础目录
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	fs := &FileStorageWithMemoryMeta{
		baseDir:       baseDir,
		maxSize:       maxSize,
		maxEntries:    maxEntries,
		watermarkHigh: watermarkHigh,
		watermarkLow:  watermarkLow,
		logger:        logger,
	}

	// 清理启动前残留的临时文件
	if err := fs.cleanupTempFiles(); err != nil {
		logger.Warnf("Failed to cleanup temp files: %v", err)
	}

	// 从磁盘恢复 metadata
	if err := fs.recoverMetadata(); err != nil {
		logger.Warnf("Failed to recover metadata from disk: %v", err)
	}

	logger.Infof("Cache initialized: size=%d/%d (%.1f%%), entries=%d/%d",
		fs.currentSize, fs.maxSize, float64(fs.currentSize)/float64(fs.maxSize)*100,
		fs.entryCount, fs.maxEntries)

	return fs, nil
}

// cleanupTempFiles 清理启动前残留的临时文件
func (fs *FileStorageWithMemoryMeta) cleanupTempFiles() error {
	var cleaned int
	var cleanedSize int64

	err := filepath.Walk(fs.baseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		// 检查是否是临时文件（包含 .tmp. 的文件）
		if strings.Contains(filepath.Base(path), ".tmp.") {
			size := info.Size()
			if err := os.Remove(path); err != nil {
				fs.logger.Warnf("Failed to remove temp file %s: %v", path, err)
			} else {
				cleaned++
				cleanedSize += size
			}
		}

		return nil
	})

	if cleaned > 0 {
		fs.logger.Infof("Cleaned up %d temp files, freed %d bytes", cleaned, cleanedSize)
	}

	return err
}

// recoverMetadata 从磁盘恢复 metadata
// 扫描所有 .meta 文件，重建内存中的 metadata 和统计信息
func (fs *FileStorageWithMemoryMeta) recoverMetadata() error {
	var recoveredCount int
	var recoveredSize int64
	var orphanBodyCount int
	var orphanMetaCount int

	err := filepath.Walk(fs.baseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		filename := filepath.Base(path)
		dir := filepath.Dir(path)

		// 获取相对路径，用于推断 key
		_, err = filepath.Rel(fs.baseDir, dir)
		if err != nil {
			return err
		}

		// 检查是否是 .meta 文件
		hashFile := strings.TrimSuffix(filename, ".meta") // 去掉 .meta 后缀
		if hashFile == filename {
			// 不是 .meta 文件
			if strings.HasSuffix(filename, ".body") {
				// 检查是否有对应的 .meta 文件
				metaPath := filepath.Join(dir, strings.TrimSuffix(filename, ".body")+".meta")
				if _, err := os.Stat(metaPath); os.IsNotExist(err) {
					orphanBodyCount++
				}
			}
			return nil
		}

		// 构建完整的 hash（用于反向查找 key - 但我们无法反向 hash）
		// 相反，我们读取 meta 文件，其中存储了原始 key
		metaData, err := os.ReadFile(path)
		if err != nil {
			fs.logger.Warnf("Failed to read meta file %s: %v", path, err)
			return nil
		}

		var meta Metadata
		if err := json.Unmarshal(metaData, &meta); err != nil {
			fs.logger.Warnf("Failed to unmarshal meta file %s: %v", path, err)
			return nil
		}

		// 验证对应的 .body 文件是否存在
		bodyPath := filepath.Join(dir, hashFile+".body")
		if bodyInfo, err := os.Stat(bodyPath); err != nil {
			if os.IsNotExist(err) {
				orphanMetaCount++
				// 删除孤立的 .meta 文件
				os.Remove(path)
			}
			return nil
		} else {
			// 使用实际文件大小而不是 meta 中记录的大小
			meta.BodySize = bodyInfo.Size()
		}

		// 检查是否过期
		if time.Now().After(meta.ExpiresAt) {
			// 过期的条目，删除文件
			os.Remove(path)
			os.Remove(bodyPath)
			return nil
		}

		// 恢复到内存
		fs.metaMap.Store(meta.Key, &meta)
		recoveredCount++
		recoveredSize += meta.BodySize

		return nil
	})

	if err != nil {
		return err
	}

	// 更新统计信息
	fs.currentSize = recoveredSize
	fs.entryCount = recoveredCount

	if recoveredCount > 0 {
		fs.logger.Infof("Recovered %d cache entries (%d bytes) from disk", recoveredCount, recoveredSize)
	}
	if orphanBodyCount > 0 {
		fs.logger.Warnf("Found %d orphan body files (no matching meta), will be cleaned on eviction", orphanBodyCount)
	}
	if orphanMetaCount > 0 {
		fs.logger.Infof("Cleaned up %d orphan meta files", orphanMetaCount)
	}

	return nil
}

// saveMetadata 将 metadata 保存到磁盘
func (fs *FileStorageWithMemoryMeta) saveMetadata(meta *Metadata) error {
	metaPath, _ := GetCachePaths(fs.baseDir, meta.Key)

	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	// 原子写入：先写临时文件，然后 rename
	tmpMetaPath := metaPath + fmt.Sprintf(".tmp.%d", time.Now().UnixNano())
	if err := os.WriteFile(tmpMetaPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write temp meta file: %w", err)
	}

	if err := os.Rename(tmpMetaPath, metaPath); err != nil {
		os.Remove(tmpMetaPath)
		return fmt.Errorf("failed to rename meta file: %w", err)
	}

	return nil
}

// Get retrieves a cache entry
func (fs *FileStorageWithMemoryMeta) Get(key string) (*Entry, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	// 从内存获取meta
	metaInterface, ok := fs.metaMap.Load(key)
	if !ok {
		return nil, nil // 缓存未命中
	}

	meta := metaInterface.(*Metadata)

	// 检查过期
	if time.Now().After(meta.ExpiresAt) {
		// 过期，删除
		fs.mu.RUnlock()
		fs.Delete(key)
		fs.mu.RLock()
		return nil, nil
	}

	// 获取body文件路径
	_, bodyPath := GetCachePaths(fs.baseDir, key)

	// 读取数据
	body, err := os.ReadFile(bodyPath)
	if err != nil {
		if os.IsNotExist(err) {
			// 文件不存在，删除meta
			fs.mu.RUnlock()
			fs.Delete(key)
			fs.mu.RLock()
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read body: %w", err)
	}

	return &Entry{
		StatusCode: meta.StatusCode,
		Headers:    meta.toHTTPHeader(),
		Body:       body,
		ExpiresAt:  meta.ExpiresAt,
		CachedAt:   meta.CachedAt,
	}, nil
}

// Delete removes a cache entry
func (fs *FileStorageWithMemoryMeta) Delete(key string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	metaPath, bodyPath := GetCachePaths(fs.baseDir, key)

	// 从内存获取meta以获取大小
	if metaInterface, ok := fs.metaMap.Load(key); ok {
		meta := metaInterface.(*Metadata)
		fs.currentSize -= meta.BodySize
		fs.metaMap.Delete(key)
	}

	// 删除body文件
	if err := os.Remove(bodyPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove body file: %w", err)
	}

	// 删除meta文件
	if err := os.Remove(metaPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove meta file: %w", err)
	}

	fs.entryCount--

	// 尝试删除空目录
	if dir, _ := filepath.Split(bodyPath); dir != "" {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
			os.Remove(dir)
		}
	}

	return nil
}

// Size returns the current cache size in bytes
func (fs *FileStorageWithMemoryMeta) Size() (int64, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return fs.currentSize, nil
}

// StreamFromCache streams a cached entry to the writer
func (fs *FileStorageWithMemoryMeta) StreamFromCache(key string, w http.ResponseWriter) error {
	fs.mu.RLock()

	// 从内存获取meta
	metaInterface, ok := fs.metaMap.Load(key)
	if !ok {
		fs.mu.RUnlock()
		return fmt.Errorf("cache entry not found")
	}

	meta := metaInterface.(*Metadata)

	// 检查过期
	if time.Now().After(meta.ExpiresAt) {
		fs.mu.RUnlock()
		fs.Delete(key)
		return fmt.Errorf("cache entry expired")
	}

	_, bodyPath := GetCachePaths(fs.baseDir, key)
	fs.mu.RUnlock()

	// 复制headers到响应
	headers := meta.toHTTPHeader()
	for key, values := range headers {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	// 设置自定义headers（缓存状态和来源）
	w.Header().Set("X-Cache-Status", "HIT")
	if meta.Upstream != "" {
		w.Header().Set("X-Upstream", meta.Upstream)
	}
	w.WriteHeader(meta.StatusCode)

	// 打开文件并流式传输
	file, err := os.Open(bodyPath)
	if err != nil {
		return fmt.Errorf("failed to open cache file: %w", err)
	}
	defer file.Close()

	_, err = io.Copy(w, file)
	return err
}

// StreamToCache streams data from reader to cache while writing to writer simultaneously
// Uses io.TeeReader to write to both cache file and client at the same time
// upstream: 数据来源的upstream名称，会保存到metadata中
func (fs *FileStorageWithMemoryMeta) StreamToCache(key string, r io.Reader, w http.ResponseWriter, statusCode int, headers http.Header, ttl time.Duration, upstream string) error {
	// 标记正在写入
	fs.writingFiles.Store(key, true)
	defer fs.writingFiles.Delete(key)

	_, bodyPath := GetCachePaths(fs.baseDir, key)

	// 创建临时文件用于缓存
	tmpBodyPath := bodyPath + fmt.Sprintf(".tmp.%d", time.Now().UnixNano())
	bodyFile, err := os.Create(tmpBodyPath)
	if err != nil {
		return fmt.Errorf("failed to create cache file: %w", err)
	}
	defer bodyFile.Close()

	// 使用io.TeeReader同时写入缓存文件和客户端
	teeReader := io.TeeReader(r, bodyFile)

	// 复制headers到响应（保留Content-Length以显示进度）
	for key, values := range headers {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	// 设置自定义headers（缓存状态和来源）
	w.Header().Set("X-Cache-Status", "MISS")
	w.Header().Set("X-Upstream", upstream)

	w.WriteHeader(statusCode)

	// 使用Flusher确保数据及时发送
	flusher, hasFlusher := w.(http.Flusher)
	if hasFlusher {
		flusher.Flush() // 立即flush一次
	}

	// 流式传输数据到客户端（同时写入缓存文件）
	const chunkSize = 16 * 1024    // 16KB chunks
	const flushInterval = 4 * 1024 // Flush every 4KB
	buf := make([]byte, chunkSize)
	var written int64
	lastFlushSize := int64(0)

	for {
		nr, er := teeReader.Read(buf)
		if nr > 0 {
			nw, ew := w.Write(buf[0:nr])
			if nw < 0 || nr < nw {
				nw = 0
				if ew == nil {
					ew = fmt.Errorf("invalid write result")
				}
			}
			written += int64(nw)

			// 频繁flush以确保数据及时发送
			if hasFlusher && written-lastFlushSize >= flushInterval {
				flusher.Flush()
				lastFlushSize = written
			}

			if ew != nil {
				os.Remove(tmpBodyPath)
				return fmt.Errorf("failed to write to client: %w", ew)
			}
			if nr != nw {
				os.Remove(tmpBodyPath)
				return io.ErrShortWrite
			}
		}
		if er != nil {
			if er != io.EOF {
				// 读取失败，删除临时文件
				os.Remove(tmpBodyPath)
				return fmt.Errorf("failed to read from upstream: %w", er)
			}
			break
		}
	}

	// 最后一次flush
	if hasFlusher {
		flusher.Flush()
	}

	// 关闭临时文件
	bodyFile.Close()

	// 验证完整性（如果有Content-Length）
	contentLength, hasLength := getContentLengthFromHeaders(headers)
	if hasLength && written != contentLength {
		os.Remove(tmpBodyPath)
		fs.logger.Warnf("Incomplete download for %s: expected %d bytes, got %d bytes", key, contentLength, written)
		// 注意：数据已经传输给客户端了，只是不完整
		// 返回错误，让调用者知道下载不完整
		return fmt.Errorf("incomplete download: expected %d bytes, got %d bytes", contentLength, written)
	}

	// 检查文件大小是否超过单个文件限制
	maxSize := fs.getMaxSingleFileSize()
	if written > maxSize {
		os.Remove(tmpBodyPath)
		fs.logger.Debugf("File too large to cache for %s: %d bytes (max: %d bytes)", key, written, maxSize)
		// 注意：数据已经传输给客户端了，只是不缓存
		return nil // 不返回错误，因为客户端已经收到数据
	}

	// 检查缓存空间
	fs.mu.Lock()
	if fs.shouldEvict(written) {
		fs.mu.Unlock()
		if err := fs.evictToLowWatermark(); err != nil {
			os.Remove(tmpBodyPath)
			fs.logger.Warnf("Failed to evict cache, removing temp file: %v", err)
			// 注意：数据已经传输给客户端了，只是缓存失败
			return nil // 不返回错误，因为客户端已经收到数据
		}
		fs.mu.Lock() // 重新获取锁
	}

	if fs.currentSize+written > fs.maxSize {
		fs.mu.Unlock()
		os.Remove(tmpBodyPath)
		fs.logger.Warnf("Insufficient cache space, removing temp file")
		// 注意：数据已经传输给客户端了，只是缓存失败
		return nil // 不返回错误，因为客户端已经收到数据
	}

	// 将临时文件重命名为正式文件
	if err := os.Rename(tmpBodyPath, bodyPath); err != nil {
		fs.mu.Unlock()
		os.Remove(tmpBodyPath)
		return fmt.Errorf("failed to rename cache file: %w", err)
	}

	// 更新内存meta
	meta := &Metadata{
		Key:        key,
		StatusCode: statusCode,
		ExpiresAt:  time.Now().Add(ttl),
		CachedAt:   time.Now(),
		BodySize:   written,
		Upstream:   upstream,
	}
	meta.fromHTTPHeader(headers)
	fs.metaMap.Store(key, meta)

	// 更新统计
	fs.currentSize += written
	fs.entryCount++

	// 保存 meta 文件到磁盘（在锁外执行，避免阻塞）
	// 注意：这里在锁内调用，但 saveMetadata 很快（只是写入小的 JSON 文件）
	if err := fs.saveMetadata(meta); err != nil {
		fs.logger.Warnf("Failed to save metadata to disk: %v", err)
		// 不返回错误，因为内存中已经有 metadata 了
	}

	fs.mu.Unlock()

	fs.logger.Debugf("Successfully cached %s: size=%d bytes", key, written)
	return nil
}

// getContentLengthFromHeaders 从headers中提取Content-Length
func getContentLengthFromHeaders(headers http.Header) (int64, bool) {
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

// getMaxSingleFileSize 返回单个文件的最大缓存大小
func (fs *FileStorageWithMemoryMeta) getMaxSingleFileSize() int64 {
	return int64(float64(fs.maxSize) * MaxSingleFileRatio)
}

// shouldEvict 检查是否需要清理
func (fs *FileStorageWithMemoryMeta) shouldEvict(additionalSize int64) bool {
	return fs.currentSize+additionalSize > int64(float64(fs.maxSize)*fs.watermarkHigh)
}

// evictToLowWatermark 清理到低水位线
// 注意：调用此函数时不应该持有mu锁
func (fs *FileStorageWithMemoryMeta) evictToLowWatermark() error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	targetSize := int64(float64(fs.maxSize) * fs.watermarkLow)

	// 收集所有过期的entry
	var expiredKeys []string
	fs.metaMap.Range(func(key, value interface{}) bool {
		meta := value.(*Metadata)
		if time.Now().After(meta.ExpiresAt) {
			expiredKeys = append(expiredKeys, key.(string))
		}
		return true
	})

	// 删除过期的
	for _, key := range expiredKeys {
		fs.deleteInternalUnlocked(key)
		if fs.currentSize <= targetSize {
			return nil
		}
	}

	// 如果还不够，按LRU删除（简化：按CachedAt排序）
	if fs.currentSize > targetSize {
		// 收集所有entry并按CachedAt排序
		type entryWithTime struct {
			key      string
			cachedAt time.Time
			size     int64
		}
		var entries []entryWithTime
		fs.metaMap.Range(func(key, value interface{}) bool {
			meta := value.(*Metadata)
			entries = append(entries, entryWithTime{
				key:      key.(string),
				cachedAt: meta.CachedAt,
				size:     meta.BodySize,
			})
			return true
		})

		// 按CachedAt排序（最早的在前）
		for i := 0; i < len(entries)-1; i++ {
			for j := i + 1; j < len(entries); j++ {
				if entries[i].cachedAt.After(entries[j].cachedAt) {
					entries[i], entries[j] = entries[j], entries[i]
				}
			}
		}

		// 删除最早的直到达到目标大小
		for _, entry := range entries {
			if fs.currentSize <= targetSize {
				break
			}
			// 检查是否正在写入，如果是则跳过
			if _, writing := fs.writingFiles.Load(entry.key); writing {
				continue
			}
			fs.deleteInternalUnlocked(entry.key)
		}
	}

	return nil
}

// deleteInternalUnlocked 删除缓存条目的内部实现（已持有锁）
func (fs *FileStorageWithMemoryMeta) deleteInternalUnlocked(key string) {
	metaPath, bodyPath := GetCachePaths(fs.baseDir, key)

	// 从内存获取meta以获取大小
	if metaInterface, ok := fs.metaMap.Load(key); ok {
		meta := metaInterface.(*Metadata)
		fs.currentSize -= meta.BodySize
		fs.metaMap.Delete(key)
		fs.entryCount--

		// 删除body和meta文件（在锁外执行，避免阻塞）
		go func() {
			os.Remove(bodyPath)
			os.Remove(metaPath)
			// 尝试删除空目录
			if dir, _ := filepath.Split(bodyPath); dir != "" {
				if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
					os.Remove(dir)
				}
			}
		}()
	}
}
