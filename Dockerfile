FROM golang:1.19.3-alpine3.16@sha256:8558ae624304387d18694b9ea065cc9813dd4f7f9bd5073edb237541f2d0561b AS build

WORKDIR /go/src/github.com/StackExchange/dnscontrol

ARG BUILD_VERSION

ENV GO111MODULE on

COPY . .

# build dnscontrol
RUN apk update \
    && apk add --no-cache ca-certificates curl gcc build-base git \
    && update-ca-certificates \
    && go build -v -trimpath -buildmode=pie -ldflags="-s -w -X main.SHA=${BUILD_VERSION}"

# Validation check
RUN cp dnscontrol /go/bin/dnscontrol
RUN dnscontrol version

# -----

FROM alpine:3.16.0@sha256:686d8c9dfa6f3ccfc8230bc3178d23f84eeaf7e457f36f271ab1acc53015037c

COPY --from=build /etc/ssl/certs /etc/ssl/certs
COPY --from=build /go/bin/dnscontrol /usr/local/bin

WORKDIR /dns

ENTRYPOINT ["/usr/local/bin/dnscontrol"]
