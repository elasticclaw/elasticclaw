# Hub development image: Go toolchain + air (hot reload) + docker CLI (for spawning agent containers)
FROM golang:1.25-alpine

RUN apk add --no-cache git ca-certificates tzdata docker-cli

# Install air for hot reload on .go file changes
RUN go install github.com/air-verse/air@v1.61.7

WORKDIR /app
EXPOSE 8080

CMD ["air", "-c", "docker/.air.toml"]
