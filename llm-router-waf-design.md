# LLM Request Router WAF - Design Document

## Executive Summary

This document outlines the design for a next-generation LLM request router that functions as a **Web Application Firewall (WAF) for LLM traffic**. The system provides intelligent routing, comprehensive security scanning, cost optimization, and real-time analytics through a graph database backend. Built in Go with full provider feature support, it eliminates the "lowest common denominator" problem while providing enterprise-grade security and observability.

## Core Design Principles

### 1. **Zero Feature Loss Architecture**
- Native provider APIs with full feature support
- Intelligent feature-based routing
- No abstraction layer compromises

### 2. **WAF-Grade Security**  
- Real-time request/response inspection
- Pattern-based threat detection
- Integration with existing audimodal security scanner
- Automated blocking and rate limiting

### 3. **Graph Database Analytics**
- Complete request/response lifecycle mapping
- Pattern analysis and anomaly detection  
- Real-time troubleshooting and forensics
- Cost and performance optimization insights

### 4. **Cloud-Native Operations**
- Kubernetes-first design
- OpenTelemetry integration
- Prometheus metrics
- Horizontal scaling

## System Architecture

```
┌─────────────────┐    ┌──────────────────┐    ┌─────────────────┐
│   Client Apps   │────│   LLM WAF        │────│   LLM Providers │
│                 │    │   Router         │    │                 │
└─────────────────┘    └──────────────────┘    └─────────────────┘
                              │
                              ▼
                    ┌─────────────────────┐
                    │   Graph Database    │
                    │   Analytics Engine  │
                    └─────────────────────┘
```

### Core Components

#### 1. **Request Router & WAF Engine**
```go
type LLMRouter struct {
    // Core routing
    providers        map[string]LLMProvider
    featureRouter    *FeatureRouter
    costOptimizer    *CostOptimizer
    
    // WAF components
    securityScanner  *SecurityScanner
    threatDetector   *ThreatDetector
    rateLimiter     *RateLimiter
    circuitBreaker  *CircuitBreaker
    
    // Analytics
    graphDB         GraphDatabase
    metricsCollector *MetricsCollector
    
    // Observability
    tracer          trace.Tracer
    logger          *slog.Logger
}
```

#### 2. **Provider Interface Hierarchy**
```go
// Base interface - all providers must implement
type LLMProvider interface {
    GetCapabilities() ProviderCapabilities
    ChatCompletion(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
    StreamCompletion(ctx context.Context, req *ChatRequest) (<-chan *ChatChunk, error)
    EstimateCost(req *ChatRequest) (*CostEstimate, error)
    HealthCheck(ctx context.Context) error
}

// Advanced feature interfaces
type FunctionCallingProvider interface {
    LLMProvider
    FunctionCall(ctx context.Context, req *FunctionCallRequest) (*FunctionCallResponse, error)
    ParallelFunctionCall(ctx context.Context, req *ParallelFunctionRequest) (*ParallelFunctionResponse, error)
}

type VisionProvider interface {
    LLMProvider
    VisionAnalysis(ctx context.Context, req *VisionRequest) (*VisionResponse, error)
}

type StructuredOutputProvider interface {
    LLMProvider
    StructuredCompletion(ctx context.Context, req *StructuredRequest) (*StructuredResponse, error)
}

type AssistantProvider interface {
    LLMProvider
    CreateAssistant(ctx context.Context, req *AssistantRequest) (*Assistant, error)
    RunAssistant(ctx context.Context, assistantID string, req *RunRequest) (*RunResponse, error)
    ManageFiles(ctx context.Context, req *FileRequest) (*FileResponse, error)
}

type BatchProvider interface {
    LLMProvider
    CreateBatch(ctx context.Context, req *BatchRequest) (*BatchResponse, error)
    GetBatchStatus(ctx context.Context, batchID string) (*BatchStatus, error)
}

type MCPProvider interface {
    LLMProvider
    ConnectMCPService(ctx context.Context, serviceID string) (*MCPConnection, error)
    ExecuteMCPTool(ctx context.Context, conn *MCPConnection, req *MCPToolRequest) (*MCPToolResponse, error)
    ListMCPServices(ctx context.Context) ([]MCPService, error)
}
```

#### 3. **Security Integration (Audimodal Scanner)**
```go
type SecurityScanner struct {
    audimodalScanner *audimodal.Scanner
    piiDetector     *PIIDetector
    promptInjector  *PromptInjectionDetector
    toxicityFilter  *ToxicityFilter
    dataLeakage     *DataLeakageDetector
}

type SecurityScanResult struct {
    RequestID       string
    Timestamp       time.Time
    ThreatLevel     ThreatLevel
    Violations      []SecurityViolation
    PIIDetected     []PIIEntity
    Action          SecurityAction // ALLOW, BLOCK, SANITIZE, ALERT
    SanitizedRequest *ChatRequest  // If sanitization applied
    GraphNodes      []GraphNode   // For graph DB storage
}

type SecurityViolation struct {
    Type        ViolationType // PII, PROMPT_INJECTION, TOXICITY, DATA_LEAKAGE
    Severity    Severity      // LOW, MEDIUM, HIGH, CRITICAL  
    Location    string        // Where in request (message[0].content, etc)
    Pattern     string        // What pattern triggered
    Confidence  float64       // Detection confidence (0-1)
    Remediation string        // Suggested action
}
```

## Graph Database Schema

### Node Types

```go
type GraphNodeType string

const (
    NodeTypeRequest      GraphNodeType = "REQUEST"
    NodeTypeResponse     GraphNodeType = "RESPONSE" 
    NodeTypeUser         GraphNodeType = "USER"
    NodeTypeApplication  GraphNodeType = "APPLICATION"
    NodeTypeProvider     GraphNodeType = "PROVIDER"
    NodeTypeModel        GraphNodeType = "MODEL"
    NodeTypeFeature      GraphNodeType = "FEATURE"
    NodeTypeThreat       GraphNodeType = "THREAT"
    NodeTypeCost         GraphNodeType = "COST"
    NodeTypePerformance  GraphNodeType = "PERFORMANCE"
    NodeTypePattern      GraphNodeType = "PATTERN"
)

type GraphNode struct {
    ID         string                 `json:"id"`
    Type       GraphNodeType          `json:"type"`
    Properties map[string]interface{} `json:"properties"`
    Timestamp  time.Time              `json:"timestamp"`
    Labels     []string               `json:"labels"`
}

type GraphRelationship struct {
    ID         string                 `json:"id"`
    Type       string                 `json:"type"`
    FromNode   string                 `json:"from_node"`
    ToNode     string                 `json:"to_node"`
    Properties map[string]interface{} `json:"properties"`
    Weight     float64                `json:"weight,omitempty"`
}
```

### Relationship Types

```go
const (
    RelationshipTypeRouted     = "ROUTED_TO"      // Request -> Provider
    RelationshipTypeUsed       = "USED_FEATURE"   // Request -> Feature
    RelationshipTypeDetected   = "DETECTED_THREAT" // Request -> Threat
    RelationshipTypeCost       = "INCURRED_COST"  // Request -> Cost
    RelationshipTypeTriggered  = "TRIGGERED_RULE" // Request -> Security Rule
    RelationshipTypeSimilar    = "SIMILAR_PATTERN" // Request -> Request
    RelationshipTypeFollowed   = "FOLLOWED_BY"    // Request -> Request (temporal)
    RelationshipTypeBlocked    = "BLOCKED_BY"     // Request -> Security Policy
    RelationshipTypeOptimized  = "OPTIMIZED_FOR" // Routing -> Cost/Performance
)
```

### Graph Queries for Analytics

```cypher
// Find all requests with PII in the last hour
MATCH (r:REQUEST)-[:DETECTED_THREAT]->(t:THREAT {type: 'PII'})
WHERE r.timestamp > datetime() - duration({hours: 1})
RETURN r, t

// Identify cost optimization opportunities  
MATCH (r:REQUEST)-[:ROUTED_TO]->(p:PROVIDER)-[:INCURRED_COST]->(c:COST)
WHERE c.amount > 0.001 AND r.features = ['basic_chat']
RETURN p.name, AVG(c.amount) as avg_cost, COUNT(r) as request_count
ORDER BY avg_cost DESC

// Find attack patterns
MATCH (u:USER)-[:MADE_REQUEST]->(r:REQUEST)-[:DETECTED_THREAT]->(t:THREAT)
WHERE t.severity = 'HIGH'
WITH u, COUNT(r) as threat_count
WHERE threat_count > 5
RETURN u, threat_count

// Performance analysis by feature usage
MATCH (r:REQUEST)-[:USED_FEATURE]->(f:FEATURE),
      (r)-[:ROUTED_TO]->(p:PROVIDER),
      (r)-[:HAD_PERFORMANCE]->(perf:PERFORMANCE)
RETURN f.name, p.name, AVG(perf.latency) as avg_latency, COUNT(r) as requests
ORDER BY avg_latency DESC
```

## WAF Capabilities

### 1. **Real-Time Threat Detection**
```go
type ThreatDetector struct {
    rules           []DetectionRule
    patterns        PatternMatcher
    ml              MLThreatModel
    graphAnalytics  *GraphAnalytics
}

type DetectionRule struct {
    ID          string
    Name        string
    Pattern     string    // Regex or rule pattern
    ThreatType  ThreatType
    Severity    Severity
    Action      SecurityAction
    Enabled     bool
    LastUpdated time.Time
}

// Examples of detection rules
var DefaultRules = []DetectionRule{
    {
        ID:         "PII_SSN",
        Pattern:    `\b\d{3}-\d{2}-\d{4}\b`,
        ThreatType: ThreatTypePII,
        Severity:   SeverityHigh,
        Action:     ActionBlock,
    },
    {
        ID:         "PROMPT_INJECTION",
        Pattern:    `(?i)(ignore|forget|override).*(previous|above|instruction|rule)`,
        ThreatType: ThreatTypePromptInjection, 
        Severity:   SeverityCritical,
        Action:     ActionBlock,
    },
}
```

### 2. **Dynamic Rate Limiting**
```go
type RateLimiter struct {
    policies    map[string]*RatePolicy
    graphDB     GraphDatabase
    redis       *redis.Client
}

type RatePolicy struct {
    MaxRequests     int
    WindowDuration  time.Duration
    BurstAllowance  int
    CostLimit       float64
    FeatureSpecific map[string]int // Different limits per feature
    
    // Dynamic adjustments based on threat level
    ThreatAdjustment map[ThreatLevel]float64
}

// Rate limiting with threat-based adjustment
func (rl *RateLimiter) CheckLimit(ctx context.Context, userID string, reqType RequestType) (*RateLimitResult, error) {
    // Check user's recent threat activity from graph
    threatLevel := rl.getUserThreatLevel(ctx, userID)
    
    policy := rl.policies[userID]
    if policy == nil {
        policy = rl.getDefaultPolicy(reqType)
    }
    
    // Adjust limits based on threat level
    adjustedLimit := int(float64(policy.MaxRequests) * policy.ThreatAdjustment[threatLevel])
    
    // Check current usage
    current := rl.getCurrentUsage(ctx, userID, policy.WindowDuration)
    
    return &RateLimitResult{
        Allowed:     current < adjustedLimit,
        Remaining:   adjustedLimit - current,
        ResetTime:   time.Now().Add(policy.WindowDuration),
        ThreatLevel: threatLevel,
    }, nil
}
```

### 3. **Intelligent Routing with Security Context**
```go
type FeatureRouter struct {
    providers        map[string]LLMProvider
    capabilities     map[string]ProviderCapabilities
    costMatrix       CostMatrix
    performanceMatrix PerformanceMatrix
    securityContext  *SecurityContext
    graphDB          GraphDatabase
}

type RoutingDecision struct {
    SelectedProvider    string
    Reasoning          []string
    SecurityFactors    []string
    CostImpact         float64
    PerformanceImpact  time.Duration
    FeatureCompatibility map[string]bool
    FallbackChain      []string
    GraphContext       RoutingGraphContext
}

func (fr *FeatureRouter) RouteWithSecurity(ctx context.Context, req *UnifiedRequest, secResult *SecurityScanResult) (*RoutingDecision, error) {
    // If security scan found critical threats, route to secure sandbox
    if secResult.ThreatLevel >= ThreatLevelHigh {
        return fr.routeToSecureSandbox(ctx, req, secResult)
    }
    
    // Analyze feature requirements
    requiredFeatures := fr.analyzeFeatureRequirements(req)
    
    // Get capable providers
    candidates := fr.findCapableProviders(requiredFeatures)
    if len(candidates) == 0 {
        return nil, ErrNoCapableProvider
    }
    
    // Factor in security context from graph
    securityFactors := fr.getSecurityFactors(ctx, req, secResult)
    
    // Multi-criteria decision with security weighting
    decision := fr.selectOptimalProvider(candidates, req.OptimizationCriteria, securityFactors)
    
    // Record decision in graph for analysis
    fr.recordRoutingDecision(ctx, req, decision)
    
    return decision, nil
}
```

## MCP (Model Context Protocol) Integration

### External MCP Service Support

The LLM Router WAF integrates with the Tributary AI Services MCP orchestrator ([tas-mcp](https://github.com/Tributary-ai-services/tas-mcp)) to provide comprehensive tool access across 1,535+ MCP servers. This separation of concerns allows the router to focus on security, routing, and analytics while delegating tool orchestration to a specialized service.

#### MCP Service Architecture
```go
type MCPIntegration struct {
    serviceClient   MCPServiceClient
    healthMonitor   *MCPHealthMonitor
    toolRegistry    *MCPToolRegistry
    securityFilter  *MCPSecurityFilter
    graphDB         GraphDatabase
}

type MCPService struct {
    ID              string
    Name            string
    Category        ServiceCategory // DATABASE, AI_SEARCH, DEV_TOOLS, etc.
    Priority        Priority        // HIGHEST, HIGH, MEDIUM, LOW
    Endpoints       []string
    AuthMethod      AuthMethod      // OAUTH2, JWT, API_KEY
    HealthStatus    HealthStatus
    Capabilities    []string
    LastHealthCheck time.Time
}

type MCPToolRequest struct {
    RequestID     string
    UserID        string
    ServiceID     string
    ToolName      string
    Parameters    map[string]interface{}
    SecurityContext *SecurityContext
    Metadata      map[string]string
}

type MCPToolResponse struct {
    RequestID     string
    ServiceID     string
    ToolName      string
    Result        interface{}
    Error         error
    ExecutionTime time.Duration
    TokensUsed    int
    Cost          float64
}
```

#### Priority-Based Service Categories

Based on the tas-mcp roadmap, services are categorized by priority:

**Highest Priority (Core AI Services):**
- Database connectors (PostgreSQL, MongoDB, Redis)
- AI/Search services (Elasticsearch, Pinecone, Weaviate)
- Development tools (GitHub, GitLab, Docker)
- Communication platforms (Slack, Discord, email)

**High Priority (Productivity):**
- Web scraping and data extraction
- Cloud storage (AWS S3, Google Drive)
- Productivity suites (Google Workspace, Microsoft 365)

**Medium Priority (Analytics & Finance):**
- Financial data services (APIs, market data)
- Data analytics and visualization tools

#### MCP Security Integration
```go
type MCPSecurityFilter struct {
    allowedServices map[string]bool
    rateLimits     map[string]*RateLimit
    securityRules  []MCPSecurityRule
    graphDB        GraphDatabase
}

type MCPSecurityRule struct {
    ServiceCategory ServiceCategory
    MaxToolsPerMin  int
    RequiredAuth    []AuthMethod
    AllowedUsers    []string
    BlockedTools    []string
    DataFiltering   DataFilterRule
}

func (msf *MCPSecurityFilter) FilterMCPRequest(ctx context.Context, req *MCPToolRequest, secResult *SecurityScanResult) (*MCPFilterResult, error) {
    // Apply security context from main WAF scan
    if secResult.ThreatLevel >= ThreatLevelHigh {
        return &MCPFilterResult{
            Allowed: false,
            Reason:  "High threat level detected in request",
        }, nil
    }
    
    // Check service-specific security rules
    rules := msf.getServiceRules(req.ServiceID)
    
    // Rate limiting based on service category
    if !msf.checkRateLimit(req.UserID, req.ServiceID) {
        return &MCPFilterResult{
            Allowed: false, 
            Reason:  "Rate limit exceeded for MCP service",
        }, nil
    }
    
    // Tool-specific filtering
    if msf.isToolBlocked(req.ServiceID, req.ToolName) {
        return &MCPFilterResult{
            Allowed: false,
            Reason:  fmt.Sprintf("Tool %s blocked for service %s", req.ToolName, req.ServiceID),
        }, nil
    }
    
    return &MCPFilterResult{Allowed: true}, nil
}
```

#### MCP Analytics and Monitoring
```go
func (router *LLMRouter) recordMCPUsage(ctx context.Context, req *MCPToolRequest, resp *MCPToolResponse) error {
    // Create graph nodes for MCP tool usage
    nodes := []GraphNode{
        {
            ID:   fmt.Sprintf("mcp_request_%s", req.RequestID),
            Type: "MCP_REQUEST",
            Properties: map[string]interface{}{
                "service_id":     req.ServiceID,
                "tool_name":      req.ToolName,
                "user_id":        req.UserID,
                "execution_time": resp.ExecutionTime.Milliseconds(),
                "tokens_used":    resp.TokensUsed,
                "cost":           resp.Cost,
            },
        },
        {
            ID:   fmt.Sprintf("mcp_service_%s", req.ServiceID),
            Type: "MCP_SERVICE", 
            Properties: map[string]interface{}{
                "service_id": req.ServiceID,
                "category":   router.mcpIntegration.getServiceCategory(req.ServiceID),
                "priority":   router.mcpIntegration.getServicePriority(req.ServiceID),
            },
        },
    }
    
    relationships := []GraphRelationship{
        {
            Type:     "USED_MCP_TOOL",
            FromNode: fmt.Sprintf("request_%s", req.RequestID),
            ToNode:   fmt.Sprintf("mcp_request_%s", req.RequestID),
        },
        {
            Type:     "CONNECTED_TO_SERVICE", 
            FromNode: fmt.Sprintf("mcp_request_%s", req.RequestID),
            ToNode:   fmt.Sprintf("mcp_service_%s", req.ServiceID),
        },
    }
    
    return router.graphDB.CreateNodesAndRelationships(ctx, nodes, relationships)
}
```

#### MCP Cost Tracking
```cypher
// Track MCP service costs by category
MATCH (r:REQUEST)-[:USED_MCP_TOOL]->(mcp:MCP_REQUEST)-[:CONNECTED_TO_SERVICE]->(s:MCP_SERVICE)
WHERE r.timestamp > datetime() - duration({hours: 24})
WITH s.category as category, 
     SUM(mcp.cost) as total_cost,
     COUNT(mcp) as tool_calls,
     AVG(mcp.execution_time) as avg_execution_time
RETURN category, total_cost, tool_calls, avg_execution_time
ORDER BY total_cost DESC

// Identify high-cost MCP users
MATCH (u:USER)-[:MADE_REQUEST]->(r:REQUEST)-[:USED_MCP_TOOL]->(mcp:MCP_REQUEST)
WHERE r.timestamp > datetime() - duration({hours: 1})
WITH u, SUM(mcp.cost) as total_mcp_cost, COUNT(mcp) as mcp_calls
WHERE total_mcp_cost > 10.0
RETURN u.id, total_mcp_cost, mcp_calls
ORDER BY total_mcp_cost DESC
```

## Integration with Audimodal Scanner

### Scanner Integration Interface
```go
type AudimodalIntegration struct {
    scanner     *audimodal.Scanner
    config      *audimodal.Config
    graphDB     GraphDatabase
    cache       *SecurityCache
}

func (ai *AudimodalIntegration) ScanRequest(ctx context.Context, req *UnifiedRequest) (*SecurityScanResult, error) {
    // Extract scannable content from request
    content := ai.extractContent(req)
    
    // Run audimodal scanner
    scanResult, err := ai.scanner.ScanMultiModal(ctx, &audimodal.ScanRequest{
        Text:   content.Text,
        Images: content.Images,
        Audio:  content.Audio,
        Metadata: map[string]interface{}{
            "request_id": req.ID,
            "user_id":    req.UserID,
            "app_id":     req.ApplicationID,
        },
    })
    if err != nil {
        return nil, fmt.Errorf("audimodal scan failed: %w", err)
    }
    
    // Convert to our security result format
    result := ai.convertScanResult(scanResult, req)
    
    // Enrich with graph context
    ai.enrichWithGraphContext(ctx, result)
    
    // Cache result for performance
    ai.cache.Store(req.ID, result, 5*time.Minute)
    
    return result, nil
}

func (ai *AudimodalIntegration) convertScanResult(scanResult *audimodal.ScanResult, req *UnifiedRequest) *SecurityScanResult {
    result := &SecurityScanResult{
        RequestID:   req.ID,
        Timestamp:   time.Now(),
        Violations:  make([]SecurityViolation, 0),
        PIIDetected: make([]PIIEntity, 0),
        GraphNodes:  make([]GraphNode, 0),
    }
    
    // Convert PII detections
    for _, pii := range scanResult.PIIEntities {
        result.PIIDetected = append(result.PIIDetected, PIIEntity{
            Type:       PIIType(pii.Type),
            Value:      pii.RedactedValue,
            Location:   pii.Location,
            Confidence: pii.Confidence,
        })
        
        // Create graph node for each PII detection
        result.GraphNodes = append(result.GraphNodes, GraphNode{
            ID:   fmt.Sprintf("pii_%s_%s", req.ID, pii.ID),
            Type: NodeTypeThreat,
            Properties: map[string]interface{}{
                "threat_type":  "PII",
                "pii_type":     pii.Type,
                "confidence":   pii.Confidence,
                "location":     pii.Location,
                "redacted_value": pii.RedactedValue,
            },
            Labels: []string{"PII", "DETECTED", pii.Type},
        })
    }
    
    // Convert other threats
    for _, threat := range scanResult.Threats {
        result.Violations = append(result.Violations, SecurityViolation{
            Type:        ViolationType(threat.Type),
            Severity:    Severity(threat.Severity),
            Location:    threat.Location,
            Pattern:     threat.Pattern,
            Confidence:  threat.Confidence,
            Remediation: threat.Remediation,
        })
    }
    
    // Determine overall threat level and action
    result.ThreatLevel = ai.calculateThreatLevel(result.Violations)
    result.Action = ai.determineSecurityAction(result.ThreatLevel, result.Violations)
    
    return result
}
```

## Graph Database Analytics Engine

### Real-Time Pattern Detection
```go
type PatternAnalytics struct {
    graphDB     GraphDatabase
    patterns    []ThreatPattern
    ml          *MLPatternDetector
    alerts      AlertManager
}

type ThreatPattern struct {
    ID          string
    Name        string
    Description string
    Query       string  // Cypher query
    Threshold   float64
    Actions     []AlertAction
    LastTrigger time.Time
}

// Predefined threat patterns
var ThreatPatterns = []ThreatPattern{
    {
        ID:   "COORDINATED_ATTACK",
        Name: "Coordinated Attack Pattern",
        Description: "Multiple users with similar request patterns indicating coordinated attack",
        Query: `
            MATCH (u1:USER)-[:MADE_REQUEST]->(r1:REQUEST)-[:DETECTED_THREAT]->(t:THREAT)
            MATCH (u2:USER)-[:MADE_REQUEST]->(r2:REQUEST)-[:DETECTED_THREAT]->(t2:THREAT)
            WHERE u1 <> u2 
              AND t.type = t2.type 
              AND abs(duration.between(r1.timestamp, r2.timestamp).seconds) < 300
            WITH u1, u2, COUNT(*) as similarity
            WHERE similarity > 5
            RETURN u1.id, u2.id, similarity
        `,
        Threshold: 0.8,
        Actions: []AlertAction{AlertActionBlock, AlertActionNotify},
    },
    {
        ID:   "COST_ABUSE",
        Name: "Cost Abuse Detection",
        Description: "Users generating unusually high costs through expensive model usage",
        Query: `
            MATCH (u:USER)-[:MADE_REQUEST]->(r:REQUEST)-[:INCURRED_COST]->(c:COST)
            WHERE r.timestamp > datetime() - duration({hours: 1})
            WITH u, SUM(c.amount) as total_cost, COUNT(r) as requests
            WHERE total_cost > 100.0 OR total_cost/requests > 1.0
            RETURN u.id, total_cost, requests, total_cost/requests as cost_per_request
            ORDER BY total_cost DESC
        `,
        Threshold: 100.0,
        Actions: []AlertAction{AlertActionRateLimit, AlertActionNotify},
    },
    {
        ID:   "FEATURE_ABUSE",
        Name: "Advanced Feature Abuse",
        Description: "Excessive use of expensive features (vision, function calling)",
        Query: `
            MATCH (u:USER)-[:MADE_REQUEST]->(r:REQUEST)-[:USED_FEATURE]->(f:FEATURE)
            WHERE f.name IN ['vision', 'function_calling', 'structured_output']
              AND r.timestamp > datetime() - duration({hours: 1})
            WITH u, f, COUNT(r) as usage_count
            WHERE usage_count > 50
            RETURN u.id, f.name, usage_count
        `,
        Threshold: 50,
        Actions: []AlertAction{AlertActionRateLimit},
    },
}

func (pa *PatternAnalytics) RunPatternDetection(ctx context.Context) error {
    for _, pattern := range pa.patterns {
        // Run pattern detection query
        results, err := pa.graphDB.Query(ctx, pattern.Query)
        if err != nil {
            continue
        }
        
        // Check if pattern threshold exceeded
        for _, result := range results {
            if pa.exceedsThreshold(result, pattern.Threshold) {
                // Trigger alerts and actions
                pa.triggerPattern(ctx, pattern, result)
            }
        }
    }
    return nil
}
```

### Performance Optimization Analytics
```go
type OptimizationAnalytics struct {
    graphDB GraphDatabase
}

func (oa *OptimizationAnalytics) AnalyzeCostOptimization(ctx context.Context, timeWindow time.Duration) (*OptimizationReport, error) {
    query := `
        MATCH (r:REQUEST)-[:ROUTED_TO]->(p:PROVIDER)-[:INCURRED_COST]->(c:COST)
        WHERE r.timestamp > datetime() - duration({seconds: $seconds})
        WITH r, p, c, 
             CASE WHEN r.features = ['basic_chat'] AND p.name != 'claude-haiku' 
                  THEN c.amount * 0.3 
                  ELSE 0 END as potential_savings
        RETURN p.name as provider,
               COUNT(r) as requests,
               SUM(c.amount) as total_cost,
               SUM(potential_savings) as potential_savings,
               AVG(c.amount) as avg_cost_per_request
        ORDER BY potential_savings DESC
    `
    
    results, err := oa.graphDB.Query(ctx, query, map[string]interface{}{
        "seconds": int(timeWindow.Seconds()),
    })
    if err != nil {
        return nil, err
    }
    
    return oa.buildOptimizationReport(results), nil
}

func (oa *OptimizationAnalytics) AnalyzeFeatureUsage(ctx context.Context) (*FeatureUsageReport, error) {
    query := `
        MATCH (r:REQUEST)-[:USED_FEATURE]->(f:FEATURE),
              (r)-[:ROUTED_TO]->(p:PROVIDER),
              (r)-[:INCURRED_COST]->(c:COST)
        WITH f.name as feature, p.name as provider, 
             COUNT(r) as usage_count,
             AVG(c.amount) as avg_cost,
             AVG(duration.between(r.start_time, r.end_time).milliseconds) as avg_latency
        RETURN feature, provider, usage_count, avg_cost, avg_latency
        ORDER BY usage_count DESC
    `
    
    results, err := oa.graphDB.Query(ctx, query)
    if err != nil {
        return nil, err
    }
    
    return oa.buildFeatureUsageReport(results), nil
}
```

## Implementation Phases (Claude Code Timeline)

### **Phase 1: Core Foundation (2-3 days)**
**Day 1: Interfaces & Basic Routing**
- Define all Go interfaces (LLMProvider hierarchy)
- Implement OpenAI and Anthropic providers with full native API support
- Basic request/response structures
- Simple routing logic (round-robin, cost-based)

**Day 2: Security Integration**
- Integrate audimodal scanner interface
- Implement security scanning pipeline
- Basic threat detection and blocking
- PII detection and sanitization

**Day 3: Graph Database Foundation**
- Neo4j/ArangoDB integration
- Basic node/relationship creation
- Request lifecycle tracking
- Simple analytics queries

### **Phase 2: Advanced Features (3-4 days)**
**Day 4: Feature-Based Routing**
- Capability detection and matching
- Intelligent provider selection
- Cost optimization algorithms
- Performance-based routing

**Day 5: WAF Capabilities**
- Rate limiting with threat context
- Circuit breakers and failover
- Pattern-based threat detection
- Real-time blocking and alerting

**Day 6: Multimodal Support**
- Vision model integration
- Function calling support
- Structured outputs with provider-specific handling
- Streaming implementations
- MCP service integration interface

**Day 7: Advanced Analytics**
- Real-time pattern detection
- Cost optimization recommendations  
- Performance analysis
- Threat correlation

### **Phase 3: Production Features (2-3 days)**
**Day 8: Observability**
- OpenTelemetry integration
- Prometheus metrics
- Grafana dashboards
- Distributed tracing

**Day 9: Kubernetes Deployment**
- Helm charts
- Horizontal Pod Autoscaling
- Service mesh integration
- ConfigMaps and Secrets management

**Day 10: Performance & Testing**
- Load testing and optimization
- Benchmark comparisons
- Security penetration testing
- Documentation and examples

## Deployment Architecture

### Kubernetes Deployment
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: llm-router-waf
spec:
  replicas: 3
  selector:
    matchLabels:
      app: llm-router-waf
  template:
    spec:
      containers:
      - name: llm-router
        image: tributary/llm-router-waf:latest
        ports:
        - containerPort: 8080
          name: http
        - containerPort: 8081  
          name: admin
        env:
        - name: GRAPH_DB_URL
          value: "neo4j://neo4j-service:7687"
        - name: REDIS_URL
          value: "redis://redis-service:6379"
        resources:
          requests:
            memory: "512Mi"
            cpu: "500m"
          limits:
            memory: "2Gi"
            cpu: "2000m"
        livenessProbe:
          httpGet:
            path: /health
            port: 8081
        readinessProbe:
          httpGet:
            path: /ready
            port: 8081
```

### Service Mesh Integration
```yaml
apiVersion: networking.istio.io/v1beta1
kind: VirtualService
metadata:
  name: llm-router-waf
spec:
  hosts:
  - llm-router-waf
  http:
  - match:
    - uri:
        prefix: "/v1/chat/completions"
    route:
    - destination:
        host: llm-router-waf
        subset: stable
    fault:
      delay:
        percentage:
          value: 0.1
        fixedDelay: 5s
    retries:
      attempts: 3
      perTryTimeout: 30s
```

## Competitive Advantages

### 1. **Zero Feature Loss**
Unlike existing gateways, full provider feature support without abstraction compromises

### 2. **WAF-Grade Security** 
Real-time threat detection with ML-powered pattern analysis, not just basic rate limiting

### 3. **Graph Database Intelligence**
Comprehensive relationship mapping enables advanced analytics impossible with traditional logging

### 4. **Cost Optimization Intelligence**
Feature-aware routing that optimizes cost while maintaining capability requirements

### 5. **Audimodal Integration**
Leverages existing multimodal security capabilities for comprehensive content scanning

### 6. **MCP Ecosystem Integration**
Seamless integration with 1,535+ MCP servers through dedicated tas-mcp orchestrator service

## Success Metrics

### Technical Metrics
- **Latency**: <50ms routing overhead (95th percentile)
- **Availability**: 99.9% uptime with automatic failover
- **Security**: <0.1% false positive rate on threat detection
- **Cost Reduction**: 30-60% savings through intelligent routing

### Business Metrics
- **Time to Market**: 10x faster than custom implementation
- **Operational Efficiency**: 90% reduction in LLM-related security incidents
- **Developer Productivity**: Single API for all LLM providers
- **Compliance**: Automated PII detection and GDPR compliance

This design creates the definitive **WAF for LLM traffic** - providing security, performance, cost optimization, and analytics in a single, intelligently designed system.