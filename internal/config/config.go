package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config holds the application configuration
type Config struct {
	// Server configuration
	Port string `mapstructure:"port"`
	Host string `mapstructure:"host"`

	// Cache configuration
	CacheDir        string        `mapstructure:"dir"`
	CacheMaxSize    int64         `mapstructure:"max_size"` // Maximum cache directory size in bytes
	SimpleTTL       time.Duration `mapstructure:"simple_ttl"`
	PackagesTTL     time.Duration `mapstructure:"packages_ttl"`
	DefaultTTL      time.Duration `mapstructure:"default_ttl"`
	MaxCacheEntries int           `mapstructure:"max_entries"`

	// Watermark configuration
	WatermarkHigh float64 `mapstructure:"watermark_high"`
	WatermarkLow  float64 `mapstructure:"watermark_low"`

	// Upstream configuration
	PrimaryName        string        `mapstructure:"primary_name"`
	PrimaryUpstream    string        `mapstructure:"primary_url"`
	PrimaryPathPrefix  string        `mapstructure:"primary_path_prefix"`
	FallbackName       string        `mapstructure:"fallback_name"`
	FallbackUpstream   string        `mapstructure:"fallback_url"`
	FallbackPathPrefix string        `mapstructure:"fallback_path_prefix"`
	UpstreamTimeout    time.Duration `mapstructure:"timeout"`
	LargeFileTimeout   time.Duration `mapstructure:"large_file_timeout"`

	// Logging
	LogLevel string `mapstructure:"level"`

	// viper instance for advanced operations
	viper *viper.Viper
}

// New creates a new configuration using Viper
func New(configFile string) (*Config, error) {
	v := viper.New()

	// ==================== 配置源优先级 ====================
	// 1. 命令行参数（最高优先级）
	// 2. 环境变量
	// 3. 配置文件
	// 4. 默认值（最低优先级）

	// ==================== 设置默认值 ====================
	setDefaults(v)

	// ==================== 环境变量配置 ====================
	v.SetEnvPrefix("PIPCACHE") // 环境变量前缀: PIPCACHE_SERVER_PORT
	v.AutomaticEnv()           // 自动读取环境变量

	// 支持下划线和点号的映射
	// PIPCACHE_SERVER_PORT -> server.port
	// PIPCACHE_CACHE_DIR -> cache.dir
	v.SetEnvKeyReplacer(strings.NewReplacer("_", "."))

	// ==================== 配置文件支持 ====================
	if configFile != "" {
		v.SetConfigFile(configFile)
	} else {
		// 默认搜索路径
		v.SetConfigName("pipcache")        // 配置文件名
		v.SetConfigType("yaml")            // 配置文件类型
		v.AddConfigPath(".")               // 当前目录
		v.AddConfigPath("$HOME/.pipcache") // 用户目录
		v.AddConfigPath("/etc/pipcache/")  // 系统目录
	}

	// 尝试读取配置文件（如果不存在则忽略）
	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("failed to read config file: %w", err)
		}
		// 配置文件不存在是正常的，使用默认值和环境变量
	}

	// ==================== 解析为结构体 ====================
	cfg := &Config{}

	// 使用mapstructure将嵌套配置映射到扁平结构
	// 需要手动映射，因为Config结构体是扁平的
	if err := unmarshalConfig(v, cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	cfg.viper = v
	return cfg, nil
}

// unmarshalConfig 手动映射嵌套配置到扁平结构
func unmarshalConfig(v *viper.Viper, cfg *Config) error {
	// Server配置
	cfg.Port = v.GetString("server.port")
	cfg.Host = v.GetString("server.host")

	// Cache配置
	cfg.CacheDir = v.GetString("cache.dir")
	cfg.CacheMaxSize = v.GetInt64("cache.max_size")
	cfg.SimpleTTL = v.GetDuration("cache.simple_ttl")
	cfg.PackagesTTL = v.GetDuration("cache.packages_ttl")
	cfg.DefaultTTL = v.GetDuration("cache.default_ttl")
	cfg.MaxCacheEntries = v.GetInt("cache.max_entries")

	// Watermark配置
	cfg.WatermarkHigh = v.GetFloat64("cache.watermark.high")
	cfg.WatermarkLow = v.GetFloat64("cache.watermark.low")

	// Upstream配置
	cfg.PrimaryName = v.GetString("upstream.primary.name")
	cfg.PrimaryUpstream = v.GetString("upstream.primary.url")
	cfg.PrimaryPathPrefix = v.GetString("upstream.primary.path_prefix")
	cfg.FallbackName = v.GetString("upstream.fallback.name")
	cfg.FallbackUpstream = v.GetString("upstream.fallback.url")
	cfg.FallbackPathPrefix = v.GetString("upstream.fallback.path_prefix")
	cfg.UpstreamTimeout = v.GetDuration("upstream.timeout")
	cfg.LargeFileTimeout = v.GetDuration("upstream.large_file_timeout")

	// Log配置
	cfg.LogLevel = v.GetString("log.level")

	return nil
}

// setDefaults 设置所有默认值
func setDefaults(v *viper.Viper) {
	// 服务器配置
	v.SetDefault("server.port", "8080")
	v.SetDefault("server.host", "0.0.0.0")

	// 缓存配置
	v.SetDefault("cache.dir", "/cache")
	v.SetDefault("cache.max_size", 1024*1024*1024) // 1GB
	v.SetDefault("cache.simple_ttl", "5m")
	v.SetDefault("cache.packages_ttl", "87600h") // ~10 years
	v.SetDefault("cache.default_ttl", "1h")
	v.SetDefault("cache.max_entries", 10000)

	// 水位线配置
	v.SetDefault("cache.watermark.high", 0.9) // 90%
	v.SetDefault("cache.watermark.low", 0.7)  // 70%

	// 上游配置
	v.SetDefault("upstream.primary.name", "tuna")
	v.SetDefault("upstream.primary.url", "https://pypi.tuna.tsinghua.edu.cn")
	v.SetDefault("upstream.primary.path_prefix", "")
	v.SetDefault("upstream.fallback.name", "aliyun")
	v.SetDefault("upstream.fallback.url", "https://mirrors.aliyun.com")
	v.SetDefault("upstream.fallback.path_prefix", "/pypi")
	v.SetDefault("upstream.timeout", "30s")
	v.SetDefault("upstream.large_file_timeout", "10m") // 10 minutes for large files

	// 日志配置
	v.SetDefault("log.level", "info")
}
