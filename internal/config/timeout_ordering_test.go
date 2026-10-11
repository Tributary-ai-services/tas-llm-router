package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// The timeout layers must stay ORDERED: provider <= router < gateway write
// < nginx read. The reason is not tidiness.
//
// If an OUTER layer times out first, the caller is told the request failed
// while the gateway keeps generating tokens we are still billed for, and no
// event records why -- the money is spent and unattributable. The gateway is
// the only layer that can stamp the outcome, price the turn and fire the
// right alert, so the gateway must be the one that decides, and everything
// around it has to be more patient.
//
// This is a guard rather than a doc comment because the ordering lives in
// four places (code default, configs/config.yaml, the k8s env, and four
// ingress manifests) and nothing else would notice one of them moving.
func TestTimeoutOrdering(t *testing.T) {
	cfg := &Config{}
	cfg.setDefaults()

	provider := cfg.Providers.Anthropic.Timeout
	router := cfg.Router.RequestTimeout
	write := cfg.Server.WriteTimeout

	if provider > router {
		t.Errorf("provider timeout %v > router request timeout %v: the router would abandon a call the provider is still making", provider, router)
	}
	if router >= write {
		t.Errorf("router request timeout %v >= server write timeout %v: the connection dies before the handler can record the outcome", router, write)
	}

	// And nginx, which is outside all of them, must be more patient than the
	// server's write deadline. Read from the manifests so a change to either
	// side is caught.
	nginxRead := smallestIngressReadTimeout(t)
	if nginxRead <= write {
		t.Errorf("nginx proxy-read-timeout %v <= server write timeout %v: nginx would 504 first, "+
			"telling the caller it failed while the gateway keeps generating billable tokens", nginxRead, write)
	}
}

// smallestIngressReadTimeout returns the least patient proxy-read-timeout
// across the ingress manifests, which is the one that binds.
func smallestIngressReadTimeout(t *testing.T) time.Duration {
	t.Helper()
	paths, err := filepath.Glob("../../k8s/ingress*.yaml")
	if err != nil || len(paths) == 0 {
		t.Skip("ingress manifests not found from this working directory")
	}
	re := regexp.MustCompile(`proxy-read-timeout:\s*"?(\d+)"?`)
	smallest := time.Duration(0)
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				continue
			}
			d := time.Duration(n) * time.Second
			if smallest == 0 || d < smallest {
				smallest = d
			}
		}
	}
	if smallest == 0 {
		t.Skip("no proxy-read-timeout found in any ingress manifest")
	}
	return smallest
}
