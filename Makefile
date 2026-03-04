.PHONY: build install clean download-binary run help

VERSION ?= 0.1.0
BINARY_NAME = vibeproxy
BUILD_DIR = .build
DATA_DIR = $(HOME)/.local/share/vibeproxy

help: ## Show this help
	@echo "VibeProxy Linux"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

build: ## Build the vibeproxy binary
	@echo "🔨 Building vibeproxy..."
	@mkdir -p $(BUILD_DIR)
	@cd . && go build -ldflags="-s -w -X main.version=$(VERSION)" -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/vibeproxy
	@echo "✅ Built: $(BUILD_DIR)/$(BINARY_NAME)"

install: build ## Install vibeproxy to ~/.local/bin
	@echo "📲 Installing..."
	@mkdir -p $(HOME)/.local/bin
	@cp $(BUILD_DIR)/$(BINARY_NAME) $(HOME)/.local/bin/$(BINARY_NAME)
	@chmod +x $(HOME)/.local/bin/$(BINARY_NAME)
	@echo "✅ Installed to $(HOME)/.local/bin/$(BINARY_NAME)"

download-binary: ## Download latest CLIProxyAPIPlus binary
	@echo "📦 Downloading CLIProxyAPIPlus..."
	@chmod +x scripts/download-binary.sh
	@./scripts/download-binary.sh $(DATA_DIR)

setup: download-binary install ## Full setup: download binary + install vibeproxy
	@echo ""
	@echo "✅ Setup complete! Run: vibeproxy start"

run: build ## Build and run
	@$(BUILD_DIR)/$(BINARY_NAME) start

clean: ## Clean build artifacts
	@rm -rf $(BUILD_DIR)
	@echo "✅ Clean"

deps: ## Download Go dependencies
	@go mod tidy
	@echo "✅ Dependencies ready"
