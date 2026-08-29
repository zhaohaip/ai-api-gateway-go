.DEFAULT_GOAL := help

APP_NAME ?= ai-api-gateway-go
MAIN_PACKAGE ?= ./cmd/gateway
BIN_DIR ?= bin
SPEC_REPO ?= git@github.com:zhaohaip/coding-prompt.git
SPEC_DIR ?= coding-prompt
GO ?= go

.PHONY: help build run test sync-spec

help:
	@printf '%-14s %s\n' \
		'help' '显示可用的 Make 命令' \
		'build' '编译主程序到 bin 目录' \
		'run' '编译并运行主程序，继承当前终端环境变量' \
		'test' '运行全部 Go 测试' \
		'sync-spec' '克隆或快进更新统一编码规范仓库'

build:
	@mkdir -p "$(BIN_DIR)"
	@GOTOOLCHAIN=local GOPROXY=off $(GO) build -o "$(BIN_DIR)/$(APP_NAME)" "$(MAIN_PACKAGE)"

run: build
	@"$(BIN_DIR)/$(APP_NAME)"

test:
	@$(GO) test ./...

sync-spec:
	@set -eu; \
	if [ ! -e "$(SPEC_DIR)" ] && [ ! -L "$(SPEC_DIR)" ]; then \
		git clone "$(SPEC_REPO)" "$(SPEC_DIR)"; \
	elif [ -d "$(SPEC_DIR)" ] && [ -e "$(SPEC_DIR)/.git" ] && \
		git -C "$(SPEC_DIR)" rev-parse --is-inside-work-tree >/dev/null 2>&1; then \
		actual_repo=$$(git -C "$(SPEC_DIR)" remote get-url origin) || { \
			echo "错误：无法读取 $(SPEC_DIR) 的 origin 仓库地址" >&2; \
			exit 1; \
		}; \
		if [ "$$actual_repo" != "$(SPEC_REPO)" ]; then \
			echo "错误：$(SPEC_DIR) 的 origin 为 $$actual_repo，不是 $(SPEC_REPO)" >&2; \
			exit 1; \
		fi; \
		git -C "$(SPEC_DIR)" pull --ff-only; \
	else \
		echo "错误：目标路径 $(SPEC_DIR) 已存在，但不是 Git 仓库" >&2; \
		exit 1; \
	fi
