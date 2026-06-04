module github.com/tributary-ai/llm-router-waf

go 1.24.0

toolchain go1.24.4

require (
	github.com/IBM/sarama v1.46.3
	github.com/Tributary-ai-services/Gatekeeper v0.0.0-00010101000000-000000000000
	github.com/anthropics/anthropic-sdk-go v1.7.0
	github.com/getkin/kin-openapi v0.133.0
	github.com/golang-jwt/jwt/v5 v5.3.0
	github.com/google/uuid v1.6.0
	github.com/gorilla/mux v1.8.1
	github.com/prometheus/client_golang v1.23.2
	github.com/sashabaranov/go-openai v1.40.5
	github.com/sirupsen/logrus v1.9.3
	github.com/stretchr/testify v1.11.1
	gopkg.in/yaml.v2 v2.4.0
	gopkg.in/yaml.v3 v3.0.1
)

replace github.com/Tributary-ai-services/Gatekeeper => ../Gatekeeper

// Mirror Gatekeeper's replace directive so its transitive dependency
// on aether-shared/go-events resolves to the same local path. Without
// this, `go build ./internal/server/...` and `make test` fail with
// "missing go.sum entry for module providing package
// github.com/Tributary-ai-services/aether-shared/go-events" because
// replace directives in a dependency don't propagate to the parent
// module's go.sum.
replace github.com/Tributary-ai-services/aether-shared/go-events => ../aether-shared/go-events

require (
	github.com/Tributary-ai-services/aether-shared/go-events v0.0.0-00010101000000-000000000000 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/eapache/go-resiliency v1.7.0 // indirect
	github.com/eapache/go-xerial-snappy v0.0.0-20230731223053-c322873962e3 // indirect
	github.com/eapache/queue v1.1.0 // indirect
	github.com/flier/gohs v1.2.3 // indirect
	github.com/go-openapi/jsonpointer v0.21.0 // indirect
	github.com/go-openapi/swag v0.23.0 // indirect
	github.com/golang/snappy v0.0.4 // indirect
	github.com/hashicorp/go-uuid v1.0.3 // indirect
	github.com/jcmturner/aescts/v2 v2.0.0 // indirect
	github.com/jcmturner/dnsutils/v2 v2.0.0 // indirect
	github.com/jcmturner/gofork v1.7.6 // indirect
	github.com/jcmturner/gokrb5/v8 v8.4.4 // indirect
	github.com/jcmturner/rpc/v2 v2.0.3 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/kylelemons/godebug v1.1.0 // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/mohae/deepcopy v0.0.0-20170929034955-c48cc78d4826 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/oasdiff/yaml v0.0.0-20250309154309-f31be36b4037 // indirect
	github.com/oasdiff/yaml3 v0.0.0-20250309153720-d2182401db90 // indirect
	github.com/perimeterx/marshmallow v1.1.5 // indirect
	github.com/pierrec/lz4/v4 v4.1.26 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.66.1 // indirect
	github.com/prometheus/procfs v0.16.1 // indirect
	github.com/rcrowley/go-metrics v0.0.0-20250401214520-65e299d6c5c9 // indirect
	github.com/tidwall/gjson v1.14.4 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	github.com/woodsbury/decimal128 v1.3.0 // indirect
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	golang.org/x/crypto v0.46.0 // indirect
	golang.org/x/net v0.48.0 // indirect
	golang.org/x/sys v0.39.0 // indirect
	google.golang.org/protobuf v1.36.10 // indirect
)
