# syntax=docker/dockerfile:1

# ---- build ----
# Собираем на нативной платформе и кросс-компилируем под целевую:
# так multi-arch (amd64 + arm64 для Apple Silicon) собирается быстро, без эмуляции.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.buildVersion=${VERSION}" -o /out/kube-demo .

# ---- runtime ----
# Alpine, а не distroless/scratch — чтобы на демо можно было сделать
# `kubectl exec -it <pod> -- sh` и показать env, DNS (nslookup), wget соседей.
FROM alpine:3.22
RUN adduser -D -H -u 10001 app
COPY --from=build /out/kube-demo /usr/local/bin/kube-demo
USER 10001
EXPOSE 8080
ENTRYPOINT ["kube-demo"]
