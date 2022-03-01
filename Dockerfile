FROM chromedp/headless-shell:98.0.4758.102

ENV PORT 4531

COPY decap /usr/local/bin/decap

WORKDIR /

ENTRYPOINT [ "decap" ]
