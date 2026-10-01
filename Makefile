BINARY := bin/skydroid-onvif-adapter
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(DATE)

.PHONY: build test lint probe run clean version help

help: ## 显示帮助
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

version: ## 显示版本
	@echo $(VERSION)

build: ## 编译单二进制
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/skydroid-onvif-adapter

test: ## 全量测试（含端到端）
	go test -race ./...

lint: ## go vet
	go vet ./...

probe: build ## 真机探测（验证与相机的连通性）
	$(BINARY) --probe --camera-ip $(CAMERA_IP)

release: ## 通过 goreleaser 发布（tag 触发，需 goreleaser 与 GITHUB_TOKEN）
	goreleaser release --clean

snapshot: ## 本地验证发布产物（不打 tag、不上传）
	goreleaser build --snapshot --clean

run: build ## 本地运行
	$(BINARY)

clean: ## 清理构建产物
	rm -rf bin
