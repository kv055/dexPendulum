// Package mmcapabilities owns the market-maker CEX provider registry, API-key
// capability evaluation, and operator policy persistence shared by every backend.
package mmcapabilities

import (
	"commons/apierror"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	PermFetchDepositAddress = "fetch_deposit_address"
	PermSpotTrading         = "spot_trading"
	PermReadBalances        = "read_balances"
	PermWithdraw            = "withdraw"
	PermHedgePositions      = "hedge_positions"

	StatusGranted     = "granted"
	StatusUnknown     = "unknown"
	StatusDenied      = "denied"
	StatusUnsupported = "unsupported"

	BlockPolicyDisabled    = "policy_disabled"
	BlockUnsupported       = "unsupported"
	BlockNoAccount         = "no_account"
	BlockNoActiveKeys      = "no_active_keys"
	BlockCheckFailed       = "check_failed"
	BlockStale             = "stale"
	BlockMissingPermission = "missing_permission"
)

var (
	spotPermissions  = []string{PermFetchDepositAddress, PermSpotTrading, PermReadBalances, PermWithdraw}
	hedgePermissions = []string{PermHedgePositions}
	allPermissions   = []string{PermFetchDepositAddress, PermSpotTrading, PermReadBalances, PermWithdraw, PermHedgePositions}
)

// DefaultMaxAge allows three missed default ten-minute health-check cycles.
const DefaultMaxAge = 30 * time.Minute

// Provider is one canonical CEX known to either the cost model or MM execution.
type Provider struct {
	ID                 string
	Name               string
	CostSupported      bool
	ExecutionSupported bool
	SpotSupported      bool
	HedgeSupported     bool
}

var providers = []Provider{
	{ID: "kraken", Name: "Kraken", CostSupported: true, ExecutionSupported: true, SpotSupported: true, HedgeSupported: true},
	{ID: "binance", Name: "Binance", CostSupported: true, ExecutionSupported: true, SpotSupported: true, HedgeSupported: true},
	{ID: "coinbase", Name: "Coinbase", CostSupported: true, ExecutionSupported: true, SpotSupported: true, HedgeSupported: true},
	{ID: "bitstamp", Name: "Bitstamp", CostSupported: true},
}

// Providers returns a defensive copy of the canonical registry.
func Providers() []Provider { return append([]Provider(nil), providers...) }

// ProviderByID accepts canonical IDs, display names, and surrounding whitespace.
func ProviderByID(raw string) (Provider, bool) {
	id := strings.ToLower(strings.TrimSpace(raw))
	for _, provider := range providers {
		if id == provider.ID || strings.EqualFold(strings.TrimSpace(raw), provider.Name) {
			return provider, true
		}
	}
	return Provider{}, false
}

// CostSupportedProviderIDs returns the registry-owned optimizer provider list.
func CostSupportedProviderIDs() []string {
	result := make([]string, 0, len(providers))
	for _, provider := range providers {
		if provider.CostSupported {
			result = append(result, provider.ID)
		}
	}
	return result
}

// ExecutionProviders returns canonical names accepted by the MM controller.
func ExecutionProviders() []string {
	result := make([]string, 0, len(providers))
	for _, provider := range providers {
		if provider.ExecutionSupported {
			result = append(result, provider.Name)
		}
	}
	return result
}

// CanonicalPermissions returns the stable API-key permission display order.
func CanonicalPermissions() []string { return append([]string(nil), allPermissions...) }

// ProviderForHedgeVenue maps a futures venue to its CEX policy identity.
func ProviderForHedgeVenue(raw string) (Provider, bool) {
	id := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), "_futures")
	return ProviderByID(id)
}

// Policy is the operator-controlled provider eligibility. Missing rows use the
// enabled defaults so deployment preserves the historical opt-out behavior.
type Policy struct {
	SpotEnabled  bool       `json:"spot_enabled"`
	HedgeEnabled bool       `json:"hedge_enabled"`
	UpdatedAt    *time.Time `json:"updated_at,omitempty"`
}

func DefaultPolicy() Policy { return Policy{SpotEnabled: true, HedgeEnabled: true} }

// Blocker is a stable machine-readable reason that a leg cannot be used.
type Blocker struct {
	Code        string   `json:"code"`
	Permissions []string `json:"permissions,omitempty"`
	Message     string   `json:"message"`
}

// LegState separates exchange capability from the operator policy and exposes
// their effective conjunction.
type LegState struct {
	Supported           bool      `json:"supported"`
	PolicyEnabled       bool      `json:"policy_enabled"`
	Available           bool      `json:"available"`
	EffectiveEnabled    bool      `json:"effective_enabled"`
	BlockingPermissions []string  `json:"blocking_permissions"`
	Blockers            []Blocker `json:"blockers"`
}

// ProviderState is the complete central state consumed by APIs and execution.
type ProviderState struct {
	AccountID            int64      `json:"account_id,omitempty"`
	Provider             string     `json:"provider"`
	Exchange             string     `json:"exchange"`
	CostSupported        bool       `json:"cost_supported"`
	ExecutionSupported   bool       `json:"execution_supported"`
	ActiveKeyCount       int        `json:"active_key_count"`
	PermissionsCheckedAt *time.Time `json:"permissions_checked_at,omitempty"`
	Policy               Policy     `json:"policy"`
	Spot                 LegState   `json:"spot"`
	Hedge                LegState   `json:"hedge"`
}

type keyResult struct {
	permissions map[string]string
	checkedAt   *time.Time
	checkError  string
}

type accountResult struct {
	id   int64
	keys []keyResult
}

// DB is the minimal shared Postgres surface used by Store.
type DB interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// Store loads persisted API-key snapshots and operator policies.
type Store struct {
	db     DB
	maxAge time.Duration
	now    func() time.Time
}

func NewStore(db DB, maxAge time.Duration) *Store {
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}
	return &Store{db: db, maxAge: maxAge, now: time.Now}
}

// States evaluates every registered provider. Providers without an account are
// included and fail closed, eliminating consumer-side provider constants.
func (s *Store) States(ctx context.Context) ([]ProviderState, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("MM capability database is unavailable")
	}
	accounts, err := s.loadAccounts(ctx)
	if err != nil {
		return nil, err
	}
	policies, err := s.loadPolicies(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	states := make([]ProviderState, 0, len(providers))
	for _, provider := range providers {
		policy, ok := policies[provider.ID]
		if !ok {
			policy = DefaultPolicy()
		}
		states = append(states, evaluate(provider, accounts[provider.ID], policy, now, s.maxAge))
	}
	return states, nil
}

// State returns one provider or an error for an unknown identifier.
func (s *Store) State(ctx context.Context, raw string) (ProviderState, error) {
	provider, ok := ProviderByID(raw)
	if !ok {
		return ProviderState{}, fmt.Errorf("unknown CEX provider %q", raw)
	}
	states, err := s.States(ctx)
	if err != nil {
		return ProviderState{}, err
	}
	for _, state := range states {
		if state.Exchange == provider.ID {
			return state, nil
		}
	}
	return ProviderState{}, fmt.Errorf("CEX provider %q is not registered", raw)
}

// UpdatePolicy atomically creates or replaces one provider policy.
func (s *Store) UpdatePolicy(ctx context.Context, raw string, policy Policy) (ProviderState, error) {
	provider, ok := ProviderByID(raw)
	if !ok {
		return ProviderState{}, fmt.Errorf("unknown CEX provider %q", raw)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO mm_cex_provider_policies (provider, spot_enabled, hedge_enabled, updated_at)
		VALUES ($1, $2, $3, CURRENT_TIMESTAMP)
		ON CONFLICT (provider) DO UPDATE SET
			spot_enabled = EXCLUDED.spot_enabled,
			hedge_enabled = EXCLUDED.hedge_enabled,
			updated_at = CURRENT_TIMESTAMP
	`, provider.ID, policy.SpotEnabled, policy.HedgeEnabled); err != nil {
		return ProviderState{}, fmt.Errorf("update MM CEX provider policy: %w", err)
	}
	return s.State(ctx, provider.ID)
}

// EffectiveExclusions returns fail-closed spot and hedge provider IDs.
func EffectiveExclusions(states []ProviderState) (spot, hedge []string) {
	for _, state := range states {
		if state.CostSupported && !state.Spot.EffectiveEnabled {
			spot = append(spot, state.Exchange)
		}
		if state.CostSupported && !state.Hedge.EffectiveEnabled {
			hedge = append(hedge, state.Exchange)
		}
	}
	return spot, hedge
}

// RequireSpot and RequireHedge turn effective state into an execution error.
func RequireSpot(state ProviderState) error  { return requireLeg(state.Provider, "spot", state.Spot) }
func RequireHedge(state ProviderState) error { return requireLeg(state.Provider, "hedge", state.Hedge) }

// Codes for disabled execution legs.
const (
	CodeSpotDisabled  = "spot_capability_disabled"
	CodeHedgeDisabled = "hedge_capability_disabled"
)

func requireLeg(provider, leg string, state LegState) error {
	if state.EffectiveEnabled {
		return nil
	}
	reasons := make([]string, 0, len(state.Blockers))
	for _, blocker := range state.Blockers {
		reasons = append(reasons, strings.TrimSpace(blocker.Message))
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "The exchange did not report this capability as available.")
	}
	technical := fmt.Errorf("%s %s capability is disabled: %s", provider, leg, strings.Join(reasons, "; "))
	if leg == "hedge" {
		return apierror.Wrapf(technical, CodeHedgeDisabled,
			"Hedging on %s isn't available to this bot. %s Fix it under Trading Accounts, or switch the liquidity target to hedgeless mode.", provider, strings.Join(reasons, " ")).
			WithStatus(http.StatusConflict).WithLeg("hedge")
	}
	return apierror.Wrapf(technical, CodeSpotDisabled,
		"Buying on %s isn't available to this bot. %s Fix it under Trading Accounts, or pick an offer whose route uses another exchange.", provider, strings.Join(reasons, " ")).
		WithStatus(http.StatusConflict).WithLeg("spot")
}
