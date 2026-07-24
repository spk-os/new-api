package gateway

// Gateway Routing types. Based on: SPKOS-LLM 统一网关方案.md Part 1

import "time"

// GatewayYaml is the top-level configuration structure.
type GatewayYaml struct {
	LLMGateway map[string]*StrategyGroup `yaml:"llm_gateway"` // key=group name (default, quality, efficient)
	Providers  map[string]*Provider      `yaml:"providers"`   // key=providerId (zen, ali, nvidia_edu, etc.)
	Cost       *CostConfig               `yaml:"cost"`
}

// StrategyGroup represents a named strategy group (default/quality/efficient).
type StrategyGroup struct {
	Version   string                   `yaml:"version"`
	Enabled   *bool                    `yaml:"enabled"`
	LLMCommon *LLMCommon               `yaml:"llmCommon"`
	Affinity  *AffinityConfig          `yaml:"affinity"`
	Routing   *RoutingConfig           `yaml:"routing"`
	Log       *LogConfig                `yaml:"log"`
	Client    map[string]*ClientPolicy `yaml:"client"` // key=clientId
}

// LLMCommon contains global defaults for timeout, rate limiting, and retry.
type LLMCommon struct {
	Timeout         *Timeout         `yaml:"timeout"`
	RateLimiting    *RateLimiting    `yaml:"rateLimiting"`
	ProviderRetry   *ProviderRetry   `yaml:"providerRetry"`
}

// Timeout configuration.
type Timeout struct {
	ConnectTimeout    int `yaml:"connectTimeout"`    // seconds
	ReadTimeout       int `yaml:"readTimeout"`       // seconds
	StreamIdleTimeout int `yaml:"streamIdleTimeout"` // seconds
}

// RateLimiting configuration.
type RateLimiting struct {
	Enabled          bool   `yaml:"enabled"`
	Concurrency      int    `yaml:"concurrency"`
	WindowRate        string `yaml:"windowRate"`        // e.g. "1h-100"
	OverLimitStrategy string `yaml:"overLimitStrategy"` // queue | downgrade | reject
	QueueTimeout      int    `yaml:"queueTimeout"`      // seconds
}

// ProviderRetry contains retry and failover configuration.
type ProviderRetry struct {
	Enabled   bool            `yaml:"enabled"`
	Model     *ModelRetry     `yaml:"model"`
	Provider  *ProviderSwitch `yaml:"provider"`
	Global    *GlobalRetry    `yaml:"global"`
}

// ModelRetry - per-model retry configuration.
type ModelRetry struct {
	MaxRetries             int   `yaml:"maxRetries"`
	BackoffIntervals       []int `yaml:"backoffIntervals"`
	RetryableStatusCodes   []int `yaml:"retryableStatusCodes"`
	NonRetryableStatusCodes []int `yaml:"nonRetryableStatusCodes"`
	RetryOnTimeout         bool  `yaml:"retryOnTimeout"`
	RetryByKey             bool  `yaml:"retryByKey"`
}

// ProviderSwitch - cross-provider switching rules.
type ProviderSwitch struct {
	SwitchDelay    int `yaml:"switchDelay"`    // seconds
	CooldownPeriod int `yaml:"cooldownPeriod"` // seconds
	FailureThreshold int `yaml:"failureThreshold"`
	FailureWindow  int `yaml:"failureWindow"`  // seconds
}

// GlobalRetry - global retry limits.
type GlobalRetry struct {
	MaxTotalRetries  int `yaml:"maxTotalRetries"`
	MaxTotalTimeout  int `yaml:"maxTotalTimeout"` // seconds
}

// AffinityConfig - session affinity configuration.
type AffinityConfig struct {
	Enabled             bool                     `yaml:"enabled"`
	ClientIdentification []ClientIdentRule       `yaml:"clientIdentification"`
	TaskIdentification   []TaskIdentRule         `yaml:"taskIdentification"`
	Binding             *AffinityBinding         `yaml:"binding"`
	Failover            *AffinityFailover        `yaml:"failover"`
}

// ClientIdentRule - how to identify the client.
type ClientIdentRule struct {
	Source   string `yaml:"source"`   // header | userAgent | token
	Key      string `yaml:"key"`      // header name (for source=header)
	Priority int    `yaml:"priority"`
	Patterns []UAPattern `yaml:"patterns"` // for source=userAgent
}

// UAPattern - user-agent pattern matching.
type UAPattern struct {
	Pattern  string `yaml:"pattern"`
	ClientId string `yaml:"clientId"`
}

// TaskIdentRule - how to identify the task/session.
type TaskIdentRule struct {
	Source   string `yaml:"source"`   // header | body
	Key      string `yaml:"key"`      // header name
	Fields   []string `yaml:"fields"` // body field names
	Priority int    `yaml:"priority"`
}

// AffinityBinding - binding storage configuration.
type AffinityBinding struct {
	TTL            int64  `yaml:"ttl"`            // seconds
	MaxTTL         int64  `yaml:"maxTTL"`         // seconds
	ExtendOnAccess bool   `yaml:"extendOnAccess"`
	Storage        string `yaml:"storage"`        // redis | memory
	KeyPattern     string `yaml:"keyPattern"`
}

// AffinityFailover - what to do when affinity-bound model fails.
type AffinityFailover struct {
	Enable        bool   `yaml:"enable"`
	BreakAfterTimeout int `yaml:"breakAfterTimeout"` // seconds
	Priority      string `yaml:"priority"`          // sameModel | sameProvider
	NotifyClient  bool   `yaml:"notifyClient"`
	NotifyHeader  string `yaml:"notifyHeader"`
}

// RoutingConfig - routing and model selection configuration.
type RoutingConfig struct {
	ModelSelection    *ModelSelection    `yaml:"modelSelection"`
	ModelAliases      map[string]interface{} `yaml:"modelAliases"` // string or []string
	ModelCapabilities map[string][]string `yaml:"modelCapabilities"`
}

// ModelSelection - strategy for choosing models.
type ModelSelection struct {
	Strategy string `yaml:"strategy"` // priority | cost
}

// LogConfig - log-related configuration (per strategy group).
type LogConfig struct {
	// ClientIDMap maps a friendly client name to a wildcard pattern.
	// The pattern supports `*` wildcards and is matched as a substring
	// (contains) against the request's User-Agent / X-Agent-Name.
	// Example: "Claude-Desktop": "Claude/*Chrome" matches any UA
	// containing "Claude/<anything>Chrome".
	ClientIDMap map[string]string `yaml:"client_id"`
}

// ClientPolicy - client-specific overrides.
type ClientPolicy struct {
	Affinity       *AffinityConfig `yaml:"affinity"`
	ProviderOrders map[string]int  `yaml:"-"` // providerId → order override (parsed from YAML sub-keys like "zen.order")
}

// Provider - a single upstream provider.
type Provider struct {
	Name         string        `yaml:"name"`
	URL          string        `yaml:"url"`
	Key          string        `yaml:"key"` // comma-separated multi-key
	ChannelType  int           `yaml:"channelType"` // New API channel type constant (1=OpenAI, 17=Ali, 26=Zhipu_v4, 45=VolcEngine etc.)
	Order        int           `yaml:"order"`
	Enabled      bool          `yaml:"enabled"`
	Tags         []string      `yaml:"tags"`
	RateLimiting *RateLimiting `yaml:"rateLimiting"`
	Timeout      *Timeout      `yaml:"timeout"`
	Models       []*ModelGroup `yaml:"models"`
}

// ModelGroup - a group of models within a provider.
type ModelGroup struct {
	Name         string        `yaml:"name"`
	Model        string        `yaml:"model"` // CSV, front = higher priority
	Order        int           `yaml:"order"`
	Enabled      bool          `yaml:"enabled"`
	Tags         []string      `yaml:"tags"`
	RateLimiting *RateLimiting `yaml:"rateLimiting"`
}

// CostConfig - model pricing configuration.
type CostConfig struct {
	Currency string                `yaml:"currency"`
	Default  *ModelCost            `yaml:"default"`
	Models   map[string]*ModelCost `yaml:"models"`
}

// ModelCost - per-model pricing.
type ModelCost struct {
	InputPer1kTokens  float64 `yaml:"inputPer1kTokens"`
	OutputPer1kTokens float64 `yaml:"outputPer1kTokens"`
}

// ============================================================
// Runtime types (not in YAML, used during request processing)
// ============================================================

// EffectiveProfile is the merged result of five-layer strategy resolution.
type EffectiveProfile struct {
	Timeout            *Timeout
	RateLimiting       *RateLimiting
	ProviderRetry      *ProviderRetry
	Affinity           *AffinityConfig
	Routing            *RoutingConfig
	ProviderOrderOverride map[string]int // client overrides for provider order
}

// GatewayOptions parsed from request body "gateway_options" field.
type GatewayOptions struct {
	PreferFree      bool     `json:"prefer_free"`
	PreferProviders []string `json:"prefer_providers"`
	Sticky          *bool    `json:"sticky"` // nil means use config default
}

// Candidate represents a single routing candidate (provider+key+model).
type Candidate struct {
	ProviderId   string   // "zen"
	ProviderName string   // = channel name
	ChannelId    int      // synced New API channel id
	KeyIndex     int      // which key in multi-key array
	Keys         []string // comma-split key list
	ModelGroup   string   // "free"
	ActualModel  string   // "minimax-m3-free"
	IsFree       bool     // modelGroup.Tags contains "free"
}

// GatewayPlan holds the execution plan for a request.
type GatewayPlan struct {
	Profile      *EffectiveProfile
	Retry        *ProviderRetry
	Candidates   []*Candidate
	AffinityHit  bool // true if affinity binding was found and prepended
	Affinity     *AffinityConfig
}

// AffinityBindingRecord is a stored affinity binding.
type AffinityBindingRecord struct {
	ClientId      string
	TaskId        string
	ProviderId    string
	ProviderName  string
	ChannelId     int
	KeyIndex      int
	ActualModel   string
	ModelGroup    string
	BoundAt       time.Time
	ExpiresAt     time.Time
	Broken        bool
	OrigProvider  string // before failover
	OrigModel     string // before failover
}

// ProviderHealth tracks provider failure state.
type ProviderHealth struct {
	FailureCount   int
	CooldownUntil  time.Time
	FailureWindowStart time.Time
}

// ApplyResult from SaveConfig.
type ApplyResult struct {
	Applied         bool     `json:"applied"`
	ChannelsCreated []string `json:"channels_created"`
	ChannelsUpdated []string `json:"channels_updated"`
	ChannelsDisabled []string `json:"channels_disabled"`
	PricingUpdated  int      `json:"pricing_updated"`
	EffectiveAt     string   `json:"effective_at"`
	Errors          []string `json:"errors,omitempty"`
}

// SyncResult from channel sync.
type SyncResult struct {
	Created   []string
	Updated   []string
	Disabled  []string
}

// CostSyncResult from cost sync.
type CostSyncResult struct {
	Updated int
}

// RoutePreviewResult from route preview API.
type RoutePreviewResult struct {
	Group        string       `json:"group"`
	ClientId     string       `json:"client_id"`
	Model        string       `json:"model"`
	Strategy     string       `json:"strategy"`
	Candidates   []*Candidate `json:"candidates"`
	AffinityHit  bool         `json:"affinity_hit"`
	AffinityBinding *AffinityBindingRecord `json:"affinity_binding,omitempty"`
}

// GatewayStats for stats API.
type GatewayStats struct {
	TotalRequests     int64            `json:"total_requests"`
	AffinityHits      int64            `json:"affinity_hits"`
	AffinityMisses    int64            `json:"affinity_misses"`
	ModelSwitches     int64            `json:"model_switches"`
	ProviderRequests  map[string]int64 `json:"provider_requests"`
	ModelRequests     map[string]int64 `json:"model_requests"`
	RetryCounts       map[string]int64 `json:"retry_counts"`
}
