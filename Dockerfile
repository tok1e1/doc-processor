# syntax=docker/dockerfile:1

FROM golang:1.23-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker

# The documents volume inherits ownership from this directory on first mount.
RUN mkdir -p /out/data/documents

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /out/worker /app/
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV STORAGE_DIR=/data/documents
USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/app/api"]
