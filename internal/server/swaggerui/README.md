# Vendored Swagger UI assets

`swagger-ui.css` and `swagger-ui-bundle.js` are unmodified files from
[swagger-ui-dist](https://www.npmjs.com/package/swagger-ui-dist) **5.9.0**,
the version the page previously loaded from unpkg.

They are vendored and embedded rather than pulled from a CDN because the
service sets `Content-Security-Policy: default-src 'self'`
(`internal/middleware/security.go`), which blocks third-party script and style
origins. Serving them from the binary keeps that policy intact — no
`unsafe-inline`, no external origin — and removes a public page's runtime
dependency on unpkg being up.

`swagger-ui-standalone-preset.js` is deliberately absent: the page uses
`BaseLayout`, and the standalone preset only adds the topbar, which the custom
CSS hid anyway.

`custom.css` and `init.js` are ours, extracted from what used to be inline
`<style>` and `<script>` blocks in `serveSwaggerIndex`.

## Upgrading

```
cd internal/server/swaggerui
for f in swagger-ui.css swagger-ui-bundle.js; do
  curl -sfL -o "$f" "https://unpkg.com/swagger-ui-dist@<version>/$f"
done
```

Then update the version above and re-run `go test ./internal/server/`.

| file | 5.9.0 size | sha256 (first 16) |
| --- | --- | --- |
| `swagger-ui.css` | 151211 | `c24ecffd63fc797d` |
| `swagger-ui-bundle.js` | 1385226 | `2a556306524bed2c` |
