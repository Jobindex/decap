# use golang to build
FROM golang:1.19 as golang

WORKDIR /app

# install deps
COPY go.mod go.sum ./
RUN go mod download

# copy sause
COPY *.go ./
COPY cmd ./cmd

# build
RUN go build ./cmd/...

# use chrome headless for deployment image
FROM chromedp/headless-shell:98.0.4758.102

COPY --from=golang /app/decap /usr/local/bin/decap

ENTRYPOINT [ "decap" ]
