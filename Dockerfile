# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -o /out/readone .

FROM alpine:3.20
WORKDIR /app
COPY --from=build /out/readone ./readone
ENV PORT=8080
ENV DB_PATH=/app/data/data.db
VOLUME ["/app/data"]
EXPOSE 8080
ENTRYPOINT ["./readone"]
