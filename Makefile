BIN      := bin
DATA     := data
AGENTS   := $(DATA)/agents
# CGO_ENABLED=0 让两个二进制都是静态的：探针丢到任何 Linux 上都能跑，
# 不用管目标机器的 glibc 版本。SQLite 用的是纯 Go 驱动，所以关掉 CGO 没有副作用。
GOFLAGS  := CGO_ENABLED=0
LDFLAGS  := -s -w

AGENT_ARCHES := amd64 arm64 arm 386

.PHONY: all
all: build

## build: 编译面板和本机架构的探针
.PHONY: build
build: panel agents

## panel: 只编译面板
.PHONY: panel
panel:
	@mkdir -p $(BIN)
	$(GOFLAGS) go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/panel ./cmd/panel
	@echo "→ $(BIN)/panel"

## agents: 交叉编译所有架构的探针，放进面板的数据目录供一键安装脚本下载
.PHONY: agents
agents:
	@mkdir -p $(AGENTS) $(BIN)
	@for arch in $(AGENT_ARCHES); do \
		echo "  编译探针 linux/$$arch"; \
		$(GOFLAGS) GOOS=linux GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o $(AGENTS)/vps-agent-linux-$$arch ./cmd/agent || exit 1; \
	done
	@cp $(AGENTS)/vps-agent-linux-$$(go env GOARCH) $(BIN)/vps-agent 2>/dev/null || true
	@echo "→ $(AGENTS)/"

## test: 跑全部单元测试
.PHONY: test
test:
	go test ./... -count=1

## test-race: 带竞态检测跑测试
.PHONY: test-race
test-race:
	go test ./... -race -count=1

## vet: 静态检查
.PHONY: vet
vet:
	go vet ./...

## run: 本地起面板（调试用，监听 127.0.0.1:8080）
.PHONY: run
run: panel agents
	$(BIN)/panel --listen 127.0.0.1:8080 --data-dir ./$(DATA) --log-level debug

## e2e: 本地端到端验收（起面板 + 两个伪造流量的探针，验证重启不丢账、超额触发）
.PHONY: e2e
e2e: build
	./scripts/e2e.sh

## clean: 清掉编译产物（保留数据库）
.PHONY: clean
clean:
	rm -rf $(BIN) $(AGENTS)

## help: 列出所有目标
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
