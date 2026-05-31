BINARY  = bin/dellipmifanctl

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.1.0")
LDFLAGS  = -ldflags "-s -w -X main.version=$(VERSION)"

.PHONY: build clean

build:
	go build -trimpath $(LDFLAGS) -o $(BINARY) .

clean:
	rm -f $(BINARY)
