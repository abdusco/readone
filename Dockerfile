FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/readone .

FROM alpine:3.20
RUN adduser -D -u 10001 readone
WORKDIR /app
COPY --from=build /out/readone ./readone
RUN mkdir -p /app/data && chown -R readone:readone /app/data
USER readone
ENV PORT=8080
ENV DB_PATH=/app/data/data.db
VOLUME ["/app/data"]
EXPOSE 8080
ENTRYPOINT ["./readone"]
