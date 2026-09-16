# newsdesk —— 构建与自检
#
# 二进制装进 bin/（已 gitignore）。不写绝对路径：程序自己从可执行文件位置
# 往上找仓库根目录，也可以用 NEWSDESK_ROOT 指定。

GO      ?= go
BIN     := bin/newsdesk
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all build test test-go test-ts check fmt vet clean install doctor

all: build

build:
	@mkdir -p bin
	$(GO) -C go build -ldflags "-X main.version=$(VERSION)" -o ../$(BIN) ./cmd/newsdesk
	@echo "  → $(BIN)  ($(VERSION))"

test: test-go test-ts

test-go:
	$(GO) -C go test ./...

test-ts:
	@cd ts && [ -d node_modules ] || npm install --silent
	@cd ts && node --test

# 提交前跑这个
check: fmt vet test
	@cd ts && npx tsc --project tsconfig.json

fmt:
	$(GO) -C go fmt ./...

vet:
	$(GO) -C go vet ./...

install: build
	@echo "把 $(PWD)/bin 加进 PATH，或："
	@echo "  ln -sf $(PWD)/$(BIN) /usr/local/bin/newsdesk"

# 逐条实测配置、时间表、沙箱边界、发布链路
doctor: build
	@./$(BIN) doctor

clean:
	rm -rf bin
