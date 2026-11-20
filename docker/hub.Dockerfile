# Hub development image: Go toolchain + air (hot reload) + docker CLI (for spawning agent containers)
FROM golang:1.25-alpine@sha256:1ae0735f00daffa3aaf1363a5184c0d2dc55c78e3db4ec70241cdac97bf84b59

RUN apk add --no-cache git ca-certificates tzdata docker-cli

# Install air for hot reload on .go file changes
RUN go install github.com/air-verse/air@v1.61.7

WORKDIR /app
EXPOSE 8080

CMD ["air", "-c", "docker/.air.toml"]
