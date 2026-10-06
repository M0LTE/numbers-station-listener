# Single-container build: frontend, then a static Go binary.
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
COPY tests/fixtures/priyom-2026-10-06.json /src/tests/fixtures/
COPY data/stations.json /src/data/
RUN mkdir -p /src/internal/webui/dist && npm run build

FROM golang:1.26-alpine AS go
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/webui/dist ./internal/webui/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/m0lte/numbers-station-listener/internal/config.Version=${VERSION}" -o /out/nsl ./cmd/nsl

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=go /out/nsl /nsl
ENV NSL_LISTEN=:8080 NSL_DATA_DIR=/data
VOLUME /data
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/nsl"]
