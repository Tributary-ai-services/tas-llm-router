package metrics

import (
	"net/http"
	"strconv"
	"time"
)

// statusRecorder captures the status code and whether anything was written,
// because net/http reports neither after the fact.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.written {
		r.status = code
		r.written = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	// A handler that writes without calling WriteHeader has implicitly sent
	// 200. Recording that here keeps streaming responses — which write the
	// first chunk directly — from being counted as status 0.
	if !r.written {
		r.status = http.StatusOK
		r.written = true
	}
	return r.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer so server-sent-event streaming keeps
// working. Without this the wrapper silently breaks streaming, since the
// handler's type assertion to http.Flusher would fail against the wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// providerFromResponse reads the provider the router actually used. The header
// is set on the way out, so it is only available after the handler returns —
// which is why the label is resolved at that point rather than up front.
//
// "none" means routing never selected a provider: an auth rejection, a
// validation failure, or a policy block. Recording those as a provider's
// traffic would make an auth outage look like a vendor problem.
const providerHeader = "X-TAS-Router-Provider"

// Middleware records request count, latency, and in-flight depth for one route.
//
// It wraps only the completion handlers. Management, health, and /metrics
// itself are excluded deliberately: scraping every fifteen seconds would
// otherwise dominate the request counter and make the rate meaningless.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		InFlightRequests.Inc()
		defer InFlightRequests.Dec()

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()

		next.ServeHTTP(rec, r)

		provider := rec.Header().Get(providerHeader)
		if provider == "" {
			provider = "none"
		}

		RequestsTotal.WithLabelValues(provider, r.Method, strconv.Itoa(rec.status)).Inc()
		RequestDurationSeconds.WithLabelValues(provider, r.Method).Observe(time.Since(start).Seconds())
	})
}
