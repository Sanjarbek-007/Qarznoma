# The build stage runs natively on the build machine and cross-compiles Go for the target
# (linux/amd64 or linux/arm64), so multi-arch builds need no emulation.
FROM --platform=$BUILDPLATFORM golang:1.26.11-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/qarznoma ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/qarznoma /qarznoma
COPY web /web
ENV HTTP_ADDR=:8080 WEB_DIR=/web
EXPOSE 8080
ENTRYPOINT ["/qarznoma"]
