GO      ?= go
VERSION := $(shell cat VERSION)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%d)

BINARY  := replctl
PREFIX  ?= /usr/local
LDFLAGS := -s -w -X github.com/master2sid2/replctl/cmd.version=$(VERSION) -X github.com/master2sid2/replctl/cmd.commit=$(COMMIT) -X github.com/master2sid2/replctl/cmd.date=$(DATE)
BUILD   := CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)"

PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: build install test lint dist clean version help

## build:   compile bin/replctl
build:
	$(BUILD) -o bin/$(BINARY) .

## install: build and copy to $(PREFIX)/bin
install: build
	install -m 0755 bin/$(BINARY) $(PREFIX)/bin/$(BINARY)

## test:    run unit tests
test:
	$(GO) test ./...

## lint:    go vet and gofmt check
lint:
	$(GO) vet ./...
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }

## dist:    release binaries for all platforms in dist/
dist:
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		out=dist/$(BINARY)-v$(VERSION)-$$os-$$arch$$ext; echo $$out; \
		GOOS=$$os GOARCH=$$arch $(BUILD) -o $$out . || exit 1; \
	done

## clean:   remove build output
clean:
	rm -f bin/$(BINARY) dist/$(BINARY)-*

## version: print version info
version:
	@echo "Version : $(VERSION)"
	@echo "Commit  : $(COMMIT)"
	@echo "Date    : $(DATE)"

## help:    list targets
help:
	@grep -E '^## ' Makefile | sed 's/## /  /'
