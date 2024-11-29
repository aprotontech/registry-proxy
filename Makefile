

all: bin

bin:
	@mkdir -p bin
	go build -o bin/rproxy ./cmd

image: bin
	docker build -t localhost.io/library/registry-proxy:latest .

.PHONY: all bin image

