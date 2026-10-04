FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY src ./src
RUN CGO_ENABLED=0 go build -trimpath -o /person-album ./src

FROM alpine:3.22
# The named volume copies this directory's ownership on first mount.
RUN apk add --no-cache tzdata ca-certificates \
    && mkdir -p /state \
    && chown 503:20 /state
COPY --from=build /person-album /usr/local/bin/person-album
USER 503:20
ENTRYPOINT ["person-album"]
