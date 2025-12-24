# Makefile for pip-cache

# 变量定义
IMAGE_NAME := pip-cache
CONTAINER_NAME := pip-cache
CACHE_DIR := ./cache
DOCKERFILE := ./Dockerfile
TAG :=
REGISTRY :=
BINARY_NAME := pip-cache

# 默认端口
HTTP_PORT := 8080

# Go 相关变量
GOCMD := go
GOBUILD := $(GOCMD) build
GOTEST := $(GOCMD) test
GOCLEAN := $(GOCMD) clean
GOGET := $(GOCMD) get
GOMOD := $(GOCMD) mod

# 构建标志
LDFLAGS := -s -w
BUILD_FLAGS := -ldflags "$(LDFLAGS)"

# 默认目标
.PHONY: help
help: ## 显示帮助信息
	@echo "可用的 make 命令:"
	@echo "  build-go             编译 Go 二进制文件"
	@echo "  build TAG=tagname    构建 Docker 镜像 (可选设置tag)"
	@echo "  push TAG=tagname     推送 Docker 镜像 (可选设置tag和REGISTRY)"
	@echo "  run                  运行 Docker 容器"
	@echo "  run-local            本地运行 Go 程序"
	@echo "  stop                 停止 Docker 容器"
	@echo "  logs                 查看容器日志"
	@echo "  status               查看容器状态"
	@echo "  test                 运行测试"
	@echo "  clean                清理构建文件"
	@echo ""

# 编译 Go 二进制文件
.PHONY: build-go
build-go: ## 编译 Go 二进制文件
	@echo "编译 Go 二进制文件..."
	$(GOBUILD) $(BUILD_FLAGS) -o $(BINARY_NAME) .
	@echo "编译完成: $(BINARY_NAME)"

# 构建 Docker 镜像
.PHONY: build
build: ## 构建 Docker 镜像
	@if [ -n "$(TAG)" ]; then \
		echo "构建 Docker 镜像: $(IMAGE_NAME):$(TAG)"; \
		docker build -t $(IMAGE_NAME):$(TAG) -f $(DOCKERFILE) .; \
	else \
		echo "构建 Docker 镜像: $(IMAGE_NAME)"; \
		docker build -t $(IMAGE_NAME) -f $(DOCKERFILE) .; \
	fi

# 推送 Docker 镜像
.PHONY: push
push: build ## 推送 Docker 镜像
	@if [ -n "$(TAG)" ]; then \
		if [ -n "$(REGISTRY)" ]; then \
			REMOTE_IMAGE=$(REGISTRY)/$(IMAGE_NAME):$(TAG); \
			echo "标记镜像: $(IMAGE_NAME):$(TAG) -> $$REMOTE_IMAGE"; \
			docker tag $(IMAGE_NAME):$(TAG) $$REMOTE_IMAGE; \
			echo "推送镜像: $$REMOTE_IMAGE"; \
			docker push $$REMOTE_IMAGE; \
		else \
			echo "推送镜像: $(IMAGE_NAME):$(TAG)"; \
			docker push $(IMAGE_NAME):$(TAG); \
		fi; \
	else \
		if [ -n "$(REGISTRY)" ]; then \
			REMOTE_IMAGE=$(REGISTRY)/$(IMAGE_NAME); \
			echo "标记镜像: $(IMAGE_NAME) -> $$REMOTE_IMAGE"; \
			docker tag $(IMAGE_NAME) $$REMOTE_IMAGE; \
			echo "推送镜像: $$REMOTE_IMAGE"; \
			docker push $$REMOTE_IMAGE; \
		else \
			echo "推送镜像: $(IMAGE_NAME)"; \
			docker push $(IMAGE_NAME); \
		fi; \
	fi

# 创建缓存目录
$(CACHE_DIR):
	@mkdir -p $(CACHE_DIR)

# 运行 Docker 容器
.PHONY: run
run: build $(CACHE_DIR) ## 运行 Docker 容器
	@if [ -n "$(TAG)" ]; then \
		IMAGE_TAG=$(IMAGE_NAME):$(TAG); \
	else \
		IMAGE_TAG=$(IMAGE_NAME); \
	fi; \
	echo "启动 Docker 容器: $(CONTAINER_NAME)"; \
	docker run -d \
		--name $(CONTAINER_NAME) \
		--replace \
		-p $(HTTP_PORT):8080 \
		-v $(PWD)/$(CACHE_DIR):/cache \
		--restart unless-stopped \
		$$IMAGE_TAG

# 本地运行 Go 程序
.PHONY: run-local
run-local: build-go $(CACHE_DIR) ## 本地运行 Go 程序
	@echo "本地运行 $(BINARY_NAME)..."
	@echo "缓存目录: $(PWD)/$(CACHE_DIR)"
	CACHE_DIR=$(PWD)/$(CACHE_DIR) PORT=$(HTTP_PORT) ./$(BINARY_NAME)

# 停止 Docker 容器
.PHONY: stop
stop: ## 停止 Docker 容器
	@echo "停止容器: $(CONTAINER_NAME)"
	docker stop $(CONTAINER_NAME)

# 查看容器日志
.PHONY: logs
logs: ## 查看容器日志
	@echo "查看容器日志: $(CONTAINER_NAME)"
	docker logs -f $(CONTAINER_NAME)

# 查看容器状态
.PHONY: status
status: ## 查看容器状态
	@echo "容器状态:"
	docker ps -a --filter name=$(CONTAINER_NAME) --format "table {{.Names}}\t{{.Status}}\t{{.Ports}}"
	@echo ""
	@echo "镜像信息:"
	docker images --filter reference=$(IMAGE_NAME) --format "table {{.Repository}}\t{{.Tag}}\t{{.Size}}"

# 运行测试
.PHONY: test
test: ## 运行测试
	@echo "运行测试..."
	$(GOTEST) -v ./...

# 清理构建文件
.PHONY: clean
clean: ## 清理构建文件
	@echo "清理构建文件..."
	$(GOCLEAN)
	rm -f $(BINARY_NAME)
	@echo "清理完成"

# 更新依赖
.PHONY: deps
deps: ## 更新依赖
	@echo "更新 Go 依赖..."
	$(GOMOD) tidy
	$(GOMOD) download

# 格式化代码
.PHONY: fmt
fmt: ## 格式化代码
	@echo "格式化代码..."
	$(GOCMD) fmt ./...

# 代码检查
.PHONY: lint
lint: ## 代码检查
	@echo "运行代码检查..."
	@which golangci-lint > /dev/null || (echo "请安装 golangci-lint" && exit 1)
	golangci-lint run ./...
