# use golang to build
FROM golang:1.22-bullseye as golang

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
FROM chromedp/headless-shell:125.0.6422.60

COPY --from=golang /app/decap /usr/local/bin/decap

ENTRYPOINT [ "decap" ]
