EXIFTOOL_VERSION := 13.55
EXIFTOOL_SHA256 := 5f4c81d34ad406538c2871ad72dbfceb5d9b412b2f16cbbeb4d712d270846667
EXIFTOOL_ARCHIVE := .exiftool/Image-ExifTool-$(EXIFTOOL_VERSION).tar.gz
EXIFTOOL_URL := https://cpan.metacpan.org/authors/id/E/EX/EXIFTOOL/Image-ExifTool-$(EXIFTOOL_VERSION).tar.gz
EXIFTOOL_BIN := $(CURDIR)/.exiftool/Image-ExifTool-$(EXIFTOOL_VERSION)/exiftool
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: build run test test-integration exiftool

build:
	go build -ldflags="$(LDFLAGS)" -o exiftool-server ./cmd/exiftool-server

run: exiftool
	EXIFTOOL_PATH=$(EXIFTOOL_BIN) go run ./cmd/exiftool-server

test:
	go test ./...

test-integration: exiftool
	EXIFTOOL_PATH=$(EXIFTOOL_BIN) EXIFTOOL_VERSION=$(EXIFTOOL_VERSION) go test -race -tags=integration ./...

exiftool: $(EXIFTOOL_BIN)

$(EXIFTOOL_BIN):
	@mkdir -p .exiftool
	curl -fL --retry 3 -o "$(EXIFTOOL_ARCHIVE)" "$(EXIFTOOL_URL)"
	@echo "$(EXIFTOOL_SHA256)  $(EXIFTOOL_ARCHIVE)" | shasum -a 256 -c
	tar -xzf "$(EXIFTOOL_ARCHIVE)" -C .exiftool
