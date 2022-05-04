FROM chromedp/headless-shell:98.0.4758.102

ENV PORT 4531

COPY cmd/decap/decap /usr/local/bin/decap

WORKDIR /

ENTRYPOINT [ "decap" ]
