// Package ui owns the browser contract and HTTP boundary. Runtime adapters keep
// configuration and credentials in their owning packages.
package ui

import (
	"context"
	"net/url"
	"time"
)

type State struct {
	Version       string       `json:"version"`
	Now           time.Time    `json:"now"`
	Started       time.Time    `json:"started"`
	Lifecycle     string       `json:"lifecycle"`
	ActiveProfile string       `json:"activeProfile"`
	DefaultPool   string       `json:"defaultPool"`
	Profiles      []Profile    `json:"profiles"`
	Connections   []Connection `json:"connections"`
	Models        []Model      `json:"models"`
	Families      []RouteRow   `json:"families"`
	Routes        []RouteRow   `json:"routes"`
	Pools         []Pool       `json:"pools"`
	Efforts       []string     `json:"efforts"`
	Summary       Summary      `json:"summary"`
	Sessions      []Session    `json:"sessions"`
	Interception  Interception `json:"interception"`
	ReloadErrors  []string     `json:"reloadErrors"`
}
type Profile struct {
	DefaultPool  string                      `json:"defaultPool"`
	Name         string                      `json:"name"`
	FamilyRoutes map[string]map[string]Route `json:"familyRoutes"`
	Routes       map[string]map[string]Route `json:"routes"`
	ModelPools   map[string][]Target         `json:"modelPools"`
}
type Route struct {
	Mode   string `json:"mode"`
	Pool   string `json:"pool,omitempty"`
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}
type Target struct {
	Model     string            `json:"model"`
	Effort    string            `json:"effort,omitempty"`
	EffortMap map[string]string `json:"effortMap,omitempty"`
}
type Connection struct {
	AuthMode            string          `json:"authMode"`
	SubscriptionEnabled bool            `json:"subscriptionEnabled"`
	CatalogUpdated      time.Time       `json:"catalogUpdated"`
	UsageURL            string          `json:"usageURL"`
	Name                string          `json:"name"`
	DisplayName         string          `json:"displayName"`
	Type                string          `json:"type"`
	BaseURL             string          `json:"baseURL"`
	KeySet              bool            `json:"keySet"`
	Connected           bool            `json:"connected"`
	Pending             bool            `json:"pending"`
	Error               string          `json:"error"`
	Models              []string        `json:"models"`
	Limits              []Limit         `json:"limits"`
	ResetsKnown         bool            `json:"resetsKnown"`
	Resets              int64           `json:"resets"`
	Updated             time.Time       `json:"updated"`
	Refreshing          bool            `json:"refreshing"`
	Usage               ConnectionUsage `json:"usage"`
}

// ConnectionUsage contains only measured counters, never private replay state.
// CacheInputTokens is the denominator for CachedInputTokens: missing cache
// metadata must not dilute the hit rate with an invented zero.
type ConnectionUsage struct {
	Since                     time.Time `json:"since"`
	Requests                  int       `json:"requests"`
	MeasuredRequests          int       `json:"measuredRequests"`
	CacheMeasuredRequests     int       `json:"cacheMeasuredRequests"`
	InputTokens               int64     `json:"inputTokens"`
	CacheInputTokens          int64     `json:"cacheInputTokens"`
	CachedInputTokens         int64     `json:"cachedInputTokens"`
	UncachedInputTokens       int64     `json:"uncachedInputTokens"`
	CacheWriteTokens          int64     `json:"cacheWriteTokens"`
	OutputTokens              int64     `json:"outputTokens"`
	ReasoningTokens           int64     `json:"reasoningTokens"`
	ReasoningMeasuredRequests int       `json:"reasoningMeasuredRequests"`
	UpstreamCalls             int       `json:"upstreamCalls"`
	ContinuationRequests      int       `json:"continuationRequests"`
	InvalidRequests           int       `json:"invalidRequests"`
	LowCache                  bool      `json:"lowCache"`
}
type Limit struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	Known     bool      `json:"known"`
	Remaining float64   `json:"remaining"`
	Reset     time.Time `json:"reset"`
	Blocked   bool      `json:"blocked"`
}
type Model struct {
	Key      string   `json:"key"`
	Provider string   `json:"provider"`
	Model    string   `json:"model"`
	Efforts  []string `json:"efforts"`
}
type RouteRow struct {
	Model   string   `json:"model"`
	Label   string   `json:"label"`
	Choices []Choice `json:"choices"`
}
type Choice struct {
	Effort      string `json:"effort"`
	Destination string `json:"destination"`
	Inherited   string `json:"inherited"`
	Configured  bool   `json:"configured"`
}
type Pool struct {
	Name          string   `json:"name"`
	Type          string   `json:"type"`
	MaxInputChars int      `json:"maxInputChars"`
	Failover      bool     `json:"failover"`
	FirstByte     int      `json:"firstByte"`
	ProbeEvery    int      `json:"probeEvery"`
	Members       []Member `json:"members"`
}
type Member struct {
	Model     string            `json:"model"`
	Effort    string            `json:"effort"`
	EffortMap map[string]string `json:"effortMap"`
	Efforts   []string          `json:"efforts"`
	Cooling   bool              `json:"cooling"`
}
type Summary struct {
	Total    int `json:"total"`
	Errors5m int `json:"errors5m"`
	Pending  int `json:"pending"`
}
type SessionRoute struct {
	Model          string `json:"model"`
	RequestedModel string `json:"requestedModel"`
	Connection     string `json:"connection"`
	Effort         string `json:"effort"`
	Route          string `json:"route"`
	Requests       int    `json:"requests"`
	Pending        int    `json:"pending"`
}

type Session struct {
	Title          string          `json:"title"`
	Project        string          `json:"project"`
	Branch         string          `json:"branch"`
	Routes         []SessionRoute  `json:"routes"`
	ID             string          `json:"id"`
	Model          string          `json:"model"`
	RequestedModel string          `json:"requestedModel"`
	Preview        string          `json:"preview"`
	Connection     string          `json:"connection"`
	Effort         string          `json:"effort"`
	Route          string          `json:"route"`
	Pending        int             `json:"pending"`
	LastAt         time.Time       `json:"lastAt"`
	Error          string          `json:"error"`
	Requests       int             `json:"requests"`
	Usage          ConnectionUsage `json:"usage"`
}
type Interception struct {
	Enabled    bool   `json:"enabled"`
	CanRestore bool   `json:"canRestore"`
	Error      string `json:"error"`
}
type Request struct {
	ID           string    `json:"id"`
	Method       string    `json:"method"`
	Path         string    `json:"path"`
	Unrecognized string    `json:"unrecognized"`
	FallbackPool string    `json:"fallbackPool"`
	Session      string    `json:"session"`
	Start        time.Time `json:"start"`
	End          time.Time `json:"end"`
	Model        string    `json:"model"`
	Served       string    `json:"served"`
	Connection   string    `json:"connection"`
	Route        string    `json:"route"`
	Status       int       `json:"status"`
	Pending      bool      `json:"pending"`
	Failed       bool      `json:"failed"`
	DurationMs   int64     `json:"durationMs"`
	Preview      string    `json:"preview"`
	Error        string    `json:"error"`
}
type RequestList struct {
	Items        []Request        `json:"items"`
	Total        int              `json:"total"`
	Offset       int              `json:"offset"`
	Limit        int              `json:"limit"`
	SessionUsage *ConnectionUsage `json:"sessionUsage,omitempty"`
}
type Attempt struct {
	Model      string `json:"model"`
	Error      string `json:"error"`
	DurationMs int64  `json:"durationMs"`
}
type Detail struct {
	Request
	RequestedEffort  *string           `json:"requestedEffort"`
	SentEffort       *string           `json:"sentEffort"`
	CaptureTruncated bool              `json:"captureTruncated"`
	RequestNote      string            `json:"requestNote"`
	RequestBody      string            `json:"request"`
	Sent             string            `json:"sent"`
	Response         string            `json:"response"`
	Attempts         []Attempt         `json:"attempts"`
	Headers          map[string]string `json:"headers"`
	Usage            map[string]int    `json:"usage"`
	Truncated        bool              `json:"truncated"`
}
type Action struct {
	Action string            `json:"action"`
	Fields map[string]string `json:"fields"`
}
type Result struct {
	Message string `json:"message"`
	URL     string `json:"url,omitempty"`
}
type Backend interface {
	State(context.Context) State
	Requests(url.Values) RequestList
	Detail(string) (Detail, bool)
	Action(context.Context, Action) (Result, error)
	Writable() bool
}
