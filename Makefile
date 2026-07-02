.PHONY: build install test release clean

# Install prefix; binary lands in $(PREFIX)/bin. Override: make install PREFIX=/usr/local
PREFIX ?= $(HOME)/.local

build:
	go build -o caravan .

install:
	mkdir -p $(PREFIX)/bin
	go build -o $(PREFIX)/bin/caravan .
	@echo "installed $$($(PREFIX)/bin/caravan version) -> $(PREFIX)/bin/caravan"
	@case ":$$PATH:" in *":$(PREFIX)/bin:"*) ;; *) echo "note: $(PREFIX)/bin is not on your PATH";; esac

test:
	go test ./...

release:
	bash scripts/release.sh

clean:
	rm -f caravan
	rm -rf dist/
