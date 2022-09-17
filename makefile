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
	env GOOS=linux GOARCH=amd64 go build -o ./build/linux/amd64/dockware-cli .
	env GOOS=linux GOARCH=386 go build -o ./build/linux/386/dockware-cli .
	env GOOS=linux GOARCH=arm64 go build -o ./build/linux/arm64/dockware-cli .
	env GOOS=linux GOARCH=arm go build -o ./build/linux/arm/dockware-cli .
	env GOOS=darwin GOARCH=arm64 go build -o ./build/mac/arm64/dockware-cli .
	env GOOS=darwin GOARCH=amd64 go build -o ./build/mac/amd64/dockware-cli .
	env GOOS=windows GOARCH=amd64 go build -o ./build/windows/amd64/dockware-cli.exe .
	env GOOS=windows GOARCH=386 go build -o ./build/windows/386/dockware-cli.exe .