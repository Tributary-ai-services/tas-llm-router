package adapters

import "strings"

// latestSuffix marks an alias that should resolve to the newest model of a
// family, e.g. "gpt-4-latest" or "claude-sonnet-latest".
const latestSuffix = "-latest"

// resolveLatest resolves a "<family>-latest" alias against a set of concrete
// model names: it returns the lexicographically greatest name that matches the
// family, which for date- or version-suffixed ids is the newest.
//
// It is a heuristic, deliberately: Anthropic ids carry dates/versions so
// greatest-name ≈ newest holds well; OpenAI ids are dateless, so this picks a
// stable representative rather than a guaranteed "latest". Returns ok=false when
// the alias is not a "-latest" form or nothing matches, so the caller falls back
// to identity.
func resolveLatest(alias string, names []string) (string, bool) {
	if !strings.HasSuffix(alias, latestSuffix) {
		return "", false
	}
	family := strings.TrimSuffix(alias, latestSuffix)
	best := ""
	for _, n := range names {
		if matchesFamily(n, family) && n > best {
			best = n
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

// matchesFamily reports whether a model name belongs to a family, by requiring
// every "-"-separated token of the family to appear in the name. Token
// containment (not prefix) handles Anthropic's two naming orders — the family
// "claude-sonnet" matches both "claude-sonnet-4-5" and "claude-3-5-sonnet".
func matchesFamily(name, family string) bool {
	name = strings.ToLower(name)
	for tok := range strings.SplitSeq(strings.ToLower(family), "-") {
		if tok == "" {
			continue
		}
		if !strings.Contains(name, tok) {
			return false
		}
	}
	return true
}
