FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/qarznoma ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/qarznoma /qarznoma
COPY web /web
ENV HTTP_ADDR=:8080 WEB_DIR=/web
EXPOSE 8080
ENTRYPOINT ["/qarznoma"]
