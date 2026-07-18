package semcache

import (
	"regexp"
	"strings"
	"time"
)

// VerifyResult is the L2 outcome for one candidate.
type VerifyResult struct {
	Pass   bool
	Reason string // the first guard that failed (empty on pass)
}

// Verify is the L2 verification gate (docs/AIQG-SEMANTIC-CACHING.md §5). A
// candidate surfaced by L1 similarity is only a semantic hit if it passes ALL of
// these cheap, deterministic, synchronous guards. They are lexical by design —
// the failure mode is embedding separation, so the gate uses a different sensor
// (entities/numbers/dates/negation) than the one that generated the candidate.
//
// incoming is the post-redaction prompt of the live request; cand is the stored
// entry. Order of guards is cheap→discriminative; the first failure short-circuits.
func Verify(incoming string, cand *Entry, scope Scope, now time.Time, ttl time.Duration) VerifyResult {
	// d. Scope — exact tenant/model/scoring match. L1 already filtered on these;
	// re-check as defense in depth (a cross-tenant hit is the worst failure, §8).
	if cand.TenantID != scope.TenantID || cand.Model != scope.Model || cand.ScoringVersion != scope.ScoringVersion {
		return VerifyResult{Reason: "scope"}
	}
	// c. Freshness — entry age within TTL.
	if ttl > 0 && cand.CreatedAtUnix > 0 {
		if now.Unix()-cand.CreatedAtUnix > int64(ttl.Seconds()) {
			return VerifyResult{Reason: "freshness"}
		}
	}
	// b. Negation — polarity must agree. "is X safe" must not hit "is X not safe".
	if negationParity(incoming) != negationParity(cand.Prompt) {
		return VerifyResult{Reason: "negation"}
	}
	// a. Entity/number/date guard — the discriminative tokens must match exactly.
	// Kills "Chase Sapphire" vs "Chase Sapphire Reserve" (cosine 0.99), model
	// versions, SKUs, plan tiers, dates — which no similarity threshold reaches.
	if !discriminativeTokensMatch(incoming, cand.Prompt) {
		return VerifyResult{Reason: "entity_number_date"}
	}
	return VerifyResult{Pass: true}
}

var (
	// numberish: integers, decimals, money, versions, dates, ranges (4111, 4.5,
	// 2026-07-18, v3, 123-45-6789 already redacted). Discriminative by definition.
	reNumberish = regexp.MustCompile(`\d[\w.\-/]*`)
	// tokenSplit: words for entity extraction.
	reWord = regexp.MustCompile(`[A-Za-z][A-Za-z'\-]*`)
	// acronym: 2+ all-caps (SKU, PDF, GPT). Kept as discriminative entities.
	reAcronym = regexp.MustCompile(`^[A-Z0-9]{2,}$`)
)

// negationWords are the polarity markers. n't is handled by contraction below.
var negationWords = map[string]bool{
	"not": true, "no": true, "never": true, "without": true, "cannot": true,
	"none": true, "nor": true, "neither": true, "nothing": true,
}

// negationParity returns count(negations) % 2, so a double negative ("not
// unsafe") reads the same polarity as none. Contractions (isn't, don't, can't)
// each count once.
func negationParity(s string) int {
	l := strings.ToLower(s)
	n := strings.Count(l, "n't") // isn't/don't/can't/won't/…
	for _, w := range reWord.FindAllString(l, -1) {
		if negationWords[w] {
			n++
		}
	}
	return n % 2
}

// discriminativeTokens returns the set of tokens that carry entity/number/date
// meaning — numbers/versions/dates, all-caps acronyms, and Capitalized words
// (proper nouns, plan tiers, product names) other than a leading sentence-start
// capital. Compared case-insensitively so casing noise doesn't cause misses, but
// presence/absence (the "Reserve" in "Sapphire Reserve") always does.
func discriminativeTokens(s string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, m := range reNumberish.FindAllString(s, -1) {
		out["#"+strings.ToLower(m)] = struct{}{}
	}
	words := reWord.FindAllStringIndex(s, -1)
	for i, idx := range words {
		w := s[idx[0]:idx[1]]
		isCap := w[0] >= 'A' && w[0] <= 'Z'
		if !isCap {
			continue
		}
		// Skip a capital only because it opens the prompt (sentence-start), unless
		// it's an all-caps acronym (those are always meaningful).
		if i == 0 && !reAcronym.MatchString(w) {
			continue
		}
		if _, stop := entityStopwords[strings.ToLower(w)]; stop {
			continue
		}
		out["@"+strings.ToLower(w)] = struct{}{}
	}
	return out
}

// entityStopwords are Capitalized words that are not discriminative entities —
// common sentence openers / interrogatives that get capitalized mid-prompt.
var entityStopwords = map[string]bool{
	"i": true, "the": true, "a": true, "an": true, "what": true, "when": true,
	"where": true, "who": true, "why": true, "how": true, "is": true, "are": true,
	"do": true, "does": true, "can": true, "should": true, "please": true,
}

// discriminativeTokensMatch is true iff both prompts carry the SAME set of
// entity/number/date tokens. A token present in one but not the other (a SKU,
// a plan tier, a version, a date) means they are different questions.
func discriminativeTokensMatch(a, b string) bool {
	ta, tb := discriminativeTokens(a), discriminativeTokens(b)
	if len(ta) != len(tb) {
		return false
	}
	for k := range ta {
		if _, ok := tb[k]; !ok {
			return false
		}
	}
	return true
}
