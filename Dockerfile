# 多阶段构建：TARGETOS/TARGETARCH 由 buildx 注入，支持多架构镜像。
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${DATE}" \
    -o /out/skydroid-onvif-adapter ./cmd/skydroid-onvif-adapter

# 运行镜像：ffmpeg 用于快照抓帧。
FROM alpine:3.20
RUN apk add --no-cache ffmpeg ca-certificates tzdata
COPY --from=build /out/skydroid-onvif-adapter /usr/local/bin/skydroid-onvif-adapter
EXPOSE 8080
ENTRYPOINT ["skydroid-onvif-adapter"]
