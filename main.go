package main

import (
	"fmt"
	"net/http"
	"time"

	"github.com/eeeming/pip-cache/internal/config"
	"github.com/eeeming/pip-cache/internal/core"
	"github.com/eeeming/pip-cache/internal/handler"
	"github.com/eeeming/pip-cache/internal/proxy"
	"github.com/sirupsen/logrus"
)

func main() {
	// 初始化日志
	logger := logrus.New()
	logger.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})

	// 加载配置
	cfg, err := config.New("")
	if err != nil {
		logger.Fatalf("Failed to load configuration: %v", err)
	}

	// 设置日志级别
	level, err := logrus.ParseLevel(cfg.LogLevel)
	if err != nil {
		logger.Warnf("Invalid log level '%s', using 'info'", cfg.LogLevel)
		level = logrus.InfoLevel
	}
	logger.SetLevel(level)

	logger.Infof("Starting PyPI Cache Proxy")
	logger.Infof("Cache directory: %s", cfg.CacheDir)
	logger.Infof("Primary upstream: %s", cfg.PrimaryUpstream)
	logger.Infof("Fallback upstream: %s", cfg.FallbackUpstream)

	// 初始化缓存存储（使用内存meta）
	cacheStorage, err := core.NewFileStorageWithMemoryMeta(
		cfg.CacheDir,
		cfg.CacheMaxSize,
		cfg.MaxCacheEntries,
		cfg.WatermarkHigh,
		cfg.WatermarkLow,
		logger,
	)
	if err != nil {
		logger.Fatalf("Failed to initialize cache storage: %v", err)
	}

	logger.Infof("Cache storage initialized")

	// 创建HTTP客户端用于上游服务器
	primaryTransport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		MaxConnsPerHost:     50,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   false,
		DisableCompression:  false,
		ForceAttemptHTTP2:   true,
	}

	fallbackTransport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		MaxConnsPerHost:     50,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   false,
		DisableCompression:  false,
		ForceAttemptHTTP2:   true,
	}

	primaryClient := &http.Client{
		Timeout:   cfg.UpstreamTimeout,
		Transport: primaryTransport,
	}

	fallbackClient := &http.Client{
		Timeout:   cfg.UpstreamTimeout,
		Transport: fallbackTransport,
	}

	// 创建上游配置
	primaryUpstream := proxy.NewUpstream(cfg.PrimaryName, cfg.PrimaryUpstream, cfg.PrimaryPathPrefix, primaryClient)
	fallbackUpstream := proxy.NewUpstream(cfg.FallbackName, cfg.FallbackUpstream, cfg.FallbackPathPrefix, fallbackClient)

	logger.Infof("Upstream servers configured")

	// 创建代理客户端
	proxyClient := proxy.NewProxyClient(primaryUpstream, fallbackUpstream, logger)

	// 设置大文件超时
	if cfg.LargeFileTimeout > 0 {
		proxyClient.LargeFileTimeout = cfg.LargeFileTimeout
		logger.Infof("Large file timeout set to: %v", cfg.LargeFileTimeout)
	}

	// 创建HTTP处理器
	httpHandler := handler.NewHandler(cacheStorage, proxyClient, cfg, logger)

	// 创建HTTP服务器
	addr := fmt.Sprintf("%s:%s", cfg.Host, cfg.Port)
	logger.Infof("Starting server on %s", addr)

	srv := &http.Server{
		Addr:           addr,
		Handler:        httpHandler,
		ReadTimeout:    30 * time.Second,
		WriteTimeout:   30 * time.Minute,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Fatalf("Failed to start server: %v", err)
	}
}
