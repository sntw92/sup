.PHONY: all build dist test install clean

all:
	@echo "build         - Build sup"
	@echo "test          - Run tests"
	@echo "clean         - Clean up"

build:
	@mkdir -p ./bin
	@rm -f ./bin/*
	GO111MODULE=on go build -o ./bin/sup ./cmd/sup

test:
	GO111MODULE=on go test ./...

clean:
	@rm -rf ./bin
