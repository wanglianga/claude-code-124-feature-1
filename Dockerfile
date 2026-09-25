# syntax=docker/dockerfile:1

# ---- 构建阶段 ----
FROM golang:1.26-alpine AS build
WORKDIR /src
ENV GOTOOLCHAIN=local \
    GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct \
    CGO_ENABLED=0
COPY go.mod ./
RUN go mod download
COPY . .
RUN go mod tidy && go build -ldflags="-s -w" -o /out/server .

# ---- 运行阶段 ----
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata wget \
    && adduser -D -u 10001 appuser
WORKDIR /app
COPY --from=build /out/server /app/server
USER appuser
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=3s --start-period=20s --retries=5 \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null 2>&1 || exit 1
CMD ["/app/server"]
