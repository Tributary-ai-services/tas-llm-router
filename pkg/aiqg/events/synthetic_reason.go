package events

// SyntheticGatewayEval marks an event the gateway produced for its own
// evaluation machinery — an LLM-as-judge score or a shadow-eval replay
// (tas-llm-router#184).
//
// It is a third reason alongside the resilience package's "declared" and
// "source_app". Both of those describe CUSTOMER traffic that was identified as
// non-production: one because the caller said so, one because the gateway
// guessed from the source app. Gateway-initiated evaluation is neither — no
// customer sent it, and no header declared it — so folding it into "declared"
// would assert something untrue about who marked it, and folding it into real
// traffic would put the gateway's own spend on the customer's usage curve.
//
// Deliberately defined here rather than in aether-shared/go-aiqg-resilience:
// that module is shared by other services, the reason is an open string set
// with nothing validating it at write time, and only this gateway produces
// evaluation calls. Move it there if a second service ever needs it.
const SyntheticGatewayEval = "gateway_eval"
