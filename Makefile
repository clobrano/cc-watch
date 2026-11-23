# Common tasks; `make help` lists them.

BINARY := cc-watch

.DEFAULT_GOAL := build
.PHONY: build install test demo clean help

build: ## Build ./cc-watch
	go build -o $(BINARY) .

install: ## Install cc-watch into $GOBIN (default ~/go/bin)
	go install .

test: ## Vet and run the tests
	go vet ./...
	go test ./...

demo: ## Record demo/cc-watch.gif and demo/statusbar.gif (needs vhs, tmux, go and ffmpeg)
	@command -v vhs >/dev/null || { echo "vhs not found: see https://github.com/charmbracelet/vhs#installation" >&2; exit 1; }
	vhs demo/demo.tape
	demo/statusbar.sh

clean: ## Remove the built binary
	rm -f $(BINARY)

help: ## List these targets
	@grep -E '^[a-z]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "} {printf "  %-8s %s\n", $$1, $$2}'
