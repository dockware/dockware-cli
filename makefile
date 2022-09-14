#
# Makefile
#

.PHONY: help build
.DEFAULT_GOAL := help


help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'

# ----------------------------------------------------------------------------------------------------------------

install: ## Installs all dependencies
	go mod download

build: ## Builds the binary
	go build -o build/dockware