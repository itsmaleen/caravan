.PHONY: build test release clean

build:
	go build -o caravan .

test:
	go test ./...

release:
	bash scripts/release.sh

clean:
	rm -f caravan
	rm -rf dist/
