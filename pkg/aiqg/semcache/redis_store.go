package semcache

import (
	"context"
	"encoding/binary"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// keyPrefix namespaces every entry; the tenant segment follows so a purge scans
// aiqg:scache:{tenant}:*.
const keyPrefix = "aiqg:scache"

// DefaultIndex is the RediSearch index name.
const DefaultIndex = "aiqg_scache_idx"

// RedisStore is the production VectorStore: a shared, tenant-scoped RediSearch
// FLAT vector index with per-key TTL (docs/AIQG-SEMANTIC-CACHING.md §7.2/§7.3).
// FLAT (exact brute force) is correct at 10k–100k entries — no recall loss on a
// correctness-sensitive path, no HNSW tuning. Lives on the dedicated
// redis-semcache instance (redis-stack-server), not shared redis.
type RedisStore struct {
	rc    redis.UniversalClient
	index string
	dim   int
}

// NewRedisStore wraps a redis-stack client. Call EnsureIndex once at startup.
func NewRedisStore(rc redis.UniversalClient, dim int) *RedisStore {
	return &RedisStore{rc: rc, index: DefaultIndex, dim: dim}
}

// EnsureIndex creates the FLAT COSINE vector index idempotently (a re-create on
// an existing index is treated as success — RediSearch rebuilds the index from
// the HASH keys on load, so a restart just re-warms).
func (s *RedisStore) EnsureIndex(ctx context.Context) error {
	err := s.rc.FTCreate(ctx, s.index,
		&redis.FTCreateOptions{OnHash: true, Prefix: []any{keyPrefix + ":"}},
		&redis.FieldSchema{FieldName: "tenant", FieldType: redis.SearchFieldTypeTag},
		&redis.FieldSchema{FieldName: "model", FieldType: redis.SearchFieldTypeTag},
		&redis.FieldSchema{FieldName: "scoring_version", FieldType: redis.SearchFieldTypeTag},
		&redis.FieldSchema{FieldName: "embedding", FieldType: redis.SearchFieldTypeVector,
			VectorArgs: &redis.FTVectorArgs{FlatOptions: &redis.FTFlatOptions{
				Type: "FLOAT32", Dim: s.dim, DistanceMetric: "COSINE",
			}}},
	).Err()
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return nil
	}
	return err
}

func (s *RedisStore) redisKey(tenantID, hash string) string {
	return keyPrefix + ":" + tenantID + ":" + hash
}

func (s *RedisStore) Put(ctx context.Context, e *Entry, ttl time.Duration) error {
	key := s.redisKey(e.TenantID, e.Key)
	fields := map[string]any{
		"tenant":          e.TenantID,
		"model":           e.Model,
		"scoring_version": e.ScoringVersion,
		"prompt":          e.Prompt,
		"response":        string(e.Response),
		"embedding":       float32sToBytes(e.Embedding),
		"created_at":      e.CreatedAtUnix,
		"clear_score":     e.ClearScore,
	}
	if err := s.rc.HSet(ctx, key, fields).Err(); err != nil {
		return err
	}
	if ttl > 0 {
		return s.rc.Expire(ctx, key, ttl).Err()
	}
	return nil
}

func (s *RedisStore) Search(ctx context.Context, scope Scope, vec []float32, minSimilarity float64, k int) ([]Candidate, error) {
	if len(vec) == 0 {
		return nil, nil
	}
	if k <= 0 {
		k = 5
	}
	// Redis cosine DISTANCE = 1 - cosine similarity; a range query of "within
	// radius" is the cache primitive (not KNN — KNN with no floor is a false-hit
	// generator, §7.3). radius = 1 - minSimilarity.
	radius := 1.0 - minSimilarity
	query := "(@tenant:{" + escapeTag(scope.TenantID) + "} @model:{" + escapeTag(scope.Model) +
		"} @scoring_version:{" + escapeTag(scope.ScoringVersion) +
		"} @embedding:[VECTOR_RANGE $radius $vec]=>{$YIELD_DISTANCE_AS: dist})"

	res, err := s.rc.FTSearchWithArgs(ctx, s.index, query, &redis.FTSearchOptions{
		Return: []redis.FTSearchReturn{
			{FieldName: "dist"}, {FieldName: "prompt"}, {FieldName: "response"},
			{FieldName: "created_at"}, {FieldName: "clear_score"},
			{FieldName: "tenant"}, {FieldName: "model"}, {FieldName: "scoring_version"},
		},
		SortBy:         []redis.FTSearchSortBy{{FieldName: "dist", Asc: true}},
		LimitOffset:    0,
		Limit:          k,
		DialectVersion: 2,
		Params:         map[string]any{"radius": radius, "vec": float32sToBytes(vec)},
	}).Result()
	if err != nil {
		return nil, err
	}

	out := make([]Candidate, 0, len(res.Docs))
	for _, d := range res.Docs {
		f := d.Fields
		dist, _ := strconv.ParseFloat(f["dist"], 64)
		sim := clamp01(1.0 - dist)
		if sim < minSimilarity {
			continue
		}
		createdAt, _ := strconv.ParseInt(f["created_at"], 10, 64)
		clearScore, _ := strconv.ParseFloat(f["clear_score"], 64)
		out = append(out, Candidate{
			Similarity: sim,
			Entry: &Entry{
				Key:            d.ID,
				Prompt:         f["prompt"],
				Response:       []byte(f["response"]),
				TenantID:       f["tenant"],
				Model:          f["model"],
				ScoringVersion: f["scoring_version"],
				CreatedAtUnix:  createdAt,
				ClearScore:     clearScore,
			},
		})
	}
	return out, nil
}

// PurgeTenant deletes a tenant's namespace via SCAN + DEL (RediSearch drops the
// docs from the index on key delete).
func (s *RedisStore) PurgeTenant(ctx context.Context, tenantID string) (int, error) {
	pattern := keyPrefix + ":" + tenantID + ":*"
	var cursor uint64
	total := 0
	for {
		keys, next, err := s.rc.Scan(ctx, cursor, pattern, 256).Result()
		if err != nil {
			return total, err
		}
		if len(keys) > 0 {
			if err := s.rc.Del(ctx, keys...).Err(); err != nil {
				return total, err
			}
			total += len(keys)
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return total, nil
}

// float32sToBytes encodes a vector as the little-endian FLOAT32 blob RediSearch
// expects for both storage and the query vector.
func float32sToBytes(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// escapeTag escapes RediSearch TAG special characters so a tenant/model value
// with hyphens (UUIDs) or punctuation matches as a literal, not query syntax.
func escapeTag(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '-', '.', ':', '/', '@', '{', '}', '|', ' ', '\\', '"', '\'':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
