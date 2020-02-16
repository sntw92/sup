.PHONY: all build dist test install clean

all:
	@echo "build         - Build sup"
	@echo "test          - Run tests"
	@echo "install       - Install binary"
	@echo "clean         - Clean up"

build:
	@mkdir -p ./bin
	@rm -f ./bin/*
	go build -o ./bin/sup ./cmd/sup

test:
	go test ./...

install:
	go install ./cmd/sup

clean:
	@rm -rf ./bin
