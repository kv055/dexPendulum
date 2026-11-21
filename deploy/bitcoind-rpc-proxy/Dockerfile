FROM golang:1.27-alpine AS build
ARG GOPROXY=https://proxy.golang.org|https://goproxy.io|direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download
COPY main.go .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -o /proxy .

FROM alpine:3.24
COPY --from=build /proxy /proxy
USER 65532:65532
ENTRYPOINT ["/proxy"]
