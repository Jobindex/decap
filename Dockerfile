FROM chromedp/headless-shell:90.0.4430.212

ENV PORT 4531

COPY decap /usr/local/bin/decap

WORKDIR /

ENTRYPOINT [ "decap" ]
