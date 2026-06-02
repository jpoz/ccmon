BINARY := ccmon
PKG    := .
# Install location. ~/bin matches the convention documented in .gitignore
# (`go build -o ~/bin/ccmon .`). Override with `make install BINDIR=/usr/local/bin`.
PREFIX ?= $(HOME)
BINDIR ?= $(PREFIX)/bin

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make \033[36m<target>\033[0m\n"} \
		/^##@/ { printf "\n\033[1;33m%s\033[0m\n", substr($$0, 5); next } \
		/^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-12s\033[0m \033[90m%s\033[0m\n", $$1, $$2 }' $(MAKEFILE_LIST)

##@ Build
.PHONY: build
build: ## Compile the ccmon binary into ./bin
	go build -o ./bin/$(BINARY) $(PKG)

.PHONY: clean
clean: ## Remove built binaries
	rm -rf ./bin

##@ Install
.PHONY: install
install: build ## Build ccmon and install it to BINDIR (default ~/bin)
	@install -d "$(BINDIR)"
	install -m 0755 ./bin/$(BINARY) "$(BINDIR)/$(BINARY)"
	@echo "Installed $(BINARY) -> $(BINDIR)/$(BINARY)"
	@echo "Next: run '$(BINARY) install' to wire ccmon into Claude Code + Codex"

.PHONY: uninstall
uninstall: ## Remove ccmon's hook wiring, then delete the installed binary
	-"$(BINDIR)/$(BINARY)" uninstall
	rm -f "$(BINDIR)/$(BINARY)"
	@echo "Removed $(BINDIR)/$(BINARY)"

##@ Local Run
.PHONY: run
run: ## Run the ccmon TUI
	go run $(PKG) tui

##@ Quality
.PHONY: test
test: ## Run tests
	go test ./...

.PHONY: vet
vet: ## Run go vet over all packages
	go vet ./...

.PHONY: fmt
fmt: ## Format all Go source
	gofmt -w .

.PHONY: tidy
tidy: ## Tidy go.mod
	go mod tidy
