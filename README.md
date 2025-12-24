# PyPI 缓存代理

基于 Go 标准库的高性能 PyPI 镜像缓存代理服务，支持多源回退、分级缓存策略、自动故障切换和元数据持久化。

## 功能特性

- **多源回退**：优先清华 tuna，失败时回退阿里云
- **分级缓存**：元数据 5 分钟，包文件 ~10 年
- **持久化缓存**：程序重启后自动恢复缓存元数据
- **流式传输**：边下载边转发边缓存，内存占用低
- **LRU 策略**：高低水位线管理，自动清理旧缓存
- **进度显示**：保留 Content-Length，pip 可显示下载进度
- **健康检查**：`/health` 端点监控状态

## 架构设计

### 缓存存储结构

```
cache/
├── abc/                    # hash 目录 (4096个)
│   ├── 123def.body        # 实际数据
│   └── 123def.meta        # 元数据 (JSON)
└── ...
```

### 元数据格式

```json
{
  "key": "GET-/simple/requests/",
  "status_code": 200,
  "headers": {"content-type": "text/html"},
  "expires_at": "2025-12-25T10:00:00Z",
  "cached_at": "2025-12-24T10:00:00Z",
  "body_size": 12345
}
```

### 一致性保证

- **原子写入**：先写 `.body`，成功后写 `.meta`
- **启动恢复**：扫描 `.meta` 文件重建内存索引
- **自动清理**：删除孤儿文件和过期缓存
- **并发安全**：使用 sync.Map 和读写锁保护

## 快速开始

### Docker 部署（推荐）

```bash
# 构建镜像
docker build -t pip-cache .

# 运行服务
docker run -d --name pip-cache \
  -p 8080:8080 \
  -v $(pwd)/cache:/cache \
  pip-cache

# 查看日志
docker logs -f pip-cache
```

### 本地运行

```bash
# 运行
go run main.go

# 或使用 make
make run
```

## 配置

### 配置文件

创建 `pipcache.yaml`：

```yaml
server:
  port: 8080
  host: 0.0.0.0

cache:
  dir: /cache                    # 缓存目录
  max_size: 1073741824           # 最大缓存大小 (1GB)
  max_entries: 10000             # 最大条目数
  simple_ttl: 5m                 # /simple/ TTL
  packages_ttl: 87600h           # /packages/ TTL (~10年)
  default_ttl: 1h                # 默认 TTL
  watermark:
    high: 0.9                    # 高水位线 90%
    low: 0.7                     # 低水位线 70%

upstream:
  primary:
    name: tuna
    url: https://pypi.tuna.tsinghua.edu.cn
    path_prefix: ""
  fallback:
    name: aliyun
    url: https://mirrors.aliyun.com
    path_prefix: /pypi
  timeout: 30s                   # 请求超时
  large_file_timeout: 10m        # 大文件超时

log:
  level: info                    # debug/info/warn/error
```

### 环境变量

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `PIPCACHE_SERVER_PORT` | 服务端口 | `8080` |
| `PIPCACHE_SERVER_HOST` | 监听地址 | `0.0.0.0` |
| `PIPCACHE_CACHE_DIR` | 缓存目录 | `/cache` |
| `PIPCACHE_CACHE_MAX_SIZE` | 缓存大小 | `1073741824` |
| `PIPCACHE_LOG_LEVEL` | 日志级别 | `info` |

## 使用方法

### 配置 pip

编辑 `~/.pip/pip.conf`：

```ini
[global]
index-url = http://localhost:8080/simple/
trusted-host = localhost
```

### 测试安装

```bash
pip install requests
```

### 使用 Poetry

```bash
# 在 pyproject.toml 中配置
[[tool.poetry.source]]
name = "custom"
url = "http://localhost:8080/simple/"
```

### 使用 uv

```bash
uv pip install requests --index-url http://localhost:8080/simple/
```

## API

### 健康检查

```bash
curl http://localhost:8080/health
```

响应：
```json
{
  "status": "healthy",
  "cache_size": 1234567
}
```

### 响应头

| 头部 | 说明 | 值 |
|------|------|-----|
| `X-Cache-Status` | 缓存状态 | `HIT` / `MISS` |
| `X-Upstream` | 上游服务器 | `tuna` / `aliyun` |

## 缓存策略

### TTL 设置

| 路径 | TTL | 说明 |
|------|-----|------|
| `/simple/*` | 5 分钟 | 包索引，变化频繁 |
| `/packages/*` | ~10 年 | 包文件，内容不变 |
| 其他 | 1 小时 | 默认过期时间 |

### 清理策略

- **高水位线** (90%)：触发清理
- **低水位线** (70%)：清理目标
- **LRU**：优先删除最旧的条目
- **并发保护**：正在写入的文件不会被删除

## 性能优化

### 连接池配置

```go
MaxIdleConns: 100              // 最大空闲连接
MaxIdleConnsPerHost: 10        // 每个主机的空闲连接
MaxConnsPerHost: 50            // 每个主机的最大连接
IdleConnTimeout: 90s           // 空闲连接超时
```

### 流式传输

- 16KB 读缓冲区
- 每 4KB flush 一次
- 支持进度条显示

## 故障排查

### 常见问题

**缓存未命中？**
```bash
# 检查缓存目录
ls -la cache/

# 检查健康状态
curl http://localhost:8080/health
```

**权限问题？**
```bash
chmod -R 755 ./cache/
```

**端口占用？**
```bash
# 修改端口
export PIPCACHE_SERVER_PORT=9000
```

**清理缓存？**
```bash
rm -rf ./cache/*
# 或重启服务让其自动恢复
```

### 调试模式

```bash
# 启用调试日志
export PIPCACHE_LOG_LEVEL=debug
go run main.go
```

## 开发

### 运行测试

```bash
go test ./...
go test -v ./internal/core/
```

### 构建

```bash
go build -o pip-cache main.go
```
