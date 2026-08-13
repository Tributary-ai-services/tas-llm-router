// Swagger UI bootstrap. Previously an inline <script> in serveSwaggerIndex;
// moved to a file because the service sets a strict Content-Security-Policy
// (default-src 'self') that blocks inline scripts.
//
// The spec is referenced by absolute path rather than absolute URL, so the
// page works under any hostname it is published on — llm-router.tas.scharber.com,
// docs.air-ops.net, or a port-forward — with no dependency on forwarded headers.
window.addEventListener('load', function () {
    SwaggerUIBundle({
        url: '/docs/openapi.yaml',
        dom_id: '#swagger-ui',
        deepLinking: true,
        presets: [SwaggerUIBundle.presets.apis],
        plugins: [SwaggerUIBundle.plugins.DownloadUrl],
        layout: 'BaseLayout',
        defaultModelsExpandDepth: 0,
        defaultModelExpandDepth: 3,
        docExpansion: 'list',
        filter: true,
        showRequestHeaders: true,
        supportedSubmitMethods: ['get', 'post', 'put', 'delete', 'patch'],
        validatorUrl: null,
        requestInterceptor: function (request) {
            if (!request.headers['X-API-Key'] && !request.headers['Authorization']) {
                request.headers['X-API-Key'] = 'your-api-key-here';
            }
            return request;
        }
    });
});
