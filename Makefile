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
	helm lint charts/anchor --set aws.region=eu-west-1
	helm template anchor charts/anchor --namespace anchor-system --kube-version 1.34.0 \
		--set aws.region=eu-west-1 >/dev/null

check: test-race vet helm

image:
	docker build --platform linux/amd64 --build-arg VERSION=$(VERSION) -t $(IMAGE) .
