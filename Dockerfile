FROM golang:1.24-bookworm AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -ldflags "-s -w -X example.com/anchor/internal/version.Version=${VERSION}" \
    -o /out/anchor ./cmd/anchor

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/anchor /anchor
USER 65532:65532
ENTRYPOINT ["/anchor"]
