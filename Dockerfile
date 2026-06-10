# Builder stage. golang:1.25.0 tag must exactly match the go directive in go.mod.
FROM golang:1.25.0 AS builder
WORKDIR /src

# Cache module downloads separately from the source tree.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# GIT_SHA is the build's version stamp, injected into main.version via ldflags.
# It is NOT a secret, so a build-arg is appropriate. NEVER pass any secret/DSN
# through ARG/build-arg.
ARG GIT_SHA=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
	-ldflags "-s -w -X main.version=${GIT_SHA}" \
	-o /app/server ./cmd/api

# Runtime stage. distroless/static carries no shell and no Go toolchain, so a
# `docker run ... which go` fails (AC14). Nothing secret is copied in.
# The :nonroot variant runs as UID 65532 (not root); 8080 > 1024 so the nonroot
# user can still bind it.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /app/server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
