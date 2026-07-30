# syntax=docker/dockerfile:1
# Pin the build stage to the host (build) platform and cross-compile via
# GOOS/GOARCH instead of letting buildx run this stage under QEMU for the
# arm64 target — Go's compiler cross-compiles natively, so this avoids
# emulating the entire toolchain, which was ~7x slower.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
ARG TARGETOS
ARG TARGETARCH
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/readone .

FROM alpine:3.20
WORKDIR /app
COPY --from=build /out/readone ./readone
ENV PORT=8080
ENV DB_PATH=/app/data/data.db
VOLUME ["/app/data"]
EXPOSE 8080
ENTRYPOINT ["./readone"]
