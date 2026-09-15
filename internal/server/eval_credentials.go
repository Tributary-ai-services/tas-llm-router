package server

import (
	"context"
	"errors"

	"github.com/tributary-ai/llm-router-waf/internal/upstreamkey"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/metrics"
)

// errEvalSkippedBYOK reports that an evaluation was not attempted because the
// tenant is BYOK-only and has no stored key for the vendor it would have used.
//
// A sentinel rather than a plain error because callers must tell it apart from
// a failure: nothing was called and nothing was billed, so counting it as a
// failed replay or a failed judge call would make a policy the gateway
// correctly respected look like a bug it has.
var errEvalSkippedBYOK = errors.New("aiqg eval: skipped, tenant is BYOK-only with no stored key")

// evalKeyDecision is what the credential check concluded for one evaluation
// call: the context to make the call with, and whether to make it at all.
type evalKeyDecision struct {
	ctx  context.Context
	skip bool
}

// resolveEvalKey decides whose key an evaluation call should bill (#184).
//
// The gateway's judge and shadow-replay calls have always used the gateway's
// own configured provider key. That is wrong for a BYOK tenant twice over:
// the spend lands on the wrong account, and a customer who brought their own
// key never agreed to the gateway spending its key on their traffic.
//
// Four outcomes, and the refusal is the one that matters:
//
//   - a stored key exists          → bill the tenant's own key
//   - none, shared fallback allowed → the configured key; the tenant declared
//     that fallback acceptable, so this is consent, not assumption
//   - none, BYOK-ONLY               → SKIP the evaluation entirely
//   - resolver error                → the configured key, degrading exactly as
//     the request path does rather than dropping the evaluation over a blip
//
// The BYOK-only skip is deliberate. Hard-failing would be wrong — no customer
// is waiting on an evaluation — and quietly using the shared key would violate
// an explicit policy in order to run something the tenant never requested. Not
// evaluating is the only outcome that respects what they declared, and it is
// counted so a BYOK-only tenant's missing evaluations are visible rather than
// simply absent from coverage.
//
// Called AFTER Route, because the key is per-(tenant, vendor) and the vendor is
// only known once routing has picked a provider. Providers read the override
// per call via upstreamkey.From in clientFor, so setting it on the context
// reaches the outbound request.
func (jr *judgeRunner) resolveEvalKey(ctx context.Context, a *evalAttribution, vendor string) evalKeyDecision {
	// BYOK unconfigured, or an unattributed call: nothing to resolve against.
	// Pre-#184 behaviour — the configured key — with no source recorded,
	// because there was no tenant to have preferred otherwise.
	if jr == nil || jr.creds == nil || a == nil || a.TenantID == "" {
		return evalKeyDecision{ctx: ctx}
	}

	res, err := jr.creds.Resolve(ctx, a.TenantID, vendor)
	if err != nil {
		metrics.EvalCredentialSourceTotal.WithLabelValues(a.Path, metrics.EvalCredResolverErr).Inc()
		jr.log.WithError(err).WithField("vendor", vendor).
			Debug("aiqg eval: BYOK resolve failed; using the configured key")
		return evalKeyDecision{ctx: ctx}
	}

	if res.Found {
		metrics.EvalCredentialSourceTotal.WithLabelValues(a.Path, metrics.EvalCredTenantStored).Inc()
		return evalKeyDecision{ctx: upstreamkey.With(ctx, res.APIKey)}
	}

	if !res.AllowSharedFallback {
		metrics.JudgeExcludedTotal.WithLabelValues(metrics.JudgeExcludedBYOKOnly).Inc()
		return evalKeyDecision{skip: true}
	}

	metrics.EvalCredentialSourceTotal.WithLabelValues(a.Path, metrics.EvalCredTASShared).Inc()
	return evalKeyDecision{ctx: ctx}
}
