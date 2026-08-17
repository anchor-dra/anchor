.PHONY: build test test-race vet check helm image

VERSION ?= 0.1.0
IMAGE ?= anchor:$(VERSION)

build:
	go build -trimpath -o bin/anchor ./cmd/anchor

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

helm:
	helm lint charts/anchor
	helm template anchor charts/anchor --namespace anchor-system >/dev/null

check: test-race vet helm

image:
	docker build --platform linux/amd64 --build-arg VERSION=$(VERSION) -t $(IMAGE) .
