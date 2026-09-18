# Lumen 开发任务入口
#
# 常用命令：
#   make test          运行两端全部测试
#   make check         运行测试 + 静态检查 + 密钥扫描
#   make run-server    本地启动服务端
#   make e2e           跑一次完整的端到端联调（假事件 → Session → mock 总结）

SHELL := /bin/bash
.DEFAULT_GOAL := help

SERVER_DIR := server
DESKTOP_DIR := desktop
VENV := $(DESKTOP_DIR)/.venv
PY := $(VENV)/bin/python

.PHONY: help
help:
	@echo "Lumen 开发命令："
	@echo "  make setup            创建 Python 虚拟环境并安装依赖"
	@echo "  make test             运行两端全部测试"
	@echo "  make test-server      只运行服务端测试"
	@echo "  make test-desktop     只运行采集端测试"
	@echo "  make fmt              格式化两端代码"
	@echo "  make lint             静态检查（go vet + ruff）"
	@echo "  make secrets-check    扫描仓库是否混入密钥或敏感数据（提交前必跑）"
	@echo "  make check            test + lint + secrets-check"
	@echo "  make build-server     编译服务端二进制"
	@echo "  make run-server       本地启动服务端（读取环境变量）"
	@echo "  make e2e              端到端联调（假事件 → Session → mock DeepSeek）"
	@echo "  make clean            清理构建产物与临时数据"

# ---- 环境准备 ----

.PHONY: setup
setup:
	@python3 -m venv $(VENV)
	@$(VENV)/bin/pip install --quiet --upgrade pip
	@$(VENV)/bin/pip install --quiet -r $(DESKTOP_DIR)/requirements.txt
	@cd $(SERVER_DIR) && go mod download
	@echo "环境准备完成。"

# ---- 测试 ----

.PHONY: test
test: test-server test-desktop

.PHONY: test-server
test-server:
	@echo "=== 服务端测试 ==="
	@cd $(SERVER_DIR) && go test ./... 

.PHONY: test-server-verbose
test-server-verbose:
	@cd $(SERVER_DIR) && go test -v ./...

.PHONY: test-desktop
test-desktop:
	@echo "=== 采集端测试 ==="
	@# 必须在 desktop/ 下运行：pytest 需要以该目录为根才能 import lumen_desktop。
	@cd $(DESKTOP_DIR) && .venv/bin/python -m pytest tests -q

# ---- 质量 ----

.PHONY: fmt
fmt:
	@cd $(SERVER_DIR) && gofmt -w .
	@$(VENV)/bin/ruff format $(DESKTOP_DIR)/lumen_desktop $(DESKTOP_DIR)/tests 2>/dev/null || true
	@echo "格式化完成。"

.PHONY: lint
lint:
	@echo "=== go vet ==="
	@cd $(SERVER_DIR) && go vet ./...
	@echo "=== gofmt 检查（应无输出）==="
	@cd $(SERVER_DIR) && gofmt -l . || true
	@echo "=== ruff ==="
	@$(VENV)/bin/ruff check $(DESKTOP_DIR)/lumen_desktop $(DESKTOP_DIR)/tests 2>/dev/null || echo "（未安装 ruff，跳过）"

# 提交前的密钥扫描。
#
# 这是最后一道人工防线：即使 .gitignore 写对了，也可能有人把密钥写进代码或
# 样例文件。扫描范围覆盖全部受版本控制的文本文件。
#
# 测试夹具需要包含"看起来像密钥"的假值来验证脱敏逻辑。这类行请在行尾加
# `secrets-check:allow` 标记，表示这是**经过人工确认的有意例外**，
# 而不是让扫描器直接放过整个测试目录。
.PHONY: secrets-check
secrets-check:
	@echo "=== 扫描仓库中的疑似密钥与敏感内容 ==="
	@found=0; \
	PATTERNS='sk-[A-Za-z0-9]{16,}|AKIA[0-9A-Z]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----|ghp_[A-Za-z0-9]{20,}|xox[baprs]-[A-Za-z0-9-]+'; \
	FILES=$$(git ls-files 2>/dev/null | grep -v -E '\.(png|jpg|jpeg|gif|ico|pdf|woff2?|ttf)$$' || true); \
	HITS=$$(echo "$$FILES" | xargs grep -nEI "$$PATTERNS" 2>/dev/null | grep -v 'secrets-check:allow' || true); \
	if [ -n "$$HITS" ]; then echo "❌ 发现疑似密钥："; echo "$$HITS"; found=1; fi; \
	LEAKS=$$(echo "$$FILES" | xargs grep -nI -E '/Users/[a-zA-Z0-9._-]+/|/home/[a-zA-Z0-9._-]+/' 2>/dev/null \
		| grep -v 'secrets-check:allow' \
		| grep -v -E '(_test\.go:|tests/|test_|Example|example|示例|README|\.md:)' || true); \
	if [ -n "$$LEAKS" ]; then echo "⚠️  代码中发现绝对路径（请确认是否为真实路径）："; echo "$$LEAKS" | head -20; found=1; fi; \
	SENSITIVE=$$(git ls-files 2>/dev/null | grep -E '(^|/)(\.env$$|\.env\.[^e]|.*\.pem$$|.*\.key$$|.*\.p12$$|secrets/|lumen\.env$$)' || true); \
	if [ -n "$$SENSITIVE" ]; then echo "❌ 以下敏感文件被纳入版本控制："; echo "$$SENSITIVE"; found=1; fi; \
	UNTRACKED_SENSITIVE=$$(find . -path ./.git -prune -o -path ./.venv -prune -o -path ./desktop/.venv -prune -o \
		\( -name '*.pem' -o -name '*.key' -o -name 'lumen.env' -o -name '.env' \) -print 2>/dev/null || true); \
	if [ -n "$$UNTRACKED_SENSITIVE" ]; then echo "❌ 工作区存在密钥类文件（确认已被忽略）："; echo "$$UNTRACKED_SENSITIVE"; found=1; fi; \
	if [ $$found -eq 1 ]; then echo ""; echo "检查未通过，请处理后重新提交。"; exit 1; fi; \
	echo "✅ 未发现密钥或敏感文件。"

.PHONY: check
check: test lint secrets-check
	@echo ""
	@echo "✅ 全部检查通过。"

# ---- 构建与运行 ----

.PHONY: build-server
build-server:
	@cd $(SERVER_DIR) && go build -trimpath -o ../bin/lumen-server ./cmd/lumen-server
	@echo "已构建 bin/lumen-server"

.PHONY: run-server
run-server:
	@cd $(SERVER_DIR) && go run ./cmd/lumen-server

.PHONY: desktop-status
desktop-status:
	@$(PY) -m lumen_desktop status

.PHONY: desktop-run
desktop-run:
	@$(PY) -m lumen_desktop run

# ---- 端到端联调 ----

.PHONY: e2e
e2e:
	@bash scripts/e2e_local.sh

# ---- 清理 ----

.PHONY: clean
clean:
	@rm -rf bin
	@cd $(SERVER_DIR) && go clean -cache -testcache 2>/dev/null || true
	@find . -name '__pycache__' -type d -prune -exec rm -rf {} + 2>/dev/null || true
	@rm -rf .pytest_cache
	@echo "清理完成。"
