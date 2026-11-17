GO ?= go
OUT ?= bin/atg-control
GOFILES := $(shell $(GO) list -f '{{.Dir}}/*.go' ./...)

.PHONY: check fmt test race vet build package clean

check: fmt test vet build package

fmt:
	test -z "$$($(GO)fmt -l $(GOFILES))"

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

build:
	$(GO) build -trimpath -o $(OUT) ./cmd/atg-control

package: build
	mkdir -p dist
	tar -czf dist/atg-control.tar.gz -C bin atg-control

clean:
	rm -rf bin dist
