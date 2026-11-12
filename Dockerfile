# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fdrchat ./cmd/fdrchat

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/fdrchat /fdrchat
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/fdrchat"]
