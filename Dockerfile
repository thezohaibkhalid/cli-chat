    FROM golang:1.22-alpine AS builder
    WORKDIR /src
    
    COPY server/go.mod server/go.sum ./server/
    COPY video/go.mod video/go.sum ./video/
    RUN --mount=type=cache,target=/go/pkg/mod \
        cd server && go mod download && \
        cd /src/video && go mod download
    
    COPY server ./server
    COPY video ./video
    
    RUN --mount=type=cache,target=/go/pkg/mod \
        CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
        go build -ldflags="-s -w" -o /out/chatserver ./server && \
        CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
        go build -ldflags="-s -w" -o /out/videosignal ./video
    
    FROM alpine:3.20
    RUN apk add --no-cache tini
    
    WORKDIR /app
    COPY --from=builder /out/chatserver /app/chatserver
    COPY --from=builder /out/videosignal /app/videosignal
    
    RUN mkdir -p /data && adduser -D -h /app appuser && \
        chown -R appuser:appuser /app /data
    USER appuser
    
    ENV VIDEO_BASE_URL="http://localhost:5001"
    
    EXPOSE 5000 5001
    
    # Simple entrypoint to run both services
    # - videosignal on :5001
    # - chatserver on :5000 (cwd=/data so chat.db persists)
    # tini makes signal handling clean
    ENTRYPOINT ["/sbin/tini", "--"]
    CMD ["/bin/sh", "-lc", "/app/videosignal & cd /data && VIDEO_BASE_URL=${VIDEO_BASE_URL} /app/chatserver"]
    