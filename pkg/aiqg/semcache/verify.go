package semcache

import (
	"regexp"
	"strings"
	"time"
	"unicode"
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
//
// Two classes live here, both of which flip what is being asked:
//
//   - grammatical negators (not/no/never/…) and the suppletive forms of
//     "cannot" — "unable" is exactly "cannot" with different morphology, and
//     omitting it meant "I can't log in" (parity 1) and "Unable to log in"
//     (parity 0) were treated as opposite polarity despite being synonyms.
//     That alone zeroed the hit rate on ticket-triage traffic.
//   - action-negating verbs (avoid/prevent/disable/exclude), which negate the
//     *action* rather than the clause. These matter because the dangerous
//     pairs are lexically close enough to clear any workable similarity
//     threshold: "How do I disable MFA?" vs "How do I enable MFA?" sit around
//     cosine 0.95, so without a polarity signal the cache would serve the
//     exact opposite instruction.
//
// The trade is deliberate and asymmetric: an extra word here costs a cache
// miss ("turn off MFA" no longer matches "disable MFA"), while a missing one
// costs a wrong answer. Misses are recoverable; wrong answers are not.
// Deliberately NOT included: stop / skip / block / halt — too polysemous in
// ops questions ("how do I stop nginx") to be read as polarity, and their
// opposites are not dangerous in the way enable/disable is.
var negationWords = map[string]bool{
	"not": true, "no": true, "never": true, "without": true, "cannot": true,
	"none": true, "nor": true, "neither": true, "nothing": true,
	"unable": true,
}

// actionNegators are the action-negating verbs, matched on their stem so every
// inflection counts (avoid/avoids/avoided/avoiding, disable/disabled/…).
// Listing surface forms instead would silently miss "excluded" while catching
// "exclude", which is how this gap first showed up.
var actionNegators = map[string]bool{
	"avoid": true, "prevent": true, "disable": true, "exclude": true,
}

// verbStems returns the candidate stems of w for actionNegators lookup. Cheap
// suffix stripping is enough here: the set is four known verbs, so the risk is
// a missed inflection (a cache miss), not a wrong stem matching something else.
func verbStems(w string) [5]string {
	return [5]string{
		strings.TrimSuffix(w, "s"),
		strings.TrimSuffix(w, "d"),
		strings.TrimSuffix(w, "ed"),
		strings.TrimSuffix(w, "ing"),
		strings.TrimSuffix(w, "ing") + "e", // disabling → disabl → disable
	}
}

// isNegationWord reports whether w flips polarity, covering both the
// grammatical negators and any inflection of an action-negating verb.
func isNegationWord(w string) bool {
	if negationWords[w] {
		return true
	}
	if actionNegators[w] {
		return true
	}
	for _, stem := range verbStems(w) {
		if actionNegators[stem] {
			return true
		}
	}
	return false
}

// positionalCapBoundaries are characters after which a capital letter is
// explained by POSITION rather than by the word being a proper noun: sentence
// enders, clause introducers, and quote/bracket openers all force a capital on
// whatever follows.
const positionalCapBoundaries = ".!?:;\"'`([{\n\r“‘"

// positionalCapitalSites returns the byte offsets of runes that sit at a
// position where a capital is positionally forced.
//
// This exists because the entity guard previously exempted a capital only at
// index 0 of the whole prompt. Any other capital was read as a proper noun —
// including a word that merely OPENS A QUOTED STRING, which is constant in
// real traffic (ticket text, email subjects, log lines). The seed
//
//	Classify this ticket: "I can't log in to my account"
//
// yielded no entity tokens, while the paraphrase
//
//	Classify this ticket: "Unable to log in to my account"
//
// yielded {@unable} — so the token sets differed and every such pair was
// rejected, silently zeroing the hit rate on exactly the classification
// workload semantic caching is best at.
//
// Known trade: a genuine proper noun that opens a quote is now also skipped
// (`Summarize: "Acme filed…"` vs `"Beta filed…"`), leaving those pairs to the
// similarity threshold. Acronyms and anything numeric/date-like are still
// always kept, and those carry most of the discriminative signal.
func positionalCapitalSites(s string) map[int]bool {
	sites := map[int]bool{}
	atBoundary := true // start of string counts
	for i, r := range s {
		// A line break is itself a boundary, so it must be handled before the
		// whitespace skip — otherwise it is swallowed as plain spacing and the
		// first word of the next line is read as a proper noun.
		if r == '\n' || r == '\r' {
			atBoundary = true
			continue
		}
		if unicode.IsSpace(r) {
			continue
		}
		if atBoundary {
			sites[i] = true
		}
		atBoundary = strings.ContainsRune(positionalCapBoundaries, r)
	}
	return sites
}

// negationParity returns count(negations) % 2, so a double negative ("not
// unsafe") reads the same polarity as none. Contractions (isn't, don't, can't)
// each count once.
func negationParity(s string) int {
	l := strings.ToLower(s)
	n := strings.Count(l, "n't") // isn't/don't/can't/won't/…
	for _, w := range reWord.FindAllString(l, -1) {
		if isNegationWord(w) {
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
	sites := positionalCapitalSites(s)
	words := reWord.FindAllStringIndex(s, -1)
	for _, idx := range words {
		w := s[idx[0]:idx[1]]
		isCap := w[0] >= 'A' && w[0] <= 'Z'
		if !isCap {
			continue
		}
		// Skip a capital that is explained by position — the start of the prompt,
		// or immediately after a sentence/clause boundary such as an opening
		// quote. All-caps acronyms are always meaningful and are never skipped.
		if sites[idx[0]] && !reAcronym.MatchString(w) {
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
