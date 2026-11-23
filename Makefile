GO      ?= $(shell command -v go 2>/dev/null || echo /usr/local/go/bin/go)
VERSION ?= 2.0.0
GOFLAGS  = -trimpath
LDFLAGS  = -s -w -X main.version=$(VERSION)

.PHONY: all build build-arm64 test race vet tidy clean run dry testmail faketest install-service

all: build

build:
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/watchd ./cmd/watchd/
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/watch  ./cmd/watch/

# 在 x86 电脑上给 64 位树莓派交叉编译
build-arm64:
	GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/arm64/watchd ./cmd/watchd/
	GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/arm64/watch  ./cmd/watch/

test:
	$(GO) test ./...

race:
	$(GO) test -race -count=1 ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

run: build
	./bin/watchd

# 只打印将要发送的邮件，不真发（调阈值用）
dry: build
	./bin/watchd -dry-run

testmail: build
	./bin/watchd -test-mail

# 无外网也能看数据流：起假 Binance（REST+WS）→ 指向它的 watchd（dry-run）→
# 喂一条 near 规则 → 等它真实触发并打出 dry-run 邮件 → 自动收尾。
# 用法：make faketest  （需要本机有 go；端口 9444 被占用时先停掉）
faketest: build
	./scripts/faketest.sh

install-service: build
	sudo install -m 0644 deploy/binance-watch.service /etc/systemd/system/binance-watch.service
	sudo systemctl daemon-reload
	sudo systemctl enable --now binance-watch
	@echo "已安装并启动。查看日志: journalctl -u binance-watch -f"

clean:
	rm -rf bin
