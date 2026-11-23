# syntax=docker/dockerfile:1
# K Garden platform: the Go services (mes, wms-bridge, cerp-bridge) and the floor tablet UI.
#   target "services" -> one image, the command picks the service:  mes serve | wms-bridge migrate | ...
#   target "floor"    -> nginx serving the tablet UI, /api proxied to the MES
# Build context: the repository root.

FROM golang:1.27-alpine AS go
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal internal
COPY services services
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./services/...

FROM oven/bun:1 AS ui
WORKDIR /src/apps/floor
COPY apps/floor/package.json apps/floor/bun.lock apps/floor/.npmrc ./
RUN bun install --frozen-lockfile
COPY apps/floor ./
RUN bun run build

FROM alpine:3.22 AS services
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 kgarden
COPY --from=go /out/ /usr/local/bin/
USER kgarden
ENV TZ=Asia/Bangkok

FROM nginx:1.29-alpine AS floor
COPY deploy/platform/floor.nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=ui /src/apps/floor/build /usr/share/nginx/html
