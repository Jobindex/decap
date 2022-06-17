# use golang to build
FROM golang:1.18 as golang

WORKDIR /app

# copy source
COPY go.mod go.sum *.go ./
COPY cmd ./cmd

RUN go build ./cmd/...

# use chrome headless for deployment image
FROM chromedp/headless-shell:98.0.4758.102

COPY --from=golang /app/decap /usr/local/bin/decap

ENTRYPOINT [ "decap" ]
