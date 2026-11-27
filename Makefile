# UFOT 构建骨架。目标说明见 docs/architecture.md 实施顺序节与 docs/deploy.md。
SHELL := /bin/bash

PROTO_DIR     := proto
GEN_DIR       := server/internal/proto/gen/chatpb
DIST_DIR      := dist
BIN           := chatd

# 发布包版本号：tag 优先，否则日期。目录名做白名单清洗（release.sh 同规则）。
# 标签优先，其次提交短号。
VERSION ?= $(shell git describe --tags --always 2>/dev/null || date -u +%Y%m%d%H%M%S)
ARCH    ?= $(shell go env GOARCH 2>/dev/null || echo amd64)

.PHONY: proto build test lint kinds release release-all clean importlint

# importlint: enforce docs/bridge-design.md §4.1 import red lines (P0-1).
# Included in lint (therefore CI). Runs standalone too.
importlint:
	@bash scripts/importlint.sh

# proto: 生成 chatpb。无 protoc 时保留仓库内已提交的 wire 兼容实现
# (server/internal/proto/gen/chatpb)，代码照常构建 —— CI 无需预装 protoc。
# 整段必须写在同一个 shell 里：make 的每条 recipe 行是独立 shell，
# `exit 0` 只结束那一行，下一行的 protoc 仍会执行（CI 实测踩到）。
proto:
	@if ! command -v protoc >/dev/null 2>&1 || ! command -v protoc-gen-go >/dev/null 2>&1; then \
		echo "protoc/protoc-gen-go not found: keeping committed chatpb implementation"; \
	else \
		protoc \
			--go_out=server/internal/proto/gen \
			--go_opt=paths=source_relative \
			--go_opt=M$(PROTO_DIR)/chat.proto=github.com/yingo-server/UFOT/$(GEN_DIR) \
			$(PROTO_DIR)/chat.proto; \
	fi

build: proto
	go build ./...

# -p 1: each test package boots a real server; running many in parallel on a
# small machine exhausts ports/CPU and produces spurious failures (observed
# locally with 2 cores). Determinism matters more than wall-clock here.
test:
	go test ./... -race -count=1 -p 1

lint:
	go vet ./...
	@fmt_out=$$(gofmt -l server/); \
	if [ -n "$$fmt_out" ]; then \
		echo "gofmt 需要格式化的文件:"; echo "$$fmt_out"; exit 1; \
	fi
	@bash scripts/importlint.sh
	@bash scripts/kinds-registry.sh

# kinds: 协议 kind 单一真源门禁（注册表完整性 + 号段冲突 + v3 号段锁定）
kinds:
	@bash scripts/kinds-registry.sh


# ---------------------------------------------------------------------------
# release: 单二进制（trimpath、去符号）
# ---------------------------------------------------------------------------
release:
	mkdir -p $(DIST_DIR)
	go build -trimpath -ldflags "-s -w" -o $(DIST_DIR)/$(BIN) ./server/cmd/chatd

# ---------------------------------------------------------------------------
# release-all: release + 自包含 tar.gz + SHA256SUMS
#
# 包内布局与 install.sh 的假设一致（REPO_DIR 的上一级即包根）：
#   dist/chatd            install.sh 从 dist/ 取二进制
#   deploy/*              安装脚本 / unit / 配置样例
#   docs/deploy.md        部署手册
# 后端专用包：不含任何前端产物（前端是独立程序，通过 env 指向本服务）。
# ---------------------------------------------------------------------------
release-all: release
	@ver="$$(printf '%s' '$(VERSION)' | tr -c 'A-Za-z0-9._-' '-')"; \
	stage="$(DIST_DIR)/pkg-$$ver"; \
	rm -rf "$$stage"; \
	mkdir -p "$$stage/dist" "$$stage/deploy" "$$stage/docs"; \
	install -m 0755 "$(DIST_DIR)/$(BIN)" "$$stage/dist/$(BIN)"; \
	cp -R deploy/systemd deploy/journald deploy/keygen deploy/lib "$$stage/deploy/"; \
	for f in install.sh install-interactive release.sh backup.sh ufot.toml policy.json Caddyfile logrotate.conf; do \
		cp "deploy/$$f" "$$stage/deploy/$$f"; \
	done; \
	chmod 0755 "$$stage/deploy/install.sh" "$$stage/deploy/release.sh" "$$stage/deploy/backup.sh"; \
	cp docs/deploy.md "$$stage/docs/deploy.md"; \
	cp README.md LICENSE "$$stage/"; \
	tar -czf "$(DIST_DIR)/ufot-$$ver-linux-$(ARCH).tar.gz" -C "$$stage" .; \
	( cd "$(DIST_DIR)" && sha256sum "ufot-$$ver-linux-$(ARCH).tar.gz" > SHA256SUMS ); \
	echo "release artifact: $(DIST_DIR)/ufot-$$ver-linux-$(ARCH).tar.gz"; \
	cat "$(DIST_DIR)/SHA256SUMS"

clean:
	rm -rf $(DIST_DIR) $(GEN_DIR)

# build-pg: chatd-pg 冷库构建变体（OPS-1，1.4 §5 #1）。位于主仓
# server/cmd/chatd-pg，根 go.mod 统一管理 pgx 依赖，CI 的 go build ./...
# 会编译验证它；不影响上面任何现有 target。
.PHONY: build-pg
build-pg:
	go build -o $(DIST_DIR)/chatd-pg ./server/cmd/chatd-pg
	@fmt_out=$$(gofmt -l server/cmd/chatd-pg/); \
	if [ -n "$$fmt_out" ]; then \
		echo "gofmt 需要格式化的文件: $$fmt_out"; exit 1; \
	fi; \
	echo "built $(DIST_DIR)/chatd-pg (pgx/v5/stdlib embedded)"
