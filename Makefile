VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT   := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE     := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
# ldflags target uses the package name (main) not the full import path for main binaries
LDFLAGS  := -s -w \
	-X main.Version=$(VERSION) \
	-X main.Commit=$(COMMIT) \
	-X main.BuildDate=$(DATE)

.PHONY: build test lint clean release

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o madar ./cmd/madar/

# Cross-compile for EC2 (Linux AMD64)
release:
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o madar-linux-amd64 ./cmd/madar/

test:
	go test ./... -count=1

lint:
	go vet ./...

clean:
	rm -f madar madar-linux-amd64
