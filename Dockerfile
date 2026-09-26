# Build stage
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOARCH=arm64 go build -ldflags="-s -w" -o /quirkproxy .

# Runtime stage
FROM alpine:3
RUN apk add --no-cache ca-certificates && adduser -D quirkproxy
WORKDIR /app
COPY --from=build /quirkproxy /app/quirkproxy
COPY config.docker.json /app/config.json
USER quirkproxy
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s CMD wget --spider -q http://127.0.0.1:8080/healthz || exit 1
CMD ["/app/quirkproxy", "-config", "/app/config.json"]
