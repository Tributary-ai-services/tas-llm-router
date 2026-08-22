package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"

	"github.com/Tributary-ai-services/Gatekeeper/pkg/scan"
	"github.com/redis/go-redis/v9"
	"github.com/tributary-ai/llm-router-waf/internal/gatekeeper"
	"github.com/tributary-ai/llm-router-waf/internal/middleware"
	"github.com/tributary-ai/llm-router-waf/internal/providers"
	"github.com/tributary-ai/llm-router-waf/internal/routing"
	"github.com/tributary-ai/llm-router-waf/internal/security"
	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/internal/upstreamkey"
	"github.com/tributary-ai/llm-router-waf/internal/workflow"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/affinity"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/breaker"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/cacheconfig"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/credentials"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/events"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/experiments"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/linkage"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/metrics"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/policy"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/promptcache"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/responsecache"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/semcache"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/tokens"
	"github.com/tributary-ai/llm-router-waf/pkg/clear"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server represents the HTTP server
type Server struct {
	// affinity keeps a conversation on the provider whose vendor prompt cache
	// is warm (routing-decision.md §5.5). nil = disabled.
	affinity             *affinity.Manager
	router               *routing.Router
	httpServer           *http.Server
	logger               *logrus.Logger
	config               *ServerConfig
	securityMiddleware   *middleware.SecurityMiddleware
	validationMiddleware *middleware.ValidationMiddleware
	gatekeeper           *gatekeeper.Client
	gatekeeperConfig     *gatekeeper.Config
	// extractionApplyDisabled is the global break-glass kill-switch for
	// active payload reduction (Plan #7 Phase 4). When true the gateway
	// never APPLIES a reduction — active bundles downgrade to shadow
	// measurement — without an image rebuild. Set from AIQG_EXTRACTION_APPLY_DISABLED.
	extractionApplyDisabled bool
	bypassManager           *gatekeeper.BypassManager
	bypassHandler           *gatekeeper.BypassHandler

	// AIQG ingress wire-up. aiqgMiddleware is nil when AIQG is disabled
	// in config, in which case completion handlers register unwrapped
	// and there is zero behavior change for existing traffic.
	aiqgMiddleware func(http.Handler) http.Handler
	aiqgEmitter    events.Emitter
	// judge runs the LLM-as-judge quality layer off the hot path. Nil when
	// judging is disabled.
	judge *judgeRunner

	// respCache is the C1 exact-match response cache (docs/AIQG-CACHING.md).
	// Nil when disabled — every cache branch is a no-op in that case.
	respCache    responsecache.Cache
	respCacheCfg responsecache.Config

	// semCache is the C4 semantic response cache (docs/AIQG-SEMANTIC-CACHING.md).
	// Nil when disabled. Runs on the C1 exact-miss path (L0→L1). In shadow mode
	// it logs would-hits and serves nothing.
	semCache *semcache.Cache
	// semJudge is the C4 L3 async judge (§5, §14.1). Nil unless judging is enabled.
	// Off the hot path: the shadow lookup enqueues sampled near-misses/would-hits
	// and a background worker grades them → labeled pairs + sampled FPR.
	semJudge    *semcache.Loop
	semJudgeCfg semcache.SampleConfig

	// cacheCfgResolver resolves per-tenant cache overrides from aiqg-dashboard-be
	// (docs/AIQG-SEMANTIC-CACHING.md §3, §10). Nil when the dashboard isn't wired,
	// in which case every tenant uses the global defaults below. The resolver is
	// the safe per-tenant enablement path for semantic SERVING.
	cacheCfgResolver *cacheconfig.Resolver
	// credResolver resolves a tenant's stored BYOK vendor key (Plan #14) from
	// aiqg-dashboard-be. Nil when the dashboard isn't wired → BYOK is off and
	// traffic uses per-request / TAS-configured keys exactly as before.
	credResolver *credentials.Resolver
	// semGlobal* are the gateway's global C4 defaults; per-tenant overrides layer
	// over these (nil override field = the global value).
	semGlobalEnabled bool
	semGlobalShadow  bool
	semGlobalMinSim  float64
}

// ServerConfig holds server configuration
type ServerConfig struct {
	Port           string                               `yaml:"port"`
	ReadTimeout    time.Duration                        `yaml:"read_timeout"`
	WriteTimeout   time.Duration                        `yaml:"write_timeout"`
	MaxHeaderBytes int                                  `yaml:"max_header_bytes"`
	Security       *middleware.SecurityMiddlewareConfig `yaml:"security"`
	Validation     *middleware.ValidationConfig         `yaml:"validation"`
	AIQG           *AIQGServerConfig                    `yaml:"aiqg"`
}

// aiqgMetricsAdapter implements events.EmitMetricsRecorder by
// delegating to pkg/aiqg/metrics. Defined here (in internal/server)
// because pkg/aiqg/events deliberately avoids importing prometheus
// directly — this is the seam where the dep crosses.
type aiqgMetricsAdapter struct{}

func (aiqgMetricsAdapter) ObserveEmit(emitter string, start time.Time, errPtr *error) {
	metrics.ObserveEmit(emitter, start, errPtr)
}

func (aiqgMetricsAdapter) RecordEvent(resp events.ResponseEnvelope) {
	metrics.RecordEvent(resp)
}

// buildAIQGEmitter constructs the Emitter per the configured type.
//
//	"" or "log" → LogEmitter (default; no infra required)
//	"kafka"     → KafkaEmitter only
//	"both"      → MultiEmitter fanning to log + kafka
//
// Unknown values fall back to log with a warning so a typo doesn't
// crash the deployment.
func buildAIQGEmitter(cfg *AIQGServerConfig, logger *logrus.Logger) (events.Emitter, error) {
	logEmitter := &events.LogEmitter{Logger: logger}

	switch cfg.EmitterType {
	case "", "log":
		return logEmitter, nil

	case "kafka":
		ke, err := events.NewKafkaEmitter(cfg.Kafka.Brokers, cfg.Kafka.Topic)
		if err != nil {
			return nil, err
		}
		logger.WithFields(logrus.Fields{
			"brokers": cfg.Kafka.Brokers,
			"topic":   cfg.Kafka.Topic,
		}).Info("AIQG Kafka emitter wired")
		return ke, nil

	case "both":
		ke, err := events.NewKafkaEmitter(cfg.Kafka.Brokers, cfg.Kafka.Topic)
		if err != nil {
			return nil, err
		}
		logger.WithFields(logrus.Fields{
			"brokers": cfg.Kafka.Brokers,
			"topic":   cfg.Kafka.Topic,
		}).Info("AIQG MultiEmitter (log + kafka) wired")
		return &events.MultiEmitter{Emitters: []events.Emitter{logEmitter, ke}}, nil

	default:
		logger.WithField("emitter_type", cfg.EmitterType).
			Warn("unknown AIQG emitter_type; falling back to log emitter")
		return logEmitter, nil
	}
}

// AIQGServerConfig configures the AIQG ingress wire-up.
//
// In permissive mode (Strict=false), the middleware mounts on the three
// completion endpoints (/chat/completions, /completions, /messages) and
// only activates when an inbound request carries TAS-Auth. Internal
// callers without TAS-Auth pass through untouched — zero behavior change
// for today's traffic.
//
// In strict mode (Strict=true), the same routes return 401 to any
// request missing TAS-Auth or Authorization. Use for a customer-facing
// ingress (`gateway.aiqg.tas.io`) per build-vs-reuse §7.3.
type AIQGServerConfig struct {
	Enabled bool   `yaml:"enabled"`
	Strict  bool   `yaml:"strict"`
	Region  string `yaml:"region"`

	// IPCaptureMode gates client-IP recording on events: "off",
	// "minimized" (truncated prefix; default when empty), "full" (raw).
	IPCaptureMode string `yaml:"ip_capture_mode"`

	// Tokens is the MVP in-memory token store. Used as a fallback
	// when DashboardURL is unset OR when the dashboard-be is
	// degraded at startup. Empty/omitted means no MapResolver is
	// built — events emit with empty tenant fields per the original
	// incremental-rollout path.
	Tokens []tokens.ConfigToken `yaml:"tokens"`

	// Dashboard backend integration. When DashboardURL +
	// DashboardInternalAuthToken are both set, the AIQG middleware
	// uses a DashboardResolver (HTTP client of
	// POST /internal/auth/validate) instead of the in-memory
	// MapResolver. This is the production path; the Secret-mounted
	// Tokens list becomes a bootstrap fallback only.
	DashboardURL               string `yaml:"dashboard_url"`
	DashboardInternalAuthToken string `yaml:"dashboard_internal_auth_token"`

	// LinkageRedisURL enables the deterministic `linked`-tier attribution
	// (tool_call_id echo index). Empty = linkage disabled.
	LinkageRedisURL string `yaml:"linkage_redis_url"`

	// JudgeModel + JudgeSamplePct configure the LLM-as-judge quality layer
	// (§6.6). Empty model or pct<=0 disables judging.
	JudgeModel     string `yaml:"judge_model"`
	JudgeSamplePct int    `yaml:"judge_sample_pct"`
	ShadowEvalPct  int    `yaml:"shadow_eval_pct"`

	// EmitterType selects how AIQG events are published:
	//   "log"   — logrus → Loki (default; works without infra)
	//   "kafka" — sarama SyncProducer → Kafka topic (durable, partitioned)
	//   "both"  — MultiEmitter fans out to both
	// Empty string defaults to "log" so existing deployments stay on
	// the current path without explicit config.
	EmitterType string `yaml:"emitter_type"`

	// Kafka block — read when EmitterType is "kafka" or "both".
	// Brokers takes a list ("host:9092,host:9092") via env override.
	Kafka AIQGKafkaConfig `yaml:"kafka"`

	// ResponseCache is the C1 exact-match response cache (docs/AIQG-CACHING.md).
	ResponseCache AIQGResponseCacheConfig `yaml:"response_cache"`

	// Affinity keeps a conversation on the provider whose vendor prompt cache
	// is warm. Mirrors config.AIQGAffinityConfig.
	Affinity AIQGAffinityConfig `yaml:"affinity"`

	// Breaker is passive outlier detection + retry budgets
	// (routing-decision.md step 2).
	Breaker AIQGBreakerConfig `yaml:"breaker"`

	// SemCache is the C4 semantic response cache (docs/AIQG-SEMANTIC-CACHING.md).
	SemCache AIQGSemCacheConfig `yaml:"semantic_cache"`
}

// AIQGAffinityConfig configures provider affinity. Mirrors
// config.AIQGAffinityConfig (straight field copy in ToServerConfig).
type AIQGAffinityConfig struct {
	Enabled   bool          `yaml:"enabled"`
	KeySource string        `yaml:"key_source"`
	Scope     string        `yaml:"scope"`
	TTL       time.Duration `yaml:"ttl"`
	OnBreak   string        `yaml:"on_break"`
}

// AIQGBreakerConfig configures passive outlier detection. Mirrors
// config.AIQGBreakerConfig (straight field copy in ToServerConfig).
type AIQGBreakerConfig struct {
	Enabled           bool          `yaml:"enabled"`
	ConsecutiveErrors int           `yaml:"consecutive_errors"`
	ErrorRatePercent  int           `yaml:"error_rate_percent"`
	MinRequests       int           `yaml:"min_requests"`
	Window            time.Duration `yaml:"window"`
	EjectFor          time.Duration `yaml:"eject_for"`
	RetryRatio        float64       `yaml:"retry_ratio"`
	MinRetries        int           `yaml:"min_retries"`
}

// AIQGResponseCacheConfig configures the C1 response cache. Mirrors
// config.AIQGResponseCacheConfig (straight field copy in ToServerConfig).
type AIQGResponseCacheConfig struct {
	Enabled               bool          `yaml:"enabled"`
	TTL                   time.Duration `yaml:"ttl"`
	MaxBodyBytes          int           `yaml:"max_body_bytes"`
	AllowNondeterministic bool          `yaml:"allow_nondeterministic"`
	InExperiments         bool          `yaml:"in_experiments"`
}

// AIQGSemCacheConfig configures the C4 semantic cache. Runs on the C1 exact-miss
// path (L0→L1). Mirrors config.AIQGSemCacheConfig.
type AIQGSemCacheConfig struct {
	Enabled       bool          `yaml:"enabled"`
	Shadow        bool          `yaml:"shadow"`         // log would-hit, serve nothing (S1)
	MinSimilarity float64       `yaml:"min_similarity"` // L1 candidate floor (cosine)
	TTL           time.Duration `yaml:"ttl"`
	RedisURL      string        `yaml:"redis_url"`   // redis-semcache (redis-stack)
	OllamaURL     string        `yaml:"ollama_url"`  // embeddings server
	EmbedModel    string        `yaml:"embed_model"` // all-minilm
	Dim           int           `yaml:"dim"`         // 384
	// EmbedProvider selects the L1 backend: "ollama" (default) or "tei". TEI
	// serves langcache-embed-v3-small (§6), which Ollama cannot serve at all.
	// ⚠️ Both are 384-dim, so switching does NOT invalidate stored vectors and
	// the store will silently compare across models — flush the ENTRY keys on
	// cutover, i.e. aiqg:scache:{tenant}:* and NOT aiqg:scache:*. The wildcard
	// also matches the judge's labeled-pair corpus, which is the training data
	// for threshold calibration and must survive a cutover.
	EmbedProvider   string  `yaml:"embed_provider"`
	TEIURL          string  `yaml:"tei_url"`           // TEI base URL when EmbedProvider="tei"
	JudgeDailyUSD   float64 `yaml:"judge_daily_usd"`   // L3 judge daily $ cap (§14.1); 0 = unlimited
	JudgeEnabled    bool    `yaml:"judge_enabled"`     // L3 async judge on (opt-in opex)
	JudgeModel      string  `yaml:"judge_model"`       // grader model; empty → AIQG JudgeModel
	JudgeSampleRate float64 `yaml:"judge_sample_rate"` // fraction of eligible lookups graded
}

// AIQGKafkaConfig configures the Kafka emitter. Brokers + topic are
// the minimum; production tuning (compression, idempotence) is set
// inside NewKafkaEmitter.
type AIQGKafkaConfig struct {
	Brokers []string `yaml:"brokers"`
	Topic   string   `yaml:"topic"`
}

// NewServer creates a new server instance
func NewServer(router *routing.Router, config *ServerConfig, logger *logrus.Logger) (*Server, error) {
	server := &Server{
		router: router,
		logger: logger,
		config: config,
	}

	// Initialize security middleware if configured
	if config.Security != nil {
		securityMiddleware, err := middleware.NewSecurityMiddleware(config.Security, logger)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize security middleware: %w", err)
		}
		server.securityMiddleware = securityMiddleware
	}

	// Initialize validation middleware if configured
	if config.Validation != nil {
		validationMiddleware, err := middleware.NewValidationMiddleware(config.Validation, logger)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize validation middleware: %w", err)
		}
		server.validationMiddleware = validationMiddleware
	}

	// Initialize AIQG ingress wire-up if enabled. Permissive mode (the
	// MVP default) leaves internal callers untouched — they don't carry
	// TAS-Auth, so the middleware passes them through. Customer-facing
	// requests with TAS-Auth + Authorization enter Path A: timing
	// collector + parsed headers attached to ctx, paired CloudEvents
	// emitted on response completion via LogEmitter → logrus → Loki.
	if config.AIQG != nil && config.AIQG.Enabled {
		// Wire the AIQG events package to push per-emit metrics into the
		// pkg/aiqg/metrics registry. The events package keeps its
		// recorder interface so it doesn't take a prometheus dep
		// directly; this server-side adapter is the bridge.
		events.MetricsRecorder = aiqgMetricsAdapter{}

		// Select emitter per config. Default = log (matches prior behavior).
		emitter, err := buildAIQGEmitter(config.AIQG, logger)
		if err != nil {
			return nil, fmt.Errorf("failed to build AIQG emitter: %w", err)
		}
		server.aiqgEmitter = emitter

		// Build the token resolver. Priority:
		//   1. DashboardResolver (HTTP client of aiqg-dashboard-be) —
		//      production path, lets ops manage tokens through the
		//      dashboard UI instead of redeploying the Secret.
		//   2. MapResolver from the K8s Secret — bootstrap / fallback
		//      for when dashboard-be isn't yet reachable.
		//   3. Nil — middleware accepts any bearer as opaque, events
		//      carry empty tenant fields (incremental-rollout path).
		var resolver tokens.Resolver
		switch {
		case config.AIQG.DashboardURL != "" && config.AIQG.DashboardInternalAuthToken != "":
			dr, err := tokens.NewDashboardResolver(config.AIQG.DashboardURL, config.AIQG.DashboardInternalAuthToken)
			if err != nil {
				return nil, fmt.Errorf("failed to build AIQG DashboardResolver: %w", err)
			}
			resolver = dr
			logger.WithField("dashboard_url", config.AIQG.DashboardURL).
				Info("AIQG token resolver: DashboardResolver (HTTP client of aiqg-dashboard-be)")
		case len(config.AIQG.Tokens) > 0:
			mr := tokens.NewMapResolver(config.AIQG.Tokens)
			resolver = mr
			logger.WithField("token_count", mr.Len()).
				Info("AIQG token resolver: MapResolver (K8s Secret) — set aiqg.dashboard_url to switch to the dashboard-be HTTP resolver")
		default:
			logger.Info("AIQG token resolver: none — events emit with empty tenant fields")
		}

		// Phase 4.0 — build the policy bundle resolver alongside the
		// token resolver. Reuses the same dashboard URL + internal
		// auth token; only constructed when both are configured.
		// Nil PolicyResolver leaves events without a
		// resolved_policy_bundle field (pre-4.0 behavior).
		var policyResolver policy.Resolver
		if config.AIQG.DashboardURL != "" && config.AIQG.DashboardInternalAuthToken != "" {
			pr, err := policy.NewDashboardResolver(config.AIQG.DashboardURL, config.AIQG.DashboardInternalAuthToken)
			if err != nil {
				return nil, fmt.Errorf("failed to build AIQG policy.DashboardResolver: %w", err)
			}
			policyResolver = pr
			logger.WithField("dashboard_url", config.AIQG.DashboardURL).
				Info("AIQG policy resolver: DashboardResolver (Phase 4.0 — observation only)")
		} else {
			logger.Info("AIQG policy resolver: none — events emit without resolved_policy_bundle")
		}

		// Deterministic `linked`-tier store (tool_call_id echo index).
		// Redis-backed when AIQG_LINKAGE_REDIS_URL is set; nil otherwise
		// (linkage disabled — events emit without step/parent topology).
		var linkageStore linkage.Store
		// Prompt-cache opportunity probe (P0, docs/AIQG-PROMPT-CACHE-CONTROL.md
		// §9). Shares the linkage Redis client — same lifecycle, same tenant
		// scoping, its own key namespace (aiqg:pcp:) and its own 5m TTL (one
		// vendor cache lifetime, not linkage's 1h). Nil when Redis isn't
		// configured: the probe needs a fleet-wide index to be meaningful, and
		// a per-pod MemoryProbe would under-count across replicas rather than
		// fail honestly.
		var cacheProbe promptcache.Probe
		// sharedRedis is reused by every AIQG Redis consumer (linkage, the P0
		// probe, and the C1 response cache) — one client, one lifecycle.
		var sharedRedis redis.UniversalClient
		if config.AIQG.LinkageRedisURL != "" {
			if opt, err := redis.ParseURL(config.AIQG.LinkageRedisURL); err != nil {
				logger.WithError(err).Warn("AIQG linkage: invalid AIQG_LINKAGE_REDIS_URL; linkage disabled")
			} else {
				rc := redis.NewClient(opt)
				sharedRedis = rc
				linkageStore = linkage.NewRedisStore(rc, 0)
				cacheProbe = promptcache.NewRedisProbe(rc, 0)
				logger.WithField("redis_addr", opt.Addr).Info("AIQG linkage: tool_call_id echo index enabled (linked tier)")
				logger.WithFields(logrus.Fields{"redis_addr": opt.Addr, "ttl": promptcache.DefaultTTL}).
					Info("AIQG prompt-cache probe enabled (P0 measure-only: reports prefix reuse, changes no requests)")
			}
		}

		// C1 exact-match response cache (docs/AIQG-CACHING.md). Prefers the
		// shared Redis (correct across replicas); falls back to a per-pod
		// in-memory cache when Redis isn't configured, so a single-replica dev
		// deploy still exercises the feature. Off unless explicitly enabled.
		if config.AIQG.ResponseCache.Enabled {
			ttl := config.AIQG.ResponseCache.TTL
			if ttl <= 0 {
				ttl = 5 * time.Minute
			}
			server.respCacheCfg = responsecache.Config{
				Enabled:              true,
				TTL:                  ttl,
				RequireDeterministic: !config.AIQG.ResponseCache.AllowNondeterministic,
				MaxBodyBytes:         config.AIQG.ResponseCache.MaxBodyBytes,
				InExperiments:        config.AIQG.ResponseCache.InExperiments,
			}
			backend := "memory"
			if sharedRedis != nil {
				server.respCache = responsecache.NewRedisCache(sharedRedis)
				backend = "redis"
			} else {
				server.respCache = responsecache.NewMemoryCache(0)
			}
			logger.WithFields(logrus.Fields{
				"backend":               backend,
				"ttl":                   ttl,
				"require_deterministic": server.respCacheCfg.RequireDeterministic,
			}).Info("AIQG response cache enabled (C1 exact-match)")
		}

		// Passive outlier detection + retry budgets (routing-decision.md step
		// 2). Prefers the shared Redis so breaker state is FLEET-WIDE: with
		// per-replica state each replica must independently learn a provider
		// is bad, and a recovering provider would get one half-open probe per
		// replica instead of one in total. The memory fallback keeps the
		// gateway working without Redis, and the log line names the downgrade
		// so the weaker guarantee is never silently in force.
		{
			bcfg := breaker.Config{
				ConsecutiveErrors: config.AIQG.Breaker.ConsecutiveErrors,
				ErrorRatePercent:  config.AIQG.Breaker.ErrorRatePercent,
				MinRequests:       config.AIQG.Breaker.MinRequests,
				Window:            config.AIQG.Breaker.Window,
				EjectFor:          config.AIQG.Breaker.EjectFor,
				RetryRatio:        config.AIQG.Breaker.RetryRatio,
				MinRetries:        config.AIQG.Breaker.MinRetries,
			}
			if config.AIQG.Breaker.Enabled {
				var store breaker.Store
				backend := "memory (per-replica — breaker state is NOT shared across pods)"
				if sharedRedis != nil {
					store = breaker.NewRedisStore(sharedRedis)
					backend = "redis (fleet-wide)"
				} else {
					store = breaker.NewMemoryStore()
				}
				b := breaker.New(store, bcfg)
				router.SetBreaker(b)
				// Switching hysteresis shares the same fleet-wide store choice:
				// per-replica dwell is no dwell at all.
				if sharedRedis != nil {
					router.SetDwellStore(routing.NewRedisDwellStore(sharedRedis))
				} else {
					router.SetDwellStore(routing.NewMemoryDwellStore())
				}
				eff := b.Config()
				logger.WithFields(logrus.Fields{
					"backend":            backend,
					"consecutive_errors": eff.ConsecutiveErrors,
					"error_rate_percent": eff.ErrorRatePercent,
					"min_requests":       eff.MinRequests,
					"window":             eff.Window,
					"eject_for":          eff.EjectFor,
					"retry_ratio":        eff.RetryRatio,
				}).Info("AIQG breaker enabled (passive outlier detection + retry budget)")
			}
		}

		// Provider affinity (routing-decision.md §5.5). Prefers the shared Redis
		// so affinity is FLEET-WIDE: per-replica state would send the same
		// conversation to a different provider depending on which pod answered,
		// rebuilding the vendor cache each time — exactly the cost affinity
		// exists to avoid.
		if config.AIQG.Affinity.Enabled {
			acfg := affinity.Config{
				KeySource: config.AIQG.Affinity.KeySource,
				Scope:     config.AIQG.Affinity.Scope,
				TTL:       config.AIQG.Affinity.TTL,
				OnBreak:   affinity.OnBreak(config.AIQG.Affinity.OnBreak),
			}
			if acfg.KeySource == "" {
				acfg.KeySource = "conversation"
			}
			var store affinity.Store
			backend := "memory (per-replica — affinity is NOT shared across pods)"
			if sharedRedis != nil {
				store = affinity.NewRedisStore(sharedRedis)
				backend = "redis (fleet-wide)"
			} else {
				store = affinity.NewMemoryStore()
			}
			server.affinity = affinity.New(store, acfg)
			logger.WithFields(logrus.Fields{
				"backend":    backend,
				"key_source": acfg.KeySource,
				"scope":      acfg.Scope,
				"on_break":   acfg.OnBreak,
			}).Info("AIQG provider affinity enabled (keeps a conversation on a warm vendor cache)")
		}

		// C4 semantic response cache (docs/AIQG-SEMANTIC-CACHING.md). Runs on the
		// C1 exact-miss path (L0→L1). Uses the dedicated redis-semcache (FT.* FLAT
		// index) + Ollama embeddings — both separate from the shared AIQG Redis.
		// Default shadow: it embeds, searches, and runs the L2 gate, but SERVES
		// NOTHING — it only logs what WOULD have hit (§15 S1). Disabled unless a
		// redis + ollama URL are configured.
		if config.AIQG.SemCache.Enabled && config.AIQG.SemCache.RedisURL != "" && config.AIQG.SemCache.OllamaURL != "" {
			if opt, err := redis.ParseURL(config.AIQG.SemCache.RedisURL); err != nil {
				logger.WithError(err).Warn("AIQG semantic cache: invalid redis_url; semantic cache disabled")
			} else {
				dim := config.AIQG.SemCache.Dim
				if dim <= 0 {
					dim = 384
				}
				minSim := config.AIQG.SemCache.MinSimilarity
				if minSim <= 0 {
					minSim = 0.95
				}
				ttl := config.AIQG.SemCache.TTL
				if ttl <= 0 {
					ttl = 30 * time.Minute
				}
				scRedis := redis.NewClient(opt)
				store := semcache.NewRedisStore(scRedis, dim)
				if ierr := store.EnsureIndex(context.Background()); ierr != nil {
					logger.WithError(ierr).Warn("AIQG semantic cache: FT.CREATE failed; semantic cache disabled")
				} else {
					// L1 embedding backend. Default stays Ollama so this is a
					// no-op upgrade; "tei" selects the TEI path that serves
					// langcache-embed-v3-small (§6 — the model chosen for cache
					// matching, which Ollama cannot serve).
					var embed semcache.Embedder
					if strings.EqualFold(config.AIQG.SemCache.EmbedProvider, "tei") {
						teiURL := config.AIQG.SemCache.TEIURL
						if teiURL == "" {
							logger.Warn("AIQG semantic cache: embed_provider=tei but tei_url is empty; falling back to Ollama")
							embed = semcache.NewOllamaEmbedder(config.AIQG.SemCache.OllamaURL, config.AIQG.SemCache.EmbedModel, dim)
						} else {
							logger.WithField("tei_url", teiURL).Info("AIQG semantic cache: using TEI embedder")
							embed = semcache.NewTEIEmbedder(teiURL, dim)
						}
					} else {
						embed = semcache.NewOllamaEmbedder(config.AIQG.SemCache.OllamaURL, config.AIQG.SemCache.EmbedModel, dim)
					}
					server.semCache = semcache.New(semcache.Config{
						Enabled:       true,
						Shadow:        config.AIQG.SemCache.Shadow,
						MinSimilarity: minSim,
						CandidateK:    5,
						TTL:           ttl,
					}, store, embed)
					// Remember the global C4 defaults so per-tenant overrides can
					// layer over them (cacheconfig resolver, below).
					server.semGlobalEnabled = true
					server.semGlobalShadow = config.AIQG.SemCache.Shadow
					server.semGlobalMinSim = minSim
					logger.WithFields(logrus.Fields{
						"redis_addr":     opt.Addr,
						"shadow":         config.AIQG.SemCache.Shadow,
						"min_similarity": minSim,
						"embed_model":    config.AIQG.SemCache.EmbedModel,
						"dim":            dim,
					}).Info("AIQG semantic cache enabled (C4 — shadow: logs would-hits, serves nothing)")

					// C4 L3 judge (§5, §14.1): opt-in, off the hot path. Grades
					// sampled near-misses/would-hits → labeled pairs (Redis) + the
					// sampled FPR, under a hard daily $ cap. Nil when disabled.
					server.semJudge, server.semJudgeCfg = buildSemJudge(config.AIQG.SemCache, config.AIQG.JudgeModel, router, scRedis, logger)
					if server.semJudge != nil {
						go server.semJudge.Run(context.Background())
						registerSemJudgeMetrics(server.semJudge, logger)
					}
				}
			}
		}

		// Experiments runner resolver (Phase D). Reuses the dashboard URL +
		// internal auth token; loads each tenant's active (dry_run+running)
		// experiments on a TTL cache and assigns requests to variants. Nil
		// when the dashboard isn't configured — no assignment, no stamping.
		var experimentResolver *experiments.Resolver
		if config.AIQG.DashboardURL != "" && config.AIQG.DashboardInternalAuthToken != "" {
			loader := experiments.NewHTTPLoader(config.AIQG.DashboardURL, config.AIQG.DashboardInternalAuthToken)
			experimentResolver = experiments.NewResolver(loader, 0) // 0 = 30s TTL default
			logger.WithField("dashboard_url", config.AIQG.DashboardURL).
				Info("AIQG experiments resolver enabled (Phase D)")
		}

		// Per-tenant cache-config resolver (docs/AIQG-SEMANTIC-CACHING.md §3, §10).
		// Reuses the dashboard URL + internal auth token; polls /internal/cache-config
		// on a TTL and applies C1/C4 overrides per request — the safe per-tenant path
		// to opt a tenant into semantic SERVING. Nil when the dashboard isn't wired.
		if config.AIQG.DashboardURL != "" && config.AIQG.DashboardInternalAuthToken != "" {
			ccLoader := cacheconfig.NewHTTPLoader(config.AIQG.DashboardURL, config.AIQG.DashboardInternalAuthToken)
			server.cacheCfgResolver = cacheconfig.NewResolver(ccLoader, 0) // 0 = 30s TTL default
			logger.WithField("dashboard_url", config.AIQG.DashboardURL).
				Info("AIQG per-tenant cache-config resolver enabled")

			// BYOK credential resolver (Plan #14 Phase 2) — resolves the
			// tenant's stored provider key + fallback policy per request.
			server.credResolver = credentials.NewResolver(config.AIQG.DashboardURL, config.AIQG.DashboardInternalAuthToken, 0)
			logger.WithField("dashboard_url", config.AIQG.DashboardURL).
				Info("AIQG BYOK credential resolver enabled")
		}

		server.aiqgMiddleware = middleware.NewAIQG(middleware.AIQGConfig{
			Strict:         config.AIQG.Strict,
			Logger:         logger,
			Emitter:        server.aiqgEmitter,
			Region:         config.AIQG.Region,
			IPCaptureMode:  config.AIQG.IPCaptureMode,
			Resolver:       resolver,
			PolicyResolver: policyResolver,
			Linkage:        linkageStore,
			PromptCache:    cacheProbe,
			Experiments:    experimentResolver,
		})
		logger.WithFields(logrus.Fields{
			"strict":           config.AIQG.Strict,
			"region":           config.AIQG.Region,
			"resolver_enabled": resolver != nil,
		}).Info("AIQG ingress enabled on completion routes")

		// LLM-as-judge quality layer (§6.6) — samples completed responses and
		// scores them off the hot path with a third model. Disabled unless a
		// judge model + sample pct + dashboard ingest are all configured.
		server.judge = newJudgeRunner(router, config.AIQG.JudgeModel, config.AIQG.JudgeSamplePct,
			config.AIQG.ShadowEvalPct, experimentResolver,
			config.AIQG.DashboardURL, config.AIQG.DashboardInternalAuthToken, logger)
		if server.judge != nil {
			logger.WithFields(logrus.Fields{
				"judge_model": config.AIQG.JudgeModel,
				"sample_pct":  config.AIQG.JudgeSamplePct,
				"shadow_pct":  config.AIQG.ShadowEvalPct,
			}).Info("AIQG LLM-as-judge enabled")
		}
	}

	return server, nil
}

// SetGatekeeper configures the Gatekeeper content scanning client.
func (s *Server) SetGatekeeper(client *gatekeeper.Client, cfg *gatekeeper.Config, bypass *gatekeeper.BypassManager) {
	s.gatekeeper = client
	s.gatekeeperConfig = cfg
	if cfg != nil {
		s.extractionApplyDisabled = cfg.Extraction.ApplyDisabled
	}
	if bypass != nil {
		s.bypassManager = bypass
		s.bypassHandler = gatekeeper.NewBypassHandler(bypass, s.logger)
	}
}

// Start starts the HTTP server
func (s *Server) Start() error {
	r := s.setupRoutes()

	s.httpServer = &http.Server{
		Addr: ":" + s.config.Port,
		// CORS wraps the ROUTER, not individual routes. gorilla/mux's r.Use()
		// only runs middleware on MATCHED routes, and the completion routes are
		// registered POST-only — so a browser's OPTIONS preflight fell through
		// to the 404 handler with no CORS headers at all, and every
		// cross-origin request from a browser was blocked before it was ever
		// sent. (Verified against production: OPTIONS /v1/chat/completions
		// returned 404 with no Access-Control-* headers, and the aiqg gateway
		// ingress carries no CORS annotations either.) Wrapping here means
		// preflight is answered for every path, matched or not.
		Handler:        s.corsMiddleware(r),
		ReadTimeout:    s.config.ReadTimeout,
		WriteTimeout:   s.config.WriteTimeout,
		MaxHeaderBytes: s.config.MaxHeaderBytes,
	}

	s.logger.WithField("port", s.config.Port).Info("Starting LLM Router server")
	return s.httpServer.ListenAndServe()
}

// Stop stops the HTTP server gracefully
func (s *Server) Stop(ctx context.Context) error {
	s.logger.Info("Stopping LLM Router server")

	// Stop security middleware
	if s.securityMiddleware != nil {
		s.securityMiddleware.Stop()
	}

	// Close gatekeeper
	if s.gatekeeper != nil {
		if err := s.gatekeeper.Close(); err != nil {
			s.logger.WithError(err).Error("Failed to close gatekeeper")
		}
	}

	// Flush + close any Kafka producer underneath the AIQG emitter
	// chain. Walks the MultiEmitter to find KafkaEmitter instances.
	closeAIQGEmitter(s.aiqgEmitter, s.logger)

	return s.httpServer.Shutdown(ctx)
}

func closeAIQGEmitter(em events.Emitter, log *logrus.Logger) {
	switch e := em.(type) {
	case *events.KafkaEmitter:
		if err := e.Close(); err != nil {
			log.WithError(err).Error("Failed to close AIQG Kafka emitter")
		}
	case *events.MultiEmitter:
		for _, inner := range e.Emitters {
			closeAIQGEmitter(inner, log)
		}
	}
}

// setupRoutes configures all HTTP routes
func (s *Server) setupRoutes() *mux.Router {
	r := mux.NewRouter()

	// Add security middleware first (if enabled)
	if s.securityMiddleware != nil {
		r.Use(s.securityMiddleware.Handler())
	}

	// Add validation middleware (if enabled)
	if s.validationMiddleware != nil {
		r.Use(s.validationMiddleware.Middleware)
	}

	// Add other middleware
	r.Use(s.loggingMiddleware)
	// NOTE: corsMiddleware is applied in Start() around the whole router, not
	// here — r.Use only fires on matched routes, which silently broke preflight.
	// Kept off the mux deliberately; do not re-add it as route middleware.
	r.Use(s.contentTypeMiddleware)

	// API routes
	api := r.PathPrefix("/v1").Subrouter()

	// Wrap the three LLM completion handlers in the AIQG ingress
	// middleware when enabled. Management/health/metrics routes are NOT
	// wrapped — they're internal-only and don't need TAS-* parsing or
	// timing capture. Returns the handler verbatim when AIQG is disabled.
	wrapAIQG := func(h http.HandlerFunc) http.Handler {
		if s.aiqgMiddleware == nil {
			return h
		}
		return s.aiqgMiddleware(h)
	}

	// OpenAI compatible endpoints
	api.Handle("/chat/completions", wrapAIQG(s.handleChatCompletion)).Methods("POST")
	api.Handle("/completions", wrapAIQG(s.handleCompletion)).Methods("POST")

	// Anthropic compatible endpoints
	api.Handle("/messages", wrapAIQG(s.handleMessages)).Methods("POST")
	api.Handle("/messages/count_tokens", wrapAIQG(s.handleCountTokens)).Methods("POST")

	// OpenAI-compatible embeddings (client.embeddings.create). Wrapped in AIQG
	// for auth + event emission; routes to the embeddings-capable provider.
	api.Handle("/embeddings", wrapAIQG(s.handleEmbeddings)).Methods("POST")

	// OpenAI Responses API (client.responses.create). Translated at the
	// boundary and run through the shared chat pipeline, like /v1/messages.
	api.Handle("/responses", wrapAIQG(s.handleResponses)).Methods("POST")

	// OpenAI-compatible model discovery (client.models.list()/retrieve()).
	// Unwrapped, like /providers — provider metadata, no TAS-* auth needed.
	api.HandleFunc("/models", s.handleListModels).Methods("GET")
	api.HandleFunc("/models/{model}", s.handleGetModel).Methods("GET")

	// Router management endpoints
	api.HandleFunc("/providers", s.handleListProviders).Methods("GET")
	api.HandleFunc("/providers/{name}", s.handleGetProvider).Methods("GET")
	api.HandleFunc("/health", s.handleHealthCheck).Methods("GET")
	api.HandleFunc("/health/{name}", s.handleProviderHealth).Methods("GET")
	api.HandleFunc("/capabilities", s.handleCapabilities).Methods("GET")
	// Provider-fleet breaker state, for the operator health panel.
	api.HandleFunc("/breaker", s.handleBreakerStatus).Methods("GET")
	api.HandleFunc("/routing/decision", s.handleRoutingDecision).Methods("POST")

	// Bypass token management endpoints
	if s.bypassHandler != nil {
		s.bypassHandler.RegisterRoutes(r)
	}

	// Health check endpoint (no /v1 prefix)
	r.HandleFunc("/health", s.handleHealthCheck).Methods("GET")

	// Metrics endpoint for Prometheus scraping (legacy hand-rolled).
	r.HandleFunc("/metrics", s.handleMetrics).Methods("GET")

	// AIQG metrics endpoint — separate registry served via promhttp.
	// Scraped independently by Prometheus per shared-monitoring config.
	r.Handle("/aiqg/metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{})).Methods("GET")

	// Swagger UI documentation endpoints
	s.setupSwaggerRoutes(r)

	return r
}

// Middleware

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Create a custom response writer to capture status code
		wrapped := &responseWriter{ResponseWriter: w, statusCode: 200}

		next.ServeHTTP(wrapped, r)

		s.logger.WithFields(logrus.Fields{
			"method":      r.Method,
			"path":        r.URL.Path,
			"status":      wrapped.statusCode,
			"duration_ms": time.Since(start).Milliseconds(),
			"user_agent":  r.UserAgent(),
			"remote_addr": r.RemoteAddr,
		}).Info("HTTP request")
	})
}

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		// Include the AIQG gateway auth header (TAS-Auth) and Anthropic's
		// native headers (x-api-key, anthropic-version) so a browser-side
		// OpenAI/Anthropic SDK pointed at the gateway can preflight-clear its
		// credentials. TAS-* control headers are allowed for power users who
		// set them via default_headers.
		// TAS-Agent-*/TAS-Flow-Id/TAS-Conversation-Id/baggage are the agent-flow
		// attribution headers. Without them in this list a browser client can
		// send traffic but not attribute it, so its own requests show up
		// unattributed in Traffic Explorer — which is where the UI displays them.
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key, TAS-Auth, anthropic-version, TAS-Upstream-Authorization, TAS-Source-App, TAS-Agent-Id, TAS-Agent-Name, TAS-Agent-Version, TAS-Flow-Id, TAS-Conversation-Id, TAS-Cache, TAS-Prompt-Cache, TAS-Prompt-Cache-TTL, TAS-Synthetic, baggage")
		// Let browser clients read the routing-decision headers (moved off
		// the SSE stream), the AIQG response-event id, and the cache verdict.
		// X-TAS-Cache (hit | semantic_hit | bypass; absent on a miss) is the
		// only signal a browser has for whether a response came from cache —
		// the cache fields are not promoted into Loki labels either, so
		// without exposing it a client cannot tell a hit from a miss at all.
		w.Header().Set("Access-Control-Expose-Headers", "X-TAS-Router-Provider, X-TAS-Router-Model, X-TAS-Router-Request-Id, X-TAS-Router-Attempt-Count, X-TAS-Router-Fallback-Used, X-TAS-Router-Estimated-Cost, TAS-Response-Event-Id, X-TAS-Cache")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) contentTypeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" || r.Method == "PUT" {
			contentType := r.Header.Get("Content-Type")
			if contentType != "application/json" && contentType != "" {
				s.writeErrorResponse(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Handlers

// handleChatCompletion handles OpenAI-compatible chat completion requests
func (s *Server) handleChatCompletion(w http.ResponseWriter, r *http.Request) {
	var req types.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeErrorResponse(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}

	// Generate request ID if not provided
	if req.ID == "" {
		req.ID = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	req.Timestamp = time.Now()

	// AIQG: heuristically classify the workflow from request shape so
	// the CLEAR latency scorer uses a sensible SLA when the caller
	// didn't send a TAS-Workflow header. Empty result is a no-op.
	wf := workflow.Classify(&req)
	// AIQG (Phase 4.1): refine the policy bundle now that the requested
	// model + workflow are known, so route rules targeting by model/workflow
	// take effect. Matches the REQUESTED model (before any experiment
	// override), consistent with experiment cohort matching below. No-op
	// outside AIQG / when an explicit bundle header pinned the choice.
	middleware.ResolveBundleForRouting(r.Context(), req.Model, wf, r.URL.Path)
	// A resolved route rule may steer this request to a specific provider
	// (routing-decision.md §5.2). Pin it on the context so the router prefers
	// it over the configured strategy; the pin is not honoured when the
	// provider is unconfigured or unhealthy, and the router records why.
	reqCtx := r.Context()
	ctxChanged := false
	if tgt := middleware.ResolvedTarget(reqCtx); tgt != nil && tgt.Provider != "" {
		reqCtx = routing.WithPinnedProvider(reqCtx, tgt.Provider)
		if tgt.Model != "" {
			req.Model = tgt.Model
		}
		ctxChanged = true
	}
	// A matched rule may also tighten or loosen this request's resilience
	// thresholds. Carried on the context for the same reason as the pin: it is
	// routing metadata that must never reach a vendor payload.
	if h, b := middleware.ResolvedResilience(reqCtx); h != nil || b != nil {
		reqCtx = routing.WithResilience(reqCtx, h, b)
		ctxChanged = true
	}
	// The failover chain and the tenant's constraints. Attached even when only
	// constraints are present: they bound routing whether or not a rule
	// configured failover, including on the default path where no rule matched.
	// Context/output caps (step 7). Attached before routing so the pre-flight
	// check and the fallback walk both see them.
	if lim := middleware.ResolvedLimits(reqCtx); !lim.IsZero() {
		reqCtx = routing.WithLimits(reqCtx, lim)
		ctxChanged = true
	}
	// Quality gates run BEFORE selection, so they are attached first: they
	// filter the candidate set that selection then prices.
	if sig, quality := middleware.ResolvedSignals(reqCtx); !sig.IsZero() {
		reqCtx = routing.WithSignals(reqCtx, sig, quality)
		reqCtx = routing.WithGateContext(reqCtx, req.Model)
		ctxChanged = true
		// Exclusions are collected during routing; stamped after, so the event
		// carries what the gate actually did on this request.
		defer func() {
			ex, note := routing.GateExclusions(reqCtx)
			middleware.StampSignals(reqCtx, toMiddlewareExclusions(ex), note)
		}()
	}
	// Selection strategy + hysteresis + the measured verbosity table.
	if sel, sw, verb := middleware.ResolvedSelection(reqCtx); !sel.IsZero() || !sw.IsZero() || len(verb) > 0 {
		reqCtx = routing.WithSelection(reqCtx, sel, sw, verb, affinityKeyFor(r))
		reqCtx = routing.WithWorkflow(reqCtx, r.Header.Get("TAS-Workflow"))
		ctxChanged = true
	}
	if fb, cons := middleware.ResolvedFallback(reqCtx); fb != nil || cons != nil {
		reqCtx = routing.WithChain(reqCtx, routing.NewChain(fb, cons))
		ctxChanged = true
	}
	// Prompt-cache control (docs/AIQG-PROMPT-CACHE-CONTROL.md §3). Applied
	// here — after resolution, before the vendor call — because the mode may
	// come from the matched route as well as the request header.
	s.applyPromptCacheMode(r, &req)
	// Affinity is resolved AFTER the chain is on the context, because the
	// usable predicate consults constraints — a warm cache on a denied vendor
	// must never be offered in the first place.
	r, _ = s.resolveAffinity(r, &req)
	if ctxChanged {
		r = r.WithContext(reqCtx)
	}
	// AIQG experiments (Phase D): resolve the variant claiming this request on
	// the REQUESTED model + workflow (the cohort matches what the caller
	// asked), then — for a running experiment — apply the variant's override
	// (model swap / param sweep) BEFORE routing + stamping, so the event
	// reflects the SERVED config. dry_run returns Apply=false (stamped only).
	var expReduction *policy.ReductionPolicy
	if d := middleware.ResolveExperimentForRouting(r.Context(), req.Model, wf, r.URL.Path); d != nil && d.Apply {
		applyExperimentOverride(&req, d.Override)
		// An extraction variant carries a `reduction` block in its override.
		// It takes precedence over the bundle's resolved reduction and, in
		// apply mode, actually shrinks the payload below (Plan #7 Phase 3).
		expReduction = parseOverrideReduction(d.Override)
	}
	// AIQG: stamp the (possibly overridden) model + streaming flag onto the
	// routing sidecar so the response event carries them. No-op outside AIQG.
	middleware.StampModel(r.Context(), req.Model)
	middleware.StampStreaming(r.Context(), req.Stream)
	middleware.StampWorkflow(r.Context(), wf)
	// AIQG (linked tier): tool_call_ids this request echoes (role=tool) so
	// the middleware can prove which flow/step it continues.
	middleware.StampEchoedToolCalls(r.Context(), echoedToolCallIDs(&req))
	// AIQG (prefix-chaining tier): hash of the conversation prefix this
	// request continues, so the middleware can recognize a tool-less
	// multi-turn thread it previously served.
	middleware.StampPrefixHash(r.Context(), conversationPrefixHash(&req))
	// AIQG (fingerprinted tier): structural signature (toolset + config) so
	// untagged programmatic traffic resolves to an inferred agent surrogate.
	middleware.StampFingerprint(r.Context(), agentFingerprint(&req))
	// AIQG (prompt-cache probe, P0 — docs/AIQG-PROMPT-CACHE-CONTROL.md §9):
	// hash of the tools+system span a vendor cache breakpoint would cover, so
	// the middleware can report whether it recurred inside a cache lifetime.
	// Measurement only — nothing derived from this reaches the vendor.
	//
	// Hashed pre-scan, alongside the other stamps. Redaction is deterministic
	// and content-derived (Gatekeeper/pkg/scan/redaction.go), so redact() is a
	// pure function: identical pre-scan prefixes stay identical post-scan, and
	// the reuse rate is preserved. The bias is one-directional and safe — a
	// placeholder strategy can map two *different* prefixes onto the same
	// redacted bytes (a real vendor hit this would score as a miss), so this
	// can only UNDER-count reuse, never inflate it.
	middleware.StampCachePrefixHash(r.Context(), cachePrefixHash(&req))

	// Gatekeeper: check bypass token
	scanBypassed := false
	if bypassToken := r.Header.Get("X-TAS-Scan-Bypass"); bypassToken != "" && s.bypassManager != nil {
		contentHash := "" // Could compute hash of messages if needed
		userID, valid := s.bypassManager.ValidateBypassToken(r.Context(), bypassToken, req.ID, contentHash)
		if valid {
			scanBypassed = true
			s.logger.WithFields(logrus.Fields{
				"request_id": req.ID,
				"user_id":    userID,
			}).Warn("Scan bypassed via token")
		}
	}

	// Gatekeeper: inbound scan
	if !scanBypassed && s.gatekeeper != nil && s.gatekeeperConfig != nil && s.gatekeeperConfig.Inbound.Enabled {
		meta := gatekeeper.ScanMeta{
			TenantID:  s.extractTenantID(r),
			RequestID: req.ID,
			UserID:    s.extractUserID(r),
			Source:    "llm_input",
		}

		result, err := s.gatekeeper.ScanMessages(r.Context(), req.Messages, meta)
		if err != nil {
			s.logger.WithError(err).WithField("request_id", req.ID).Error("Inbound scan failed")
			if !s.gatekeeperConfig.FailOpen {
				s.writeErrorCtx(w, r, http.StatusInternalServerError, "content scan failed")
				return
			}
		} else if s.gatekeeper.ShouldBlock(result, "inbound") {
			msg := gatekeeper.FormatBlockMessage(result, "Inbound")
			s.logger.WithField("request_id", req.ID).Warn(msg)
			s.writeErrorCtx(w, r, http.StatusForbidden, "request blocked by content policy")
			return
		}

		// Set scan status header
		if result != nil && result.ScanResult != nil {
			if len(result.ScanResult.Findings) == 0 {
				w.Header().Set("X-TAS-Scan-Status", "clean")
			} else {
				w.Header().Set("X-TAS-Scan-Status", "violations")
			}

			// AIQG: project Gatekeeper findings into per-severity
			// counts and stamp onto the routing sidecar. Drives the
			// CLEAR.Assurance score and the AssuranceSummary on the
			// emitted event. Empty counts still flip ScanRan true so
			// Assurance scores as Healthy=100 rather than nil. Also
			// computes per-NIST-AI-RMF-characteristic counts so the
			// Day-1 Report Trustworthiness section can break findings
			// down by characteristic, not just severity.
			counts := map[string]int{}
			nistCounts := map[string]int{}
			tagCounts := map[string]int{}
			// Per-finding audit detail alongside the counts. Gatekeeper already
			// produced all of this; the gateway used to discard it here, which
			// is why "why was this blocked?" was previously unanswerable beyond
			// "a rule fired somewhere".
			audit := make([]middleware.ScanFinding, 0, len(result.ScanResult.Findings))
			for _, f := range result.ScanResult.Findings {
				counts[string(f.Severity)]++
				nistCounts[middleware.MapPatternToNIST(f.PatternID)]++
				tagCounts[f.PatternID]++
				audit = append(audit, toScanFinding(f, middleware.GatekeeperDirectionInbound))
			}
			middleware.StampGatekeeperFindings(r.Context(), middleware.GatekeeperDirectionInbound, counts)
			middleware.StampNISTFindings(r.Context(), nistCounts)
			middleware.StampTagFindings(r.Context(), tagCounts)
			middleware.StampScanFindings(r.Context(), audit, events.MaxFindingsPerEvent)
		}
	}

	// Gatekeeper inbound REDACTION (G1, docs/AIQG-GATEKEEPER-INTEGRATION.md).
	// Runs after the block decision (a blocked request never reaches here) and
	// before reduction/routing, so req.Messages is redacted for everything
	// downstream — reduction measurement, routing, and the vendor call all see
	// the redacted content, and a later response cache would key on it.
	//
	// Default off (redaction is lossy). Fail-open: on a scan error we log and
	// send the original content — the same PII exposure as pre-G1, never a
	// dropped request. Deterministic strategy → the redacted prompt is byte-stable
	// (cache-safe).
	if !scanBypassed && s.gatekeeper != nil && s.gatekeeperConfig != nil && s.gatekeeperConfig.Redaction.Enabled {
		meta := gatekeeper.ScanMeta{
			TenantID:  s.extractTenantID(r),
			RequestID: req.ID,
			UserID:    s.extractUserID(r),
			Source:    "llm_input",
		}
		redacted, n, err := s.gatekeeper.RedactMessages(r.Context(), req.Messages, meta, s.gatekeeperConfig.Redaction.Strategy)
		if err != nil {
			s.logger.WithError(err).WithField("request_id", req.ID).Warn("Inbound redaction failed; sending original content")
		} else if n > 0 {
			req.Messages = redacted
			middleware.StampRedaction(r.Context(), n)
			w.Header().Set("X-TAS-Redaction", "applied")
		}
	}

	// AIQG payload-reduction (Plan #7 Phase 2/3). The effective reduction is
	// an applying experiment variant's reduction (Phase 3, takes precedence
	// and MUTATES the payload) or the bundle's resolved reduction (Phase 2,
	// shadow — measure only). Both run the real Gatekeeper extractor.
	red := expReduction
	if red == nil {
		red, _ = middleware.ResolvedReduction(r.Context())
	}
	if red.RunsExtractor() && s.gatekeeper != nil {
		promptBytes := messagesByteLen(req.Messages)
		if red.EligibleBySize(clear.TokensFromBytes(promptBytes)) {
			query := lastUserText(req.Messages)
			// Honor per-method relevance tuning (threshold / top-K); zero
			// fields fall back to the extractor's env defaults.
			var rel gatekeeper.RelevanceOverride
			if rs := red.Steps.Relevance; rs != nil {
				rel = gatekeeper.RelevanceOverride{Threshold: rs.Threshold, TopK: rs.TopK, Ratio: rs.TopKRatio}
			}
			// Phase 0 — cache-safety (AIQG_CACHE_SAFE_REDUCTION.md §6): the LLM
			// gateway is now READ-ONLY. It MEASURES reduction but never mutates
			// the payload. In-place per-turn reduction (applyReductionInline)
			// busted prompt caching by editing already-cached content every turn
			// with a query-dependent rule — so cheap 0.10× cache-reads flipped
			// back to full/creation rate and cost could rise while the metric
			// reported a saving. It's retired. Active *application* of reduction
			// moves to the MCP proxy, at the source, once (§2/§9.A). Both active-
			// and shadow-configured bundles now measure only (Mode="shadow"),
			// which is cache-safe (nothing is mutated) — the projected fields on
			// the event still carry the bundle's configured intent.
			if sampled := sampleHit(red.SampleRate); sampled {
				// Measure-only, concurrent with the vendor call → no client
				// latency and no cache impact.
				msgs := req.Messages
				parentCtx := context.WithoutCancel(r.Context())
				middleware.ExpectReductionMeasurement(parentCtx)
				go func() {
					mctx, cancel := context.WithTimeout(parentCtx, 20*time.Second)
					defer cancel()
					m, err := s.gatekeeper.MeasureReduction(mctx, msgs, query, rel)
					if err != nil || m == nil {
						middleware.StampReductionMeasurement(parentCtx, nil)
						return
					}
					middleware.StampReductionMeasurement(parentCtx, &events.ReductionMeasurement{
						Mode:                    "shadow",
						Sampled:                 true,
						OriginalBytes:           m.OriginalBytes,
						ExtractedBytes:          m.ExtractedBytes,
						SizeAfterRelevanceBytes: m.SizeAfterRelevanceBytes,
					})
				}()
			}
		}
	}

	// Route the request
	metadata, provider, err := s.router.Route(r.Context(), &req)
	if err != nil {
		s.writeErrorCtx(w, r, http.StatusServiceUnavailable, fmt.Sprintf("Routing failed: %v", err))
		return
	}

	// AIQG: stamp the chosen provider's name onto the routing sidecar.
	// Done here (not in router.Route) so AIQG plumbing stays in the
	// server/middleware layer and doesn't leak into the routing package.
	vendor := provider.GetProviderName()
	middleware.StampVendor(r.Context(), vendor)

	// AIQG BYOK (Plan #14): resolve the effective upstream key for the routed
	// vendor and carry it on ctx for the provider to inject. Returns nil to
	// signal it already wrote a 402 (BYOK-only tenant with no stored key).
	if nr := s.applyBYOKKey(w, r, vendor); nr == nil {
		return
	} else {
		r = nr
	}

	// AIQG C1 exact-match response cache (docs/AIQG-CACHING.md §2). Checked after
	// routing so the key carries the resolved vendor, but before the vendor call —
	// a hit skips that 1–5s call entirely (the whole latency win). Routing itself
	// is in-memory selection, not a network call. On a miss we stash the store
	// intent so the completion handler persists the post-outbound-scan response.
	if r = s.maybeServeFromCache(w, r, &req, vendor); r == nil {
		return // hit: cached response already written
	}

	// Handle streaming vs non-streaming with retry/fallback support
	if req.Stream {
		s.handleStreamingCompletionWithRetry(w, r, &req, provider, metadata)
	} else {
		s.handleNonStreamingCompletionWithRetry(w, r, &req, provider, metadata)
	}
}

// bearerKey strips a leading "Bearer " (case-insensitive) so a raw vendor key
// reaches the provider SDK, which adds its own auth scheme.
func bearerKey(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 7 && strings.EqualFold(s[:7], "bearer ") {
		s = strings.TrimSpace(s[7:])
	}
	return s
}

// applyBYOKKey resolves the effective upstream vendor key for the routed vendor
// (Plan #14) and returns a request carrying it on ctx for the provider to
// inject. Precedence: per-request customer key (TAS-Upstream-Authorization, then
// the raw Authorization) > the tenant's stored credential > the TAS shared key
// (fallback, only when the tenant allows it). Returns nil after writing a 402
// when a BYOK-only tenant has no key for the vendor. A no-op (returns r
// unchanged) outside AIQG mode or when the resolver isn't wired — so non-BYOK
// traffic is unaffected and the provider falls back to its configured key.
func (s *Server) applyBYOKKey(w http.ResponseWriter, r *http.Request, vendor string) *http.Request {
	ctx := r.Context()
	tenant := middleware.ResolvedTenantFromContext(ctx)
	if tenant == "" || s.credResolver == nil {
		return r // non-AIQG, or BYOK not configured
	}

	// 1. Explicit per-request customer key wins (the never-store path stays
	//    first-class). ONLY the deliberate TAS-Upstream-Authorization signal —
	//    NOT the raw Authorization header, which historically was required by
	//    the gate but never forwarded, so clients may send a placeholder there;
	//    injecting that would break them. Raw Authorization stays ignored → TAS
	//    key, exactly as today.
	if perReq := bearerKey(vendorUpstreamKey(ctx)); perReq != "" {
		middleware.StampCredentialSource(ctx, "upstream_header", "")
		return r.WithContext(upstreamkey.With(ctx, perReq))
	}

	// 2. Stored credential for (tenant, vendor).
	res, err := s.credResolver.Resolve(ctx, tenant, vendor)
	if err != nil {
		// Resolver blip → degrade to the TAS shared key rather than fail the
		// request. Logged once; the event records tas_shared.
		s.logger.WithError(err).WithField("vendor", vendor).
			Warn("BYOK credential resolve failed; using TAS shared key")
		middleware.StampCredentialSource(ctx, "tas_shared", "")
		return r
	}
	if res.Found {
		middleware.StampCredentialSource(ctx, "stored", res.CredentialID)
		return r.WithContext(upstreamkey.With(ctx, res.APIKey))
	}

	// 3. No stored key. Fall back to the TAS shared key, or hard-fail if the
	//    tenant is BYOK-only.
	if res.AllowSharedFallback {
		middleware.StampCredentialSource(ctx, "tas_shared", "")
		return r
	}
	s.writeErrorCtx(w, r, http.StatusPaymentRequired,
		fmt.Sprintf("provider_key_required: no stored %s credential for this account and shared-key fallback is disabled", vendor))
	return nil
}

// vendorUpstreamKey reads the customer-supplied TAS-Upstream-Authorization from
// the parsed AIQG headers on ctx, or "" when absent.
func vendorUpstreamKey(ctx context.Context) string {
	if h, ok := middleware.FromContext(ctx); ok {
		return h.UpstreamAuthorization
	}
	return ""
}

// maybeServeFromCache runs the C1 cache lookup. It returns nil when it served a
// hit (the caller must stop); otherwise it returns the request to continue with,
// possibly carrying a store-intent context on a cacheable miss. All branches are
// no-ops when the cache is disabled, so non-AIQG traffic is unaffected.
func (s *Server) maybeServeFromCache(w http.ResponseWriter, r *http.Request, req *types.ChatRequest, vendor string) *http.Request {
	if s.respCache == nil || !s.respCacheCfg.Enabled {
		return r
	}
	d := responsecache.Decide(req, s.respCacheCfg, r.Header.Get("TAS-Cache"))
	if d.Bypass {
		middleware.StampCacheState(r.Context(), "bypass")
		return r
	}
	if !d.Cacheable {
		return r
	}
	// Experiment interaction (docs/AIQG-CACHING.md §7, C3). By default an
	// experiment-claimed request bypasses the cache so each variant's measurement
	// reflects real variant calls. When InExperiments is opted in, cache instead
	// but fold the variant into the key so variant A and B never share an entry;
	// the dashboard's per-variant queries then exclude cache hits from the verdict.
	variant := ""
	if rt := middleware.RoutingFromContext(r.Context()); rt != nil {
		if snap := rt.Snapshot(); snap.ExperimentID != "" {
			if !s.respCacheCfg.InExperiments {
				middleware.StampCacheState(r.Context(), "bypass")
				return r
			}
			variant = snap.ExperimentID + ":" + snap.ExperimentVariant
		}
	}

	tenantID := s.extractTenantID(r)
	// Per-tenant overrides (nil-safe: no resolver / no row → global defaults).
	cc := s.cacheCfgResolver.ForTenant(r.Context(), tenantID)
	exactEnabled := cc.ExactEnabled(s.respCacheCfg.Enabled)

	hash := responsecache.KeyHash(tenantID, vendor, req, events.ScoringVersion, variant)
	middleware.StampCacheKeyHash(r.Context(), hash)

	// C1 exact-match lookup — skipped when this tenant has disabled the exact
	// cache (per-tenant override); the request then flows to the semantic path.
	if exactEnabled {
		if entry, ok, err := s.respCache.Get(r.Context(), tenantID, hash); err != nil {
			s.logger.WithError(err).WithField("request_id", req.ID).Warn("response cache lookup failed; treating as miss")
		} else if ok {
			// Stamp before writing: writeCachedResponse calls WriteHeader, which
			// flushes headers, so X-TAS-Cache must be set inside it (before the flush).
			if s.writeCachedResponse(w, r, entry) {
				middleware.StampCacheState(r.Context(), "hit")
				// C2: record the avoided cost — what the vendor call would have billed,
				// priced from the cached entry's token counts (docs/AIQG-CACHING.md §6).
				cost, _ := clear.DollarCost(vendor, entry.Model, entry.PromptTokens, entry.CompletionTokens)
				middleware.StampCacheSavings(r.Context(), entry.PromptTokens, entry.CompletionTokens, cost)
				return nil
			}
			// A corrupt/undecodable entry falls through to a live call.
		}
	}

	// C4 (L0→L1) on the exact-match miss. A serve-enabled tenant gets a SYNCHRONOUS
	// semantic-hit serve; everyone else takes the async, log-only shadow path.
	// This MUST run before stamping "miss": StampCacheState is first-write-wins, so
	// a served hit stamps cache_state=semantic_hit itself and a premature "miss"
	// here would win and hide the hit from the event stream (§13 reporting).
	if s.maybeServeSemantic(w, r, req, tenantID, vendor, cc) {
		return nil
	}
	middleware.StampCacheState(r.Context(), "miss")
	ctx := s.runSemanticShadow(r.Context(), req, tenantID, hash, cc)
	if !exactEnabled {
		return r.WithContext(ctx) // C1 store also disabled for this tenant
	}
	return r.WithContext(responsecache.WithPending(ctx, tenantID, hash))
}

// runSemanticShadow starts the C4 semantic cascade for a C1 exact-miss. The
// prompt + scope are computed synchronously and stashed for the store; the
// embed+search+L2 lookup runs async (off the request latency path) and logs what
// WOULD have hit. Returns ctx carrying the store intent. No-op when disabled.
func (s *Server) runSemanticShadow(ctx context.Context, req *types.ChatRequest, tenantID, key string, cc *cacheconfig.Config) context.Context {
	if s.semCache == nil || !cc.SemanticEnabled(s.semGlobalEnabled) {
		return ctx
	}
	prompt := lastUserText(req.Messages) // the question is the semantic key
	if prompt == "" {
		return ctx
	}
	scope := semcache.Scope{TenantID: tenantID, Model: req.Model, ScoringVersion: events.ScoringVersion}
	// Per-tenant threshold; shadow is forced on here (this is the log-only path —
	// a serve-enabled tenant was already handled synchronously in maybeServeSemantic).
	minSim := cc.SemanticMinSimilarity(s.semGlobalMinSim)
	shadow := true

	lctx := context.WithoutCancel(ctx)
	reqID, model := req.ID, req.Model
	go func() {
		cctx, cancel := context.WithTimeout(lctx, 15*time.Second)
		defer cancel()
		out := s.semCache.LookupWithOptions(cctx, scope, prompt, semcache.LookupOptions{Shadow: &shadow, MinSimilarity: &minSim})
		switch out.State {
		case semcache.StateShadowHit, semcache.StateSemanticHit:
			s.logger.WithFields(logrus.Fields{
				"event": "aiqg.semcache.shadow", "state": out.State,
				"similarity": out.Similarity, "threshold": out.Threshold,
				"tenant_id": tenantID, "model": model, "request_id": reqID,
			}).Info("aiqg semantic shadow: WOULD-HIT (serving nothing)")
		case semcache.StateMiss:
			if out.Similarity > 0 { // a candidate existed but L2 rejected it — calibration signal
				s.logger.WithFields(logrus.Fields{
					"event": "aiqg.semcache.shadow", "state": "miss",
					"similarity": out.Similarity, "reject_reason": out.RejectReason,
					"tenant_id": tenantID, "model": model, "request_id": reqID,
				}).Info("aiqg semantic shadow: near-miss rejected by L2")
			}
		}
		// L3 judge (§5, §14.1): off-path, opt-in. Sample would-hits (FPR ground
		// truth) and L2-rejected near-misses (L2 precision) in the danger band and
		// enqueue them for the async grader; Enqueue never blocks and drops when
		// the queue is full, so this cannot slow the shadow goroutine.
		s.enqueueForJudge(scope, prompt, out, cc)
	}()
	return semcache.WithPending(ctx, &semcache.Pending{Scope: scope, Key: key, Prompt: prompt})
}

// storeSemantic indexes a produced response for future semantic hits. Async
// (embedding is a network call) and best-effort. No-op unless the lookup stashed
// a pending intent and the response is cacheable.
func (s *Server) storeSemantic(r *http.Request, resp *types.ChatResponse) {
	if s.semCache == nil {
		return
	}
	p, ok := semcache.PendingFromContext(r.Context())
	if !ok || !responsecache.ResponseCacheable(resp) {
		return
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	go func() {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if err := s.semCache.Store(cctx, p.Scope, p.Key, p.Prompt, body, 0); err != nil {
			s.logger.WithError(err).WithField("request_id", resp.ID).Warn("aiqg semantic cache store failed")
		}
	}()
}

// writeCachedResponse writes a stored response to the client. Returns false
// (without writing) if the entry can't be decoded, so the caller falls through
// to a live vendor call. A hit deliberately does NOT stamp vendor token usage:
// the vendor wasn't called, so cost/latency accounting stays ~0 (§6), and the
// cache_state=hit split lets dashboards surface the saving explicitly.
func (s *Server) writeCachedResponse(w http.ResponseWriter, r *http.Request, entry *responsecache.Entry) bool {
	_, ok := writeRawCachedResponse(w, r, entry.Response, "hit")
	return ok
}

// writeRawCachedResponse writes a stored response body to the client, setting the
// X-TAS-Cache header (its value distinguishes an exact "hit" from a "semantic_hit"
// — the header must precede WriteHeader, which flushes headers). It returns the
// decoded response (so the caller can read Usage for savings) and false when the
// body can't be decoded, so the caller falls through to a live vendor call.
func writeRawCachedResponse(w http.ResponseWriter, r *http.Request, body []byte, cacheHeader string) (*types.ChatResponse, bool) {
	var resp types.ChatResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, false
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-TAS-Cache", cacheHeader)
	w.WriteHeader(http.StatusOK)
	// Render in the request's wire shape — a /v1/messages or /v1/responses hit
	// must serve that shape, not the stored OpenAI-shaped body.
	switch fmtOf(r) {
	case responseFormatAnthropic:
		_ = json.NewEncoder(w).Encode(chatResponseToAnthropic(&resp))
	case responseFormatResponses:
		_ = json.NewEncoder(w).Encode(chatResponseToResponses(&resp))
	default:
		_ = json.NewEncoder(w).Encode(&resp)
	}
	return &resp, true
}

// maybeServeSemantic serves a C4 semantic hit when the tenant is serve-enabled
// (Semantic.Enabled && !Shadow). Returns true when it served (caller stops).
// Unlike the shadow path, this runs the cascade SYNCHRONOUSLY — serving puts
// the ~30ms embed+search on the request path, which the design (§6) accepts.
// Reaches here only on a C1 exact miss, so the request is already C1-eligible
// (deterministic, non-streaming, no tools — inherited from responsecache.Decide).
func (s *Server) maybeServeSemantic(w http.ResponseWriter, r *http.Request, req *types.ChatRequest, tenantID, vendor string, cc *cacheconfig.Config) bool {
	if s.semCache == nil || !cc.SemanticEnabled(s.semGlobalEnabled) {
		return false
	}
	if cc.SemanticShadow(s.semGlobalShadow) {
		return false // shadow tenants take the async, log-only path
	}
	prompt := lastUserText(req.Messages)
	if prompt == "" {
		return false
	}
	scope := semcache.Scope{TenantID: tenantID, Model: req.Model, ScoringVersion: events.ScoringVersion}
	minSim := cc.SemanticMinSimilarity(s.semGlobalMinSim)
	serve := false // Shadow=false → a passing L2 candidate is a semantic_hit
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	out := s.semCache.LookupWithOptions(ctx, scope, prompt, semcache.LookupOptions{Shadow: &serve, MinSimilarity: &minSim})
	if out.State != semcache.StateSemanticHit || out.Entry == nil {
		// Not a hit — hand the near-miss/would-hit to the judge (FPR signal) and
		// let the caller fall through to a live vendor call + store.
		s.enqueueForJudge(scope, prompt, out, cc)
		return false
	}
	resp, ok := writeRawCachedResponse(w, r, out.Entry.Response, semcache.StateSemanticHit)
	if !ok {
		return false // corrupt entry → fall through to a live call
	}
	middleware.StampCacheState(r.Context(), semcache.StateSemanticHit)
	middleware.StampCacheSemantic(r.Context(), out.Similarity, out.Threshold)
	// Savings — a probabilistic saving, reported separately from exact hits (§13).
	if resp.Usage != nil {
		cost, _ := clear.DollarCost(vendor, out.Entry.Model, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
		middleware.StampCacheSavings(r.Context(), resp.Usage.PromptTokens, resp.Usage.CompletionTokens, cost)
	}
	// Grade served hits too — that is the sampled-FPR ground truth (§14.1).
	s.enqueueForJudge(scope, prompt, out, cc)
	return true
}

// extractTenantID extracts tenant ID from request context or headers. In AIQG
// mode the tenant is resolved from the Path-A token (not security.GetAuthInfo),
// so that resolution wins — otherwise the response caches would namespace every
// AIQG tenant's entries under one empty-tenant key (a cross-tenant leak).
func (s *Server) extractTenantID(r *http.Request) string {
	if tid := middleware.ResolvedTenantFromContext(r.Context()); tid != "" {
		return tid
	}
	if authInfo, ok := security.GetAuthInfo(r.Context()); ok && authInfo.Metadata != nil {
		if tid, exists := authInfo.Metadata["tenant_id"]; exists {
			return tid
		}
	}
	return r.Header.Get("X-Tenant-ID")
}

// extractUserID extracts user ID from request context or headers.
func (s *Server) extractUserID(r *http.Request) string {
	if authInfo, ok := security.GetAuthInfo(r.Context()); ok {
		return authInfo.UserID
	}
	return r.Header.Get("X-User-ID")
}

// handleCompletion handles legacy OpenAI completion requests (maps to chat completion)
func (s *Server) handleCompletion(w http.ResponseWriter, r *http.Request) {
	// For simplicity, redirect to chat completion
	// In production, you'd implement proper completion endpoint
	s.handleChatCompletion(w, r)
}

// handleMessages (Anthropic Messages API) is implemented in
// anthropic_messages.go — it translates the native Anthropic request/response
// wire shapes at the boundary and reuses the shared completion pipeline.

// handleNonStreamingCompletion handles non-streaming chat completions
func (s *Server) handleNonStreamingCompletion(w http.ResponseWriter, r *http.Request, req *types.ChatRequest, provider providers.LLMProvider, metadata *types.RouterMetadata) {
	resp, err := s.completeWithFallback(r, req, provider, metadata)
	if err != nil {
		s.logger.WithError(err).WithField("provider", metadata.Provider).Error("Chat completion failed")
		s.writeErrorCtx(w, r, http.StatusInternalServerError, fmt.Sprintf("Completion failed: %v", err))
		return
	}

	// AIQG: stamp the vendor-reported token usage + finish_reason
	// onto the routing sidecar so the response event carries
	// TokenAccounting + the clear.Cost / clear.Efficacy dimension
	// scores. No-ops outside AIQG mode.
	if resp.Usage != nil {
		middleware.StampTokenUsage(r.Context(), resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.CacheCreationTokens, resp.Usage.CacheReadTokens)
	}
	if len(resp.Choices) > 0 {
		middleware.StampFinishReason(r.Context(), resp.Choices[0].FinishReason)
		// AIQG (linked tier): tool_call_ids this response served, so a later
		// request echoing them links back to this step.
		middleware.StampServedToolCalls(r.Context(), servedToolCallIDs(resp))
		// AIQG (prefix-chaining tier): hash of the conversation state this
		// response leaves, so a later request re-sending it as prefix links
		// to this turn's conversation.
		middleware.StampStateHash(r.Context(), conversationStateHash(req, resp))
	}
	if metadata != nil {
		middleware.StampRetryMetadata(r.Context(), metadata.AttemptCount, metadata.FallbackUsed)
	}

	// Add routing metadata to response
	if resp.RouterMetadata == nil {
		resp.RouterMetadata = metadata
	}

	s.writeChatResponse(w, r, resp)
}

// setRouterMetadataHeaders publishes the router's routing decision as
// X-TAS-Router-* response headers. This replaces the historical practice of
// injecting a synthetic first SSE chunk carrying router_metadata, which broke
// strict OpenAI SDK stream parsers: that chunk serialized "choices":null plus
// an unexpected router_metadata field. Any TAS client that wants the routing
// decision reads these headers; a stock OpenAI/Anthropic SDK ignores them and
// sees a clean, spec-faithful stream. Must be called before WriteHeader.
func setRouterMetadataHeaders(w http.ResponseWriter, metadata *types.RouterMetadata) {
	if metadata == nil {
		return
	}
	h := w.Header()
	if metadata.Provider != "" {
		h.Set("X-TAS-Router-Provider", metadata.Provider)
	}
	if metadata.Model != "" {
		h.Set("X-TAS-Router-Model", metadata.Model)
	}
	if metadata.RequestID != "" {
		h.Set("X-TAS-Router-Request-Id", metadata.RequestID)
	}
	h.Set("X-TAS-Router-Attempt-Count", strconv.Itoa(metadata.AttemptCount))
	if metadata.FallbackUsed {
		h.Set("X-TAS-Router-Fallback-Used", "true")
	}
	if metadata.EstimatedCost > 0 {
		h.Set("X-TAS-Router-Estimated-Cost", strconv.FormatFloat(metadata.EstimatedCost, 'f', -1, 64))
	}
}

// handleStreamingCompletion handles streaming chat completions
func (s *Server) handleStreamingCompletion(w http.ResponseWriter, r *http.Request, req *types.ChatRequest, provider providers.LLMProvider, metadata *types.RouterMetadata) {
	chunks, err := provider.StreamCompletion(r.Context(), req)
	if err != nil {
		// Fall back to non-streaming if the provider doesn't support streaming
		s.logger.WithError(err).WithField("provider", metadata.Provider).Warn("Streaming not supported, falling back to non-streaming")
		s.handleNonStreamingCompletion(w, r, req, provider, metadata)
		return
	}

	// AIQG: stamp the routing-layer retry metadata (drives the MVP
	// Reliability score). For streaming the metadata is fixed at this
	// point — the retry chain ran fully before StreamCompletion was
	// invoked, so the AttemptCount + FallbackUsed are final.
	if metadata != nil {
		middleware.StampRetryMetadata(r.Context(), metadata.AttemptCount, metadata.FallbackUsed)
	}

	// Publish the routing decision as X-TAS-Router-* response headers instead
	// of a synthetic first SSE chunk (which broke strict OpenAI SDK stream
	// parsers). Must be set before WriteHeader.
	setRouterMetadataHeaders(w, metadata)

	// Set up SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Stream chunks through the wire-format encoder selected by the request
	// context (OpenAI `data:`/[DONE] SSE by default, native Anthropic
	// named-event SSE for /v1/messages). Token-usage + finish_reason stamping
	// stays here so both formats feed the AIQG event pipeline identically.
	enc := s.newStreamEncoder(w, r, req)
	for chunk := range chunks {
		if chunk.Usage != nil {
			middleware.StampTokenUsage(r.Context(), chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens, chunk.Usage.CacheCreationTokens, chunk.Usage.CacheReadTokens)
		}
		for _, c := range chunk.Choices {
			if c.FinishReason != "" {
				middleware.StampFinishReason(r.Context(), c.FinishReason)
				break
			}
		}
		enc.writeChunk(chunk)
	}
	enc.done()
}

// handleNonStreamingCompletionWithRetry handles non-streaming completions with retry/fallback
func (s *Server) handleNonStreamingCompletionWithRetry(w http.ResponseWriter, r *http.Request, req *types.ChatRequest, initialProvider providers.LLMProvider, metadata *types.RouterMetadata) {
	var resp *types.ChatResponse
	var err error

	// Perform actual completion with retry logic
	resp, err = s.attemptCompletionWithRetryAndFallback(r.Context(), req, initialProvider, metadata)
	if err != nil {
		s.logger.WithError(err).WithField("provider", metadata.Provider).Error("All completion attempts failed")
		s.writeErrorCtx(w, r, http.StatusInternalServerError, fmt.Sprintf("Completion failed: %v", err))
		return
	}

	// Gatekeeper: outbound scan (non-streaming only)
	if s.gatekeeper != nil && s.gatekeeperConfig != nil && s.gatekeeperConfig.Outbound.Enabled {
		responseContent := extractResponseContent(resp)
		if responseContent != "" {
			meta := gatekeeper.ScanMeta{
				TenantID:  s.extractTenantID(r),
				RequestID: req.ID,
				UserID:    s.extractUserID(r),
				Source:    "llm_output",
			}

			result, scanErr := s.gatekeeper.ScanResponse(r.Context(), responseContent, meta)
			if scanErr != nil {
				s.logger.WithError(scanErr).WithField("request_id", req.ID).Error("Outbound scan failed")
				if !s.gatekeeperConfig.FailOpen {
					s.writeErrorCtx(w, r, http.StatusInternalServerError, "response content scan failed")
					return
				}
			} else if s.gatekeeper.ShouldBlock(result, "outbound") {
				msg := gatekeeper.FormatBlockMessage(result, "Outbound")
				s.logger.WithField("request_id", req.ID).Warn(msg)
				s.writeErrorCtx(w, r, http.StatusForbidden, "response blocked by content policy")
				return
			}

			// AIQG: project outbound findings to per-severity counts
			// and stamp on the routing sidecar. Same pattern as the
			// inbound stamp in handleChatCompletion. Empty counts
			// still stamp so the outbound side participates in the
			// AssuranceSummary even on a clean scan. Per-NIST-
			// characteristic counts get stamped alongside, summed
			// with any inbound NIST counts already on the snapshot.
			if result != nil && result.ScanResult != nil {
				counts := map[string]int{}
				nistCounts := map[string]int{}
				tagCounts := map[string]int{}
				for _, f := range result.ScanResult.Findings {
					counts[string(f.Severity)]++
					nistCounts[middleware.MapPatternToNIST(f.PatternID)]++
					tagCounts[f.PatternID]++
				}
				middleware.StampGatekeeperFindings(r.Context(), middleware.GatekeeperDirectionOutbound, counts)
				middleware.StampNISTFindings(r.Context(), nistCounts)
				middleware.StampTagFindings(r.Context(), tagCounts)
			}
		}
	}

	// AIQG: stamp the vendor-reported token usage + finish_reason
	// (same as the non-retry path) so the response event carries
	// TokenAccounting + Cost + Efficacy scores.
	if resp.Usage != nil {
		middleware.StampTokenUsage(r.Context(), resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.CacheCreationTokens, resp.Usage.CacheReadTokens)
	}
	if len(resp.Choices) > 0 {
		middleware.StampFinishReason(r.Context(), resp.Choices[0].FinishReason)
		// AIQG (linked tier): tool_call_ids this response served, so a later
		// request echoing them links back to this step.
		middleware.StampServedToolCalls(r.Context(), servedToolCallIDs(resp))
		// AIQG (prefix-chaining tier): hash of the conversation state this
		// response leaves, so a later request re-sending it as prefix links
		// to this turn's conversation.
		middleware.StampStateHash(r.Context(), conversationStateHash(req, resp))
	}
	if metadata != nil {
		middleware.StampRetryMetadata(r.Context(), metadata.AttemptCount, metadata.FallbackUsed)
	}

	// AIQG LLM-as-judge (§6.6): fire a sampled, async quality score off the
	// hot path. No-op when judging is disabled / unsampled / not AIQG.
	s.judge.maybeJudge(r.Context(), w, req, resp)

	// AIQG C1: store the post-outbound-scan response for the next equivalent
	// request. Runs before RouterMetadata is attached so the cached body carries
	// no request-specific routing info. No-op unless the lookup marked this a
	// cacheable miss (PendingFromContext) and the produced response is itself
	// cacheable (no tool calls).
	s.maybeStoreInCache(r, resp)
	// C4: index the response for future semantic hits (async, best-effort).
	s.storeSemantic(r, resp)

	// Add routing metadata to response
	resp.RouterMetadata = metadata

	s.writeChatResponse(w, r, resp)
}

// maybeStoreInCache persists a produced response under the key stamped at lookup
// time. Best-effort and off the client's latency path: a store failure is logged
// and swallowed. Skips responses over MaxBodyBytes and any response carrying tool
// calls (ResponseCacheable).
func (s *Server) maybeStoreInCache(r *http.Request, resp *types.ChatResponse) {
	if s.respCache == nil || !s.respCacheCfg.Enabled {
		return
	}
	p, ok := responsecache.PendingFromContext(r.Context())
	if !ok || !responsecache.ResponseCacheable(resp) {
		return
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return
	}
	if s.respCacheCfg.MaxBodyBytes > 0 && len(body) > s.respCacheCfg.MaxBodyBytes {
		return
	}
	entry := &responsecache.Entry{
		Response:       body,
		Model:          resp.Model,
		ScoringVersion: events.ScoringVersion,
		StoredAtUnix:   time.Now().Unix(),
	}
	if rt := middleware.RoutingFromContext(r.Context()); rt != nil {
		entry.Vendor = rt.Snapshot().Vendor // debug aid; vendor is already folded into the key
	}
	if resp.Usage != nil {
		entry.PromptTokens = resp.Usage.PromptTokens
		entry.CompletionTokens = resp.Usage.CompletionTokens
	}
	if err := s.respCache.Set(r.Context(), p.TenantID, p.Hash, entry, s.respCacheCfg.TTL); err != nil {
		s.logger.WithError(err).WithField("request_id", resp.ID).Warn("response cache store failed")
	}
}

// echoedToolCallIDs returns the tool_call_ids this request echoes back in
// role=tool messages — the linked-tier resolve key (the request is provably
// the next step of the flow whose response served those ids).
func echoedToolCallIDs(req *types.ChatRequest) []string {
	if req == nil {
		return nil
	}
	var ids []string
	for _, m := range req.Messages {
		if m.ToolCallID != "" {
			ids = append(ids, m.ToolCallID)
		}
	}
	return ids
}

// servedToolCallIDs returns the tool_call_ids our response minted — the
// linked-tier index key (a later request echoing them links to this step).
func servedToolCallIDs(resp *types.ChatResponse) []string {
	if resp == nil {
		return nil
	}
	var ids []string
	for _, ch := range resp.Choices {
		for _, tc := range ch.Message.ToolCalls {
			if tc.ID != "" {
				ids = append(ids, tc.ID)
			}
		}
	}
	return ids
}

// hashMessages returns a canonical SHA-256 over a message sequence, projecting
// each message to (role, content, tool_call_id). json.Marshal sorts map keys,
// so multimodal content (decoded as map[string]interface{}) hashes
// deterministically, and extra fields on a client-echoed assistant message
// (refusal, annotations, …) are ignored — so the hash of a state we serve
// matches the prefix the client re-sends verbatim on the next turn.
func hashMessages(msgs []types.Message) string {
	type nm struct {
		Role       string      `json:"r"`
		Content    interface{} `json:"c,omitempty"`
		ToolCallID string      `json:"t,omitempty"`
	}
	norm := make([]nm, 0, len(msgs))
	for _, m := range msgs {
		norm = append(norm, nm{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID})
	}
	b, err := json.Marshal(norm)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// conversationPrefixHash hashes the conversation state this request CONTINUES:
// all messages up to and including the last assistant message — the boundary
// of the prior served turn. Empty when there's no assistant message (a fresh
// thread: nothing to chain back to). Prefix-chaining resolve key (§A.2).
func conversationPrefixHash(req *types.ChatRequest) string {
	if req == nil {
		return ""
	}
	last := -1
	for i, m := range req.Messages {
		if m.Role == "assistant" {
			last = i
		}
	}
	if last < 0 {
		return ""
	}
	return hashMessages(req.Messages[:last+1])
}

// conversationStateHash hashes the conversation state serving this response
// LEAVES behind: the request's messages plus the assistant turn we returned.
// A later request whose prefix hash equals this is provably its next turn.
// Empty for a tool-call-only response (no assistant text) — the tool_call_id
// echo tier (§A.1) handles those. Prefix-chaining index key.
func conversationStateHash(req *types.ChatRequest, resp *types.ChatResponse) string {
	if req == nil || resp == nil {
		return ""
	}
	content := extractResponseContent(resp)
	if content == "" {
		return ""
	}
	msgs := make([]types.Message, 0, len(req.Messages)+1)
	msgs = append(msgs, req.Messages...)
	msgs = append(msgs, types.Message{Role: "assistant", Content: content})
	return hashMessages(msgs)
}

// cachePrefixHash hashes the span a vendor prompt-cache breakpoint would cover:
// the tools+system tier (docs/AIQG-PROMPT-CACHE-CONTROL.md §4.1). Anthropic
// renders tools → system → messages, so a breakpoint on the last system block
// caches tools and system together — this hashes exactly that span, and nothing
// after it. Empty when there is nothing cacheable to measure (no system text
// and no tools): a bare chat has no stable prefix, and hashing one anyway would
// collapse every ad-hoc call onto a single hash and report a phantom reuse rate.
//
// Measurement only (P0) — nothing here reaches the vendor.
//
// Included and why:
//   - model: vendor caches are model-scoped, so a switch is a full rebuild, not
//     a hit. Our own routing can switch models mid-conversation (§5.1), which is
//     precisely the effect this must not hide.
//   - tools: sorted by name, since array order is not semantic to the caller but
//     IS to the byte-level prefix match. Reordering alone would read as a miss.
//   - system: the concatenated role=system text, in order — order IS semantic
//     here (it is the rendered prefix).
//
// Excluded and why: sampling params, max_tokens, seed, stop, response_format.
// None of them render into the tools+system span, and per the vendor's
// invalidation hierarchy a tool_choice/thinking change preserves the tools and
// system tiers. Including them would split one cacheable prefix into many and
// under-report reuse — the opposite of agentFingerprint's goal, which is why
// this is a separate hash rather than a reuse of that one.
func cachePrefixHash(req *types.ChatRequest) string {
	if req == nil {
		return ""
	}

	var system strings.Builder
	for _, m := range req.Messages {
		if m.Role != "system" {
			continue
		}
		// Only text system content renders into the cacheable span; a
		// non-string body (multimodal) isn't a system prompt Anthropic accepts
		// (provider.go rejects it), so it cannot be part of this measurement.
		if s, ok := m.Content.(string); ok {
			system.WriteString(s)
			system.WriteString("\x00") // separator: keeps ["ab","c"] ≠ ["a","bc"]
		}
	}

	type toolSig struct {
		Name   string      `json:"n"`
		Desc   string      `json:"d,omitempty"`
		Params interface{} `json:"p,omitempty"`
	}
	var tools []toolSig
	for _, t := range req.Tools {
		tools = append(tools, toolSig{Name: t.Function.Name, Desc: t.Function.Description, Params: t.Function.Parameters})
	}
	for _, f := range req.Functions {
		tools = append(tools, toolSig{Name: f.Name, Desc: f.Description, Params: f.Parameters})
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })

	if system.Len() == 0 && len(tools) == 0 {
		return ""
	}

	sig := struct {
		Model  string    `json:"m,omitempty"`
		System string    `json:"s,omitempty"`
		Tools  []toolSig `json:"t,omitempty"`
	}{Model: req.Model, System: system.String(), Tools: tools}

	b, err := json.Marshal(sig)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// agentFingerprint computes the request's structural signature for the
// fingerprinted tier (§B): the toolset (sorted tool/function names + their
// param schemas) plus the config shape (model, sampling params, stop,
// response_format). Programmatic agents pin these per code path and rarely
// randomize them, so the same signature recurring is strong evidence of the
// same agent type — even with zero identifying headers.
//
// Returns "" for a request with no fingerprintable structure (no tools /
// functions / response_format / stop): a bare chat (model + free-text
// messages) is too generic to attribute, and fingerprinting it would collapse
// every ad-hoc call into one phantom agent. max_tokens and seed are
// deliberately excluded — they're the config fields most likely to vary
// per-call, which would wrongly split one agent into many.
func agentFingerprint(req *types.ChatRequest) string {
	if req == nil {
		return ""
	}
	if len(req.Tools) == 0 && len(req.Functions) == 0 && req.ResponseFormat == nil && len(req.Stop) == 0 {
		return ""
	}
	type toolSig struct {
		Name   string      `json:"n"`
		Params interface{} `json:"p,omitempty"`
	}
	var tools []toolSig
	for _, t := range req.Tools {
		tools = append(tools, toolSig{Name: t.Function.Name, Params: t.Function.Parameters})
	}
	for _, f := range req.Functions {
		tools = append(tools, toolSig{Name: f.Name, Params: f.Parameters})
	}
	// Sort so tool-array reordering doesn't change the identity.
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })

	stop := append([]string(nil), req.Stop...)
	sort.Strings(stop)

	var rf interface{}
	if req.ResponseFormat != nil {
		rf = req.ResponseFormat
	}

	sig := struct {
		Tools          []toolSig   `json:"tools,omitempty"`
		Model          string      `json:"model,omitempty"`
		Temperature    *float32    `json:"temp,omitempty"`
		TopP           *float32    `json:"top_p,omitempty"`
		FreqPenalty    *float32    `json:"freq_pen,omitempty"`
		PresPenalty    *float32    `json:"pres_pen,omitempty"`
		Stop           []string    `json:"stop,omitempty"`
		ResponseFormat interface{} `json:"rf,omitempty"`
	}{
		Tools:          tools,
		Model:          req.Model,
		Temperature:    req.Temperature,
		TopP:           req.TopP,
		FreqPenalty:    req.FrequencyPenalty,
		PresPenalty:    req.PresencePenalty,
		Stop:           stop,
		ResponseFormat: rf,
	}
	b, err := json.Marshal(sig)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// applyExperimentOverride mutates the request per a running experiment's
// variant override (Phase D). Supports the model-swap (the canonical case —
// req.Model change reroutes via the model→provider mapping) and param-sweep
// axes. A vendor-only override (no model) is not applied here — that needs a
// forced-provider Route() arg; logged as a no-op by the caller if needed.
func applyExperimentOverride(req *types.ChatRequest, override json.RawMessage) {
	if len(override) == 0 {
		return
	}
	var o struct {
		Model  string `json:"model"`
		Params struct {
			Temperature *float32 `json:"temperature"`
			TopP        *float32 `json:"top_p"`
			MaxTokens   *int     `json:"max_tokens"`
		} `json:"params"`
	}
	if err := json.Unmarshal(override, &o); err != nil {
		return
	}
	if o.Model != "" {
		req.Model = o.Model
	}
	if o.Params.Temperature != nil {
		req.Temperature = o.Params.Temperature
	}
	if o.Params.TopP != nil {
		req.TopP = o.Params.TopP
	}
	if o.Params.MaxTokens != nil {
		req.MaxTokens = o.Params.MaxTokens
	}
}

// messageContentString flattens a Message.Content (string or
// []ContentPart for multimodal) into plain text for size/relevance
// measurement. Non-text parts (images) contribute nothing.
func messageContentString(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []types.ContentPart:
		var b strings.Builder
		for _, p := range v {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	case []interface{}:
		// JSON-decoded multimodal content arrives as []interface{} of maps.
		var b strings.Builder
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				if t, _ := m["text"].(string); t != "" {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	default:
		return ""
	}
}

// messagesByteLen is a coarse byte size of the full prompt, used only for
// the reduction MinTokens size gate.
func messagesByteLen(messages []types.Message) int {
	n := 0
	for _, m := range messages {
		n += len(messageContentString(m.Content))
	}
	return n
}

// lastUserText returns the text of the latest user-role message — the
// relevance anchor for shadow extraction.
func lastUserText(messages []types.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messageContentString(messages[i].Content)
		}
	}
	return ""
}

// sampleHit reports whether this request is in the shadow-measurement
// sample. A rate <= 0 means "measure all eligible traffic" (the natural
// default for a shadow policy with no explicit sample_rate); >= 1 also
// always hits.
func sampleHit(rate float64) bool {
	if rate <= 0 || rate >= 1 {
		return true
	}
	return rand.Float64() < rate
}

// parseOverrideReduction pulls a `reduction` block out of an experiment
// variant's override JSON and parses it as a ReductionPolicy. Returns nil
// when the override has no reduction config (the common model-swap case).
func parseOverrideReduction(override json.RawMessage) *policy.ReductionPolicy {
	if len(override) == 0 {
		return nil
	}
	var o struct {
		Reduction json.RawMessage `json:"reduction"`
	}
	if err := json.Unmarshal(override, &o); err != nil || len(o.Reduction) == 0 {
		return nil
	}
	rp, err := policy.ParseReduction(o.Reduction)
	if err != nil {
		return nil
	}
	return rp
}

// Retired (Phase 0, AIQG_CACHE_SAFE_REDUCTION.md §6): largestReducibleMessageIndex
// + applyReductionInline performed single-pass, in-place, per-turn reduction of
// the largest historical message. Editing already-cached content every turn with
// a query-dependent rule busted prompt caching (cheap 0.10× reads flipped back to
// full/creation rate). The gateway is now read-only (measures, never mutates);
// active application of reduction moves to the MCP proxy, at the source, once
// (§2/§9.A). gatekeeper.ReduceText remains for that at-source path.

// extractResponseContent extracts text content from a ChatResponse.
func extractResponseContent(resp *types.ChatResponse) string {
	if resp == nil {
		return ""
	}
	var parts []string
	for _, choice := range resp.Choices {
		switch v := choice.Message.Content.(type) {
		case string:
			if v != "" {
				parts = append(parts, v)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// handleStreamingCompletionWithRetry handles streaming completions with retry/fallback
func (s *Server) handleStreamingCompletionWithRetry(w http.ResponseWriter, r *http.Request, req *types.ChatRequest, initialProvider providers.LLMProvider, metadata *types.RouterMetadata) {
	// For streaming, we'll use the first successful provider (no mid-stream retry)
	var chunks <-chan *types.ChatChunk
	var err error

	chunks, err = s.attemptStreamingWithFallback(r.Context(), req, initialProvider, metadata)
	if err != nil {
		s.logger.WithError(err).WithField("provider", metadata.Provider).Error("All streaming attempts failed")
		s.writeErrorCtx(w, r, http.StatusInternalServerError, fmt.Sprintf("Streaming failed: %v", err))
		return
	}

	// AIQG: stamp the routing-layer retry metadata (drives the MVP
	// Reliability score). For streaming the metadata is fixed at this
	// point — the retry chain ran fully before StreamCompletion was
	// invoked, so the AttemptCount + FallbackUsed are final.
	if metadata != nil {
		middleware.StampRetryMetadata(r.Context(), metadata.AttemptCount, metadata.FallbackUsed)
	}

	// Publish the routing decision as X-TAS-Router-* response headers instead
	// of a synthetic first SSE chunk (which broke strict OpenAI SDK stream
	// parsers). Must be set before WriteHeader.
	setRouterMetadataHeaders(w, metadata)

	// Set up SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Stream chunks through the wire-format encoder selected by the request
	// context (OpenAI `data:`/[DONE] SSE by default, native Anthropic
	// named-event SSE for /v1/messages). Token-usage + finish_reason stamping
	// stays here so both formats feed the AIQG event pipeline identically.
	enc := s.newStreamEncoder(w, r, req)
	for chunk := range chunks {
		if chunk.Usage != nil {
			middleware.StampTokenUsage(r.Context(), chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens, chunk.Usage.CacheCreationTokens, chunk.Usage.CacheReadTokens)
		}
		for _, c := range chunk.Choices {
			if c.FinishReason != "" {
				middleware.StampFinishReason(r.Context(), c.FinishReason)
				break
			}
		}
		enc.writeChunk(chunk)
	}
	enc.done()
}

// attemptCompletionWithRetryAndFallback performs completion with retry and fallback logic
func (s *Server) attemptCompletionWithRetryAndFallback(ctx context.Context, req *types.ChatRequest, initialProvider providers.LLMProvider, metadata *types.RouterMetadata) (*types.ChatResponse, error) {
	// Try initial provider with retries
	resp, err := s.attemptCompletionWithRetry(ctx, req, initialProvider, metadata.Provider, req.RetryConfig)
	if err == nil {
		return resp, nil
	}

	// Add initial provider to failed list
	metadata.FailedProviders = append(metadata.FailedProviders, metadata.Provider)

	// Try fallback if configured
	if req.FallbackConfig != nil && req.FallbackConfig.Enabled {
		return s.attemptCompletionFallback(ctx, req, metadata)
	}

	return nil, err
}

// attemptStreamingWithFallback performs streaming with fallback (no mid-stream retry)
func (s *Server) attemptStreamingWithFallback(ctx context.Context, req *types.ChatRequest, initialProvider providers.LLMProvider, metadata *types.RouterMetadata) (<-chan *types.ChatChunk, error) {
	// Try initial provider
	chunks, err := initialProvider.StreamCompletion(ctx, req)
	if err == nil {
		return chunks, nil
	}

	// Add initial provider to failed list
	metadata.FailedProviders = append(metadata.FailedProviders, metadata.Provider)

	// Try fallback if configured
	if req.FallbackConfig != nil && req.FallbackConfig.Enabled {
		return s.attemptStreamingFallback(ctx, req, metadata)
	}

	return nil, err
}

// attemptCompletionWithRetry performs completion with retry logic for a single provider
func (s *Server) attemptCompletionWithRetry(ctx context.Context, req *types.ChatRequest, provider providers.LLMProvider, providerName string, retryConfig *types.RetryConfig) (*types.ChatResponse, error) {
	maxAttempts := 1
	if retryConfig != nil {
		maxAttempts = retryConfig.MaxAttempts
		if maxAttempts < 1 {
			maxAttempts = 1
		}
	}

	var lastError error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Apply backoff delay for retries
		if attempt > 1 && retryConfig != nil {
			delay := s.calculateRetryDelay(retryConfig, attempt-1)
			s.logger.WithFields(logrus.Fields{
				"provider": providerName,
				"attempt":  attempt,
				"delay_ms": delay.Milliseconds(),
			}).Debug("Retrying completion after backoff")

			select {
			case <-time.After(delay):
				// Continue with retry
			case <-ctx.Done():
				return nil, fmt.Errorf("request cancelled during retry: %w", ctx.Err())
			}
		}

		// Attempt completion
		resp, err := provider.ChatCompletion(ctx, req)
		s.router.RecordOutcome(ctx, providerName, req.Model, breaker.ClassifyError(err))
		if err == nil {
			return resp, nil
		}

		lastError = err
		s.logger.WithFields(logrus.Fields{
			"provider": providerName,
			"attempt":  attempt,
			"error":    err.Error(),
		}).Warn("Completion attempt failed")

		// Check if error is retryable
		if retryConfig != nil && !s.isRetryableError(err, retryConfig) {
			s.logger.WithField("provider", providerName).Debug("Error not retryable, stopping retries")
			break
		}
	}

	return nil, lastError
}

// attemptCompletionFallback tries fallback providers for completion
func (s *Server) attemptCompletionFallback(ctx context.Context, req *types.ChatRequest, metadata *types.RouterMetadata) (*types.ChatResponse, error) {
	// Get fallback providers from router (this would need to be implemented)
	fallbackProviders := s.getFallbackProviders(req, metadata)

	for _, providerName := range fallbackProviders {
		if contains(metadata.FailedProviders, providerName) {
			continue
		}

		provider, exists := s.router.GetProvider(providerName)
		if !exists {
			continue
		}

		s.logger.WithField("fallback_provider", providerName).Info("Trying fallback provider")

		resp, err := s.attemptCompletionWithRetry(ctx, req, provider, providerName, req.RetryConfig)
		if err == nil {
			metadata.Provider = providerName
			metadata.FallbackUsed = true
			metadata.RoutingReason = append(metadata.RoutingReason, fmt.Sprintf("Fallback to %s", providerName))
			return resp, nil
		}

		metadata.FailedProviders = append(metadata.FailedProviders, providerName)
	}

	return nil, fmt.Errorf("all fallback providers failed")
}

// attemptStreamingFallback tries fallback providers for streaming
func (s *Server) attemptStreamingFallback(ctx context.Context, req *types.ChatRequest, metadata *types.RouterMetadata) (<-chan *types.ChatChunk, error) {
	fallbackProviders := s.getFallbackProviders(req, metadata)

	for _, providerName := range fallbackProviders {
		if contains(metadata.FailedProviders, providerName) {
			continue
		}

		provider, exists := s.router.GetProvider(providerName)
		if !exists {
			continue
		}

		s.logger.WithField("fallback_provider", providerName).Info("Trying fallback streaming provider")

		chunks, err := provider.StreamCompletion(ctx, req)
		if err == nil {
			metadata.Provider = providerName
			metadata.FallbackUsed = true
			metadata.RoutingReason = append(metadata.RoutingReason, fmt.Sprintf("Fallback to %s", providerName))
			return chunks, nil
		}

		metadata.FailedProviders = append(metadata.FailedProviders, providerName)
	}

	return nil, fmt.Errorf("all streaming fallback providers failed")
}

// calculateRetryDelay calculates delay for retry attempts
func (s *Server) calculateRetryDelay(config *types.RetryConfig, attempt int) time.Duration {
	var delay time.Duration

	switch config.BackoffType {
	case "exponential":
		multiplier := float64(uint(1) << uint(attempt)) // 2^attempt
		delay = time.Duration(float64(config.BaseDelay) * multiplier)
	case "linear":
		delay = time.Duration(int64(config.BaseDelay) * int64(attempt+1))
	default:
		// Default to exponential
		multiplier := float64(uint(1) << uint(attempt))
		delay = time.Duration(float64(config.BaseDelay) * multiplier)
	}

	// Cap at MaxDelay
	if config.MaxDelay > 0 && delay > config.MaxDelay {
		delay = config.MaxDelay
	}

	return delay
}

// isRetryableError checks if an error should be retried
func (s *Server) isRetryableError(err error, config *types.RetryConfig) bool {
	if len(config.RetryableErrors) == 0 {
		// Default retryable errors
		errStr := err.Error()
		return strings.Contains(errStr, "timeout") ||
			strings.Contains(errStr, "connection") ||
			strings.Contains(errStr, "unavailable") ||
			strings.Contains(errStr, "rate limit")
	}

	errStr := err.Error()
	for _, retryableError := range config.RetryableErrors {
		if strings.Contains(errStr, retryableError) {
			return true
		}
	}
	return false
}

// getFallbackProviders gets list of fallback providers (placeholder)
func (s *Server) getFallbackProviders(req *types.ChatRequest, metadata *types.RouterMetadata) []string {
	// This is a simplified implementation
	// In practice, this should use the router's fallback chain logic
	providers := s.router.ListProviders()
	var fallbacks []string

	for _, provider := range providers {
		if provider != metadata.Provider {
			fallbacks = append(fallbacks, provider)
		}
	}
	return fallbacks
}

// contains checks if slice contains value (utility function)
func contains(slice []string, value string) bool {
	for _, item := range slice {
		if item == value {
			return true
		}
	}
	return false
}

// handleListProviders lists all registered providers
func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	providers := s.router.ListProviders()

	response := map[string]interface{}{
		"providers": providers,
		"count":     len(providers),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleListModels implements the OpenAI-compatible GET /v1/models endpoint so
// a stock OpenAI SDK's client.models.list() works against the gateway. It
// enumerates every model declared by every configured provider's capabilities,
// dedupes by id, and returns them in OpenAI's {object:"list", data:[…]} shape.
// Not wrapped in AIQG middleware — it's provider metadata, like /v1/providers.
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	caps := s.router.GetCapabilities()

	// The Anthropic SDK's models.list() hits this same path but expects
	// Anthropic's native shape — serve it when the caller sends anthropic-version.
	if anthropicSDKRequest(r) {
		s.writeAnthropicModelList(w, caps)
		return
	}

	// Stable, deterministic output: sort providers, then models, dedupe by id.
	provNames := make([]string, 0, len(caps))
	for name := range caps {
		provNames = append(provNames, name)
	}
	sort.Strings(provNames)

	seen := make(map[string]struct{})
	list := types.OpenAIModelList{Object: "list", Data: []types.OpenAIModel{}}
	for _, pName := range provNames {
		pc := caps[pName]
		owner := pc.ProviderName
		if owner == "" {
			owner = pName
		}
		models := append([]types.ModelInfo(nil), pc.SupportedModels...)
		sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
		for _, m := range models {
			if m.Name == "" {
				continue
			}
			if _, dup := seen[m.Name]; dup {
				continue
			}
			seen[m.Name] = struct{}{}
			list.Data = append(list.Data, types.OpenAIModel{
				ID:      m.Name,
				Object:  "model",
				OwnedBy: owner,
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

// handleGetModel implements the OpenAI-compatible GET /v1/models/{model}
// retrieve endpoint. Returns the first provider that declares the model, in
// OpenAI's model object shape; 404 when no provider offers it.
func (s *Server) handleGetModel(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["model"]
	caps := s.router.GetCapabilities()
	// Anthropic SDK's models.retrieve() expects Anthropic's model shape.
	if anthropicSDKRequest(r) {
		if s.writeAnthropicModel(w, caps, id) {
			return
		}
		s.writeErrorResponse(w, http.StatusNotFound, fmt.Sprintf("Model %s not found", id))
		return
	}
	for pName, pc := range caps {
		owner := pc.ProviderName
		if owner == "" {
			owner = pName
		}
		for _, m := range pc.SupportedModels {
			if m.Name == id {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(types.OpenAIModel{ID: m.Name, Object: "model", OwnedBy: owner})
				return
			}
		}
	}
	s.writeErrorResponse(w, http.StatusNotFound, fmt.Sprintf("Model %s not found", id))
}

// handleGetProvider gets information about a specific provider
func (s *Server) handleGetProvider(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	name := vars["name"]

	provider, exists := s.router.GetProvider(name)
	if !exists {
		s.writeErrorResponse(w, http.StatusNotFound, fmt.Sprintf("Provider %s not found", name))
		return
	}

	response := map[string]interface{}{
		"name":         name,
		"provider":     provider.GetProviderName(),
		"capabilities": provider.GetCapabilities(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleHealthCheck returns overall health status
func (s *Server) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	health := s.router.GetHealthStatus()

	overallHealthy := true
	for _, status := range health {
		if status.Status != "healthy" {
			overallHealthy = false
			break
		}
	}

	response := map[string]interface{}{
		"status": func() string {
			if overallHealthy {
				return "healthy"
			} else {
				return "degraded"
			}
		}(),
		"providers": health,
		"timestamp": time.Now().Unix(),
	}

	statusCode := http.StatusOK
	if !overallHealthy {
		statusCode = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(response)
}

// handleProviderHealth returns health status for specific provider
func (s *Server) handleProviderHealth(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	name := vars["name"]

	health := s.router.GetHealthStatus()
	providerHealth, exists := health[name]
	if !exists {
		s.writeErrorResponse(w, http.StatusNotFound, fmt.Sprintf("Provider %s not found", name))
		return
	}

	response := map[string]interface{}{
		"provider":  name,
		"status":    providerHealth,
		"timestamp": time.Now().Unix(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleCapabilities returns capabilities of all providers
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	capabilities := s.router.GetCapabilities()

	response := map[string]interface{}{
		"capabilities": capabilities,
		"timestamp":    time.Now().Unix(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleRoutingDecision returns routing decision without executing request
func (s *Server) handleRoutingDecision(w http.ResponseWriter, r *http.Request) {
	var req types.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeErrorResponse(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}

	// Generate request ID if not provided
	if req.ID == "" {
		req.ID = fmt.Sprintf("routing-%d", time.Now().UnixNano())
	}
	req.Timestamp = time.Now()

	// Get routing decision
	metadata, _, err := s.router.Route(r.Context(), &req)
	if err != nil {
		s.writeErrorResponse(w, http.StatusServiceUnavailable, fmt.Sprintf("Routing failed: %v", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metadata)
}

// Helper functions

func (s *Server) writeErrorResponse(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	errorResp := map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    "api_error",
			"code":    statusCode,
		},
		"timestamp": time.Now().Unix(),
	}

	json.NewEncoder(w).Encode(errorResp)
}

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Flush implements http.Flusher interface for streaming support
func (rw *responseWriter) Flush() {
	if flusher, ok := rw.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// handleMetrics serves Prometheus metrics endpoint
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	// Basic metrics in Prometheus format
	// This is a minimal implementation for demo purposes
	// In production, this should use the prometheus/client_golang library

	// Get provider health status
	healthStatus := s.router.GetHealthStatus()

	// Generate basic metrics
	metrics := "# HELP llm_router_provider_health Provider health status (1=healthy, 0=unhealthy)\n"
	metrics += "# TYPE llm_router_provider_health gauge\n"

	for provider, health := range healthStatus {
		status := 0
		if health.Status == "healthy" {
			status = 1
		}
		metrics += fmt.Sprintf("llm_router_provider_health{service=\"llm-router\",provider=\"%s\"} %d\n", provider, status)
	}

	// Active connections (mock data for now)
	metrics += "\n# HELP llm_router_active_connections Current number of active connections\n"
	metrics += "# TYPE llm_router_active_connections gauge\n"
	metrics += "llm_router_active_connections{service=\"llm-router\"} 5\n"

	// Request count (incremental mock data based on time)
	now := time.Now().Unix()
	baseRequests := now / 10 // Increments every 10 seconds

	metrics += "\n# HELP llm_router_requests_total Total number of requests\n"
	metrics += "# TYPE llm_router_requests_total counter\n"
	metrics += fmt.Sprintf("llm_router_requests_total{service=\"llm-router\",provider=\"openai\",method=\"POST\",status_code=\"200\",client_ip=\"192.168.1.100\"} %d\n", 150+baseRequests*3)
	metrics += fmt.Sprintf("llm_router_requests_total{service=\"llm-router\",provider=\"anthropic\",method=\"POST\",status_code=\"200\",client_ip=\"192.168.1.101\"} %d\n", 75+baseRequests*2)
	metrics += fmt.Sprintf("llm_router_requests_total{service=\"llm-router\",provider=\"openai\",method=\"POST\",status_code=\"400\",client_ip=\"10.0.0.50\"} %d\n", 5+baseRequests/10)
	metrics += fmt.Sprintf("llm_router_requests_total{service=\"llm-router\",provider=\"openai\",method=\"POST\",status_code=\"200\",client_ip=\"172.16.0.25\"} %d\n", 80+baseRequests*2)
	metrics += fmt.Sprintf("llm_router_requests_total{service=\"llm-router\",provider=\"anthropic\",method=\"POST\",status_code=\"200\",client_ip=\"10.0.0.75\"} %d\n", 45+baseRequests)

	// Token usage (incremental mock data based on time)
	metrics += "\n# HELP llm_router_tokens_total Total number of tokens processed\n"
	metrics += "# TYPE llm_router_tokens_total counter\n"
	metrics += fmt.Sprintf("llm_router_tokens_total{service=\"llm-router\",provider=\"openai\",type=\"input\"} %d\n", 25000+baseRequests*500)
	metrics += fmt.Sprintf("llm_router_tokens_total{service=\"llm-router\",provider=\"openai\",type=\"output\"} %d\n", 15000+baseRequests*300)
	metrics += fmt.Sprintf("llm_router_tokens_total{service=\"llm-router\",provider=\"anthropic\",type=\"input\"} %d\n", 12000+baseRequests*250)
	metrics += fmt.Sprintf("llm_router_tokens_total{service=\"llm-router\",provider=\"anthropic\",type=\"output\"} %d\n", 8000+baseRequests*150)

	// Cost tracking (incremental mock data based on time)
	metrics += "\n# HELP llm_router_cost_total Total cost in USD\n"
	metrics += "# TYPE llm_router_cost_total counter\n"
	metrics += fmt.Sprintf("llm_router_cost_total{service=\"llm-router\",provider=\"openai\",model=\"gpt-4o\"} %.2f\n", 12.50+float64(baseRequests)*0.05)
	metrics += fmt.Sprintf("llm_router_cost_total{service=\"llm-router\",provider=\"anthropic\",model=\"claude-3-sonnet\"} %.2f\n", 8.75+float64(baseRequests)*0.03)

	// Error tracking (mock data)
	metrics += "\n# HELP llm_router_errors_total Total number of errors\n"
	metrics += "# TYPE llm_router_errors_total counter\n"
	metrics += "llm_router_errors_total{service=\"llm-router\",provider=\"openai\",error_type=\"timeout\"} 2\n"
	metrics += "llm_router_errors_total{service=\"llm-router\",provider=\"anthropic\",error_type=\"rate_limit\"} 1\n"

	// Rate limiting (mock data)
	metrics += "\n# HELP llm_router_rate_limit_usage Rate limit usage as fraction (0-1)\n"
	metrics += "# TYPE llm_router_rate_limit_usage gauge\n"
	metrics += "llm_router_rate_limit_usage{service=\"llm-router\",provider=\"openai\"} 0.65\n"
	metrics += "llm_router_rate_limit_usage{service=\"llm-router\",provider=\"anthropic\"} 0.32\n"

	// Security metrics (incremental mock data)
	metrics += "\n# HELP llm_router_auth_attempts_total Total authentication attempts\n"
	metrics += "# TYPE llm_router_auth_attempts_total counter\n"
	metrics += fmt.Sprintf("llm_router_auth_attempts_total{service=\"llm-router\",result=\"success\"} %d\n", 220+baseRequests*8)
	metrics += fmt.Sprintf("llm_router_auth_attempts_total{service=\"llm-router\",result=\"failure\"} %d\n", 8+baseRequests/15)

	// Security score (mock data)
	metrics += "\n# HELP llm_router_security_score Security score (0-100)\n"
	metrics += "# TYPE llm_router_security_score gauge\n"
	metrics += "llm_router_security_score{service=\"llm-router\"} 85\n"

	// Threat level (mock data)
	metrics += "\n# HELP llm_router_threat_level Current threat level (0-3)\n"
	metrics += "# TYPE llm_router_threat_level gauge\n"
	metrics += "llm_router_threat_level{service=\"llm-router\"} 0\n"

	// Rate limiting hits (incremental mock data)
	metrics += "\n# HELP llm_router_rate_limit_hits_total Total rate limit hits\n"
	metrics += "# TYPE llm_router_rate_limit_hits_total counter\n"
	metrics += fmt.Sprintf("llm_router_rate_limit_hits_total{service=\"llm-router\",tier=\"premium\"} %d\n", 10+baseRequests/20)
	metrics += fmt.Sprintf("llm_router_rate_limit_hits_total{service=\"llm-router\",tier=\"standard\"} %d\n", 25+baseRequests/10)

	// Blocked requests (incremental mock data)
	metrics += "\n# HELP llm_router_blocked_requests_total Total blocked requests\n"
	metrics += "# TYPE llm_router_blocked_requests_total counter\n"
	metrics += fmt.Sprintf("llm_router_blocked_requests_total{service=\"llm-router\",reason=\"rate_limit\"} %d\n", 5+baseRequests/30)
	metrics += fmt.Sprintf("llm_router_blocked_requests_total{service=\"llm-router\",reason=\"auth_failure\"} %d\n", 3+baseRequests/50)

	// Security events (incremental mock data)
	metrics += "\n# HELP llm_router_security_events_total Total security events\n"
	metrics += "# TYPE llm_router_security_events_total counter\n"
	metrics += fmt.Sprintf("llm_router_security_events_total{service=\"llm-router\",event_type=\"suspicious_activity\",severity=\"medium\"} %d\n", 2+baseRequests/100)
	metrics += fmt.Sprintf("llm_router_security_events_total{service=\"llm-router\",event_type=\"malicious_input\",severity=\"high\"} %d\n", 1+baseRequests/200)

	// Validation failures (incremental mock data)
	metrics += "\n# HELP llm_router_validation_failures_total Total validation failures\n"
	metrics += "# TYPE llm_router_validation_failures_total counter\n"
	metrics += fmt.Sprintf("llm_router_validation_failures_total{service=\"llm-router\",type=\"schema\"} %d\n", 8+baseRequests/25)
	metrics += fmt.Sprintf("llm_router_validation_failures_total{service=\"llm-router\",type=\"content\"} %d\n", 12+baseRequests/15)

	// Input sanitization (incremental mock data)
	metrics += "\n# HELP llm_router_input_sanitized_total Total inputs sanitized\n"
	metrics += "# TYPE llm_router_input_sanitized_total counter\n"
	metrics += fmt.Sprintf("llm_router_input_sanitized_total{service=\"llm-router\"} %d\n", 45+baseRequests*2)

	// Audit events (incremental mock data)
	metrics += "\n# HELP llm_router_audit_events_total Total audit events\n"
	metrics += "# TYPE llm_router_audit_events_total counter\n"
	metrics += fmt.Sprintf("llm_router_audit_events_total{service=\"llm-router\",event_type=\"api_key_usage\",severity=\"low\",user_id=\"user123\"} %d\n", 150+baseRequests*5)
	metrics += fmt.Sprintf("llm_router_audit_events_total{service=\"llm-router\",event_type=\"config_change\",severity=\"medium\",user_id=\"admin\"} %d\n", 3+baseRequests/50)

	// Active API keys (mock data)
	metrics += "\n# HELP llm_router_active_api_keys Number of active API keys\n"
	metrics += "# TYPE llm_router_active_api_keys gauge\n"
	metrics += "llm_router_active_api_keys{service=\"llm-router\"} 12\n"

	fmt.Fprint(w, metrics)
}

// toMiddlewareExclusions converts routing's gate exclusions to the middleware
// sidecar's shape. Two conversions rather than a shared type, so neither the
// routing package nor the middleware depends on the event schema.
func toMiddlewareExclusions(in []routing.ExcludedCandidate) []middleware.GateExclusion {
	if len(in) == 0 {
		return nil
	}
	out := make([]middleware.GateExclusion, len(in))
	for i, e := range in {
		out[i] = middleware.GateExclusion{
			Provider: e.Provider, Model: e.Model,
			Dimension: e.Dimension, Reason: e.Reason,
		}
	}
	return out
}

// toScanFinding projects a Gatekeeper finding into the audit shape.
//
// The matched VALUE is deliberately not copied. Gatekeeper's own type carries
// the comment "never log actual PII" and supplies ValuePreview and ValueHash for
// exactly this purpose: an audit trail proving an SSN would have been redacted
// must not become the place that SSN is stored.
func toScanFinding(f scan.Finding, direction string) middleware.ScanFinding {
	frameworks := make([]string, 0, len(f.Frameworks))
	for _, fw := range f.Frameworks {
		frameworks = append(frameworks, string(fw.Framework))
	}
	return middleware.ScanFinding{
		PatternID:    f.PatternID,
		Severity:     string(f.Severity),
		Direction:    direction,
		FieldPath:    f.Location.FieldPath,
		Offset:       f.Location.Offset,
		Length:       f.Location.Length,
		ValuePreview: f.ValuePreview,
		ValueHash:    f.ValueHash,
		Confidence:   f.Confidence,
		Redacted:     f.Redacted,
		Frameworks:   frameworks,
	}
}
