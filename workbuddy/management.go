// management.go implements the WorkBuddy management API and web panel:
// account dashboard (nickname, credits, plan, check-in streak), manual/auto
// check-in (daily at 09:00 and 21:00 local time), and quota refresh.
package main

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// billingBase hosts the Buddy-gas-station check-in and resource-package APIs.
// It is a var (not const) so tests can override it with an httptest server.
var billingBase = "https://www.codebuddy.cn"

// billingBaseGlobal is the international (www.workbuddy.ai) billing base.
var billingBaseGlobal = "https://www.workbuddy.ai"

// If the panel later wants to surface "usage export ready", re-add it and wire
// it into buildDashboardEx's response.

// -----------------------------------------------------------------------------
// Account listing via host auth callbacks
// -----------------------------------------------------------------------------

// wbAccount is one row of the dashboard.
type wbAccount struct {
	AuthIndex    string          `json:"auth_index"`
	AuthID       string          `json:"auth_id,omitempty"`
	Name         string          `json:"name"`
	Label        string          `json:"label"`
	Nickname     string          `json:"nickname"`
	UID          string          `json:"uid"`
	Region       string          `json:"region"` // "cn" or "global"
	Plan         string          `json:"plan"`
	Status       string          `json:"status"`
	Disabled     bool            `json:"disabled"`
	Exhausted    bool            `json:"exhausted"`
	Selected     bool            `json:"selected"` // panel active routing card
	Credits      *creditsSummary `json:"credits,omitempty"`
	Checkin      *checkinSummary `json:"checkin,omitempty"`
	TrialClaimed bool            `json:"trial_claimed,omitempty"` // Global: expert trial already claimed
	Error        string          `json:"error,omitempty"`
}

type creditsSummary struct {
	// TotalRemain is currently usable credits across all active packages.
	TotalRemain int64 `json:"total_remain"`
	// TotalUsed is consumed credits in the current cycle (sum of packages).
	TotalUsed int64 `json:"total_used"`
	// TotalSize is the credit capacity/pool (sum of package sizes). remain+used ≈ size.
	TotalSize int64 `json:"total_size"`
	// PackCount is number of resource packages included in the aggregate.
	PackCount int `json:"pack_count"`
	// FetchedAt is when this snapshot was taken (RFC3339). Upstream billing lag
	// can make remain/used look "stuck" for minutes after chat; compare this
	// timestamp — not only the numbers — when diagnosing frozen credits.
	FetchedAt string           `json:"fetched_at,omitempty"`
	Packages  []packageSummary `json:"packages"`
}

type packageSummary struct {
	Name       string `json:"name"`
	Remain     int64  `json:"remain"`
	Used       int64  `json:"used"`
	Size       int64  `json:"size"`
	CycleStart string `json:"cycle_start"`
	CycleEnd   string `json:"cycle_end"`
}

type checkinSummary struct {
	Active          bool     `json:"active"`
	TodayCheckedIn  bool     `json:"today_checked_in"`
	StreakDays      int64    `json:"streak_days"`
	DailyCredit     int64    `json:"daily_credit"`
	TodayCredit     int64    `json:"today_credit"`
	TotalCredits    int64    `json:"total_credits"`
	WeekCheckinDays int64    `json:"week_checkin_days"`
	ActivityName    string   `json:"activity_name"`
	Season          int64    `json:"season"`
	CheckinDates    []string `json:"checkin_dates,omitempty"`
}

// with a transient error (HTTP 5xx or transport error). codebuddy.cn
// intermittently returns 500s; without a retry a single hiccup surfaces as a
// panel error even though the very next request would succeed.
var billingRetryDelays = []time.Duration{300 * time.Millisecond, 900 * time.Millisecond}
//
//	CapacityRemain/Used/Size         — lifetime package totals (Used often ≈0
//	                                   for monthly-refresh free packs)
//	CycleCapacityRemain/Used/Size    — the active billing cycle; Used is
//	                                   sometimes omitted entirely
type resourcePackage struct {
	PackageName         string `json:"PackageName"`
	CapacityRemain      int64  `json:"CapacityRemain"`
	CapacityUsed        int64  `json:"CapacityUsed"`
	CapacitySize        int64  `json:"CapacitySize"`
	CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
	CycleCapacitySize   int64  `json:"CycleCapacitySize"`
	CycleStartTime      string `json:"CycleStartTime"`
	CycleEndTime        string `json:"CycleEndTime"`
}

// credits/checkin/plan fields are left empty — the panel renders skeletons
// and fetches them lazily via /credits?auth_index=<idx>. This avoids hitting
// upstream billing APIs for all accounts simultaneously on page load (which
// causes 500 from rate-limited /v2/billing/meter/get-user-resource).
func buildDashboardEx(force, fetchCredits bool) map[string]any {
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	// Prune cache entries for accounts that no longer exist (auth deleted via
	// CPA UI) or whose TTL expired long ago. Without this, accountCache grows
	// monotonically for the lifetime of the process.
	live := make(map[string]struct{}, len(files))
	for _, f := range files {
		live[f.ID] = struct{}{}
	}
	accountCache.Range(func(key, value any) bool {
		idx, _ := key.(string)
		if _, ok := live[idx]; !ok {
			accountCache.Delete(key)
			checkinLocks.Delete(key)
			lifecycleState.Delete(key)
			return true
		}
		if e, ok := value.(*accountCacheEntry); ok && time.Since(e.fetched) > 4*accountCacheTTL {
			accountCache.Delete(key)
		}
		return true
	})
	// Also prune stale lifecycle state and checkin locks for gone accounts.
	pruneLifecycleState()
	pruneCheckinLocks()
	out := make([]wbAccount, len(files))
	// Accounts are independent — fetch their dashboards concurrently. With 4
	// accounts this cuts cold-load latency from ~4×(3 serial upstream calls)
	// to roughly one slowest account.
	var wg sync.WaitGroup
	for i, f := range files {
		wg.Add(1)
		go func(i int, f pluginapi.HostAuthFileEntry) {
			defer wg.Done()
			acct := wbAccount{
				AuthIndex: f.AuthIndex,
				AuthID:    f.ID,
				Name:      f.Name,
				Label:     f.Label,
				Status:    f.Status,
				Disabled:  f.Disabled,
			}
			sa, phys, err := hostAuthGetBundle(f.AuthIndex)
			if err != nil {
				acct.Error = "load auth: " + err.Error()
				out[i] = acct
				return
			}
			// Physical file is source of truth for disabled (host list may lag).
			if phys != nil {
				acct.Disabled = phys.Disabled
				if phys.Name != "" {
					acct.Name = phys.Name
				}
			}
			acct.Nickname = sa.Account.Nickname
			acct.UID = sa.Account.UID
			acct.Region = accountRegion(sa)
			if fetchCredits {
				plan, ci, cr, errs := cachedAccountDetails(f.ID, sa, force)
				acct.Plan = plan
				acct.Checkin = ci
				acct.Credits = cr
				acct.Exhausted = isCreditsExhausted(cr)
				if isGlobalDomain(sa.Auth.Domain) {
					acct.TrialClaimed = hasTrialPack(cr)
				}
				// Keep note in sync (throttled); do not block dashboard on save errors.
				_ = syncAuthNote(f.AuthIndex, f.ID, sa, cr, acct.Disabled)
				acct.Error = strings.Join(errs, "; ")
			} else {
				// Light load: use cached values if available, but don't fetch upstream.
				if v, ok := accountCache.Load(f.ID); ok {
					if e, ok2 := v.(*accountCacheEntry); ok2 {
						acct.Plan = e.plan
						acct.Checkin = e.checkin
						acct.Credits = e.credits
						acct.Exhausted = isCreditsExhausted(e.credits)
						if isGlobalDomain(sa.Auth.Domain) {
							acct.TrialClaimed = hasTrialPack(e.credits)
						}
					}
				}
			}
			out[i] = acct
		}(i, f)
	}
	wg.Wait()
	// After refresh (force), run lifecycle so exhaust→disable/delete is immediate.
	var life []map[string]any
	if force && lifecycleEnabled() {
		life = reconcileAllAccounts(true)
		// Drop accounts deleted during reconcile (Global exhaust) and refresh
		// disabled/exhausted from disk/cache (host list may lag after save).
		if files2, err2 := hostAuthList(); err2 == nil {
			live := make(map[string]struct{}, len(files2))
			disabledBy := make(map[string]bool, len(files2))
			for _, f := range files2 {
				live[f.AuthIndex] = struct{}{}
				// Prefer host list Disabled after reconcile; avoids N extra host.auth.get.
				// Dashboard row load already used hostAuthGetBundle for physical truth.
				disabledBy[f.AuthIndex] = f.Disabled
			}
			filtered := out[:0]
			for _, a := range out {
				if _, ok := live[a.AuthIndex]; !ok {
					continue
				}
				if d, ok := disabledBy[a.AuthIndex]; ok {
					a.Disabled = d
				}
				// Credits may have been refreshed during reconcile — re-read cache.
				if v, ok := accountCache.Load(a.AuthID); ok {
					if e, ok2 := v.(*accountCacheEntry); ok2 {
						if e.credits != nil {
							a.Credits = e.credits
							a.Exhausted = isCreditsExhausted(e.credits)
						}
						if e.plan != "" {
							a.Plan = e.plan
						}
						if e.checkin != nil {
							a.Checkin = e.checkin
						}
					}
				}
				filtered = append(filtered, a)
			}
			out = filtered
		}
	}
	checkinAutoMu.RLock()
	auto := checkinAuto
	checkinAutoMu.RUnlock()
	// Ensure default selection for panel + scheduler (first usable card).
	activeID := ensureDefaultActiveAuth(out)
	// Aggregate credits for panel/API consumers (all accounts currently in out).
	sum := summarizeCredits(out)
	// Mark selected account in list for UI.
	for i := range out {
		out[i].Selected = out[i].AuthID == activeID
	}
	resp := map[string]any{
		"accounts":       out,
		"active_auth":    activeID,
		"checkin_auto":   auto,
		"lifecycle_auto": lifecycleEnabled(),
		"schedule":       []string{"09:00", "21:00"},
		"server_time":    time.Now().Format("2006-01-02 15:04:05"),
		"summary":        sum,
	}
	if len(life) > 0 {
		resp["lifecycle"] = life
	}
	return resp
}

// summarizeCredits aggregates remain/used across dashboard accounts.
func summarizeCredits(accounts []wbAccount) map[string]any {
	var remain, used, size, cnRemain, cnUsed, cnSize, glRemain, glUsed, glSize int64
	var known, disabledN, exhaustedN, packs int
	for _, a := range accounts {
		if a.Disabled {
			disabledN++
		}
		if a.Exhausted {
			exhaustedN++
		}
		if a.Credits == nil {
			continue
		}
		cr := a.Credits
		if cr.TotalRemain == 0 && cr.TotalUsed == 0 && cr.TotalSize == 0 && len(cr.Packages) == 0 {
			continue
		}
		known++
		remain += cr.TotalRemain
		used += cr.TotalUsed
		size += cr.TotalSize
		packs += cr.PackCount
		if a.Region == "global" {
			glRemain += cr.TotalRemain
			glUsed += cr.TotalUsed
			glSize += cr.TotalSize
		} else {
			cnRemain += cr.TotalRemain
			cnUsed += cr.TotalUsed
			cnSize += cr.TotalSize
		}
	}
	total := remain + used
	if size > total {
		total = size
	}
	return map[string]any{
		"account_count":   len(accounts),
		"known_count":     known,
		"disabled_count":  disabledN,
		"exhausted_count": exhaustedN,
		"pack_count":      packs,
		"total_remain":    remain,
		"total_used":      used,
		"total_size":      size,
		"total":           total,
		"cn_remain":       cnRemain,
		"cn_used":         cnUsed,
		"cn_size":         cnSize,
		"global_remain":   glRemain,
		"global_used":     glUsed,
		"global_size":     glSize,
	}
}

// -----------------------------------------------------------------------------
// Auto check-in scheduler (09:00 / 21:00 local)
// -----------------------------------------------------------------------------

// Management API routes + handler
// -----------------------------------------------------------------------------

type managementRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

// managementBasePathCache holds the host-injected BasePath so handleManagement
// doesn't hardcode /v0/management. Falls back to the historical default if the
// host doesn't provide one (older CPA builds).
var (
	managementBasePathCache   = "/v0/management"
	managementBasePathCacheMu sync.RWMutex
)

func loadedManagementBasePath() string {
	managementBasePathCacheMu.RLock()
	defer managementBasePathCacheMu.RUnlock()
	return managementBasePathCache
}

func setManagementBasePath(p string) {
	p = strings.TrimRight(strings.TrimSpace(p), "/")
	if p == "" {
		return
	}
	managementBasePathCacheMu.Lock()
	managementBasePathCache = p
	managementBasePathCacheMu.Unlock()
}

func managementRegistration() managementRegistrationResponse {
	base := "/plugins/" + providerName
	return managementRegistrationResponse{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: base + "/accounts", Description: "List WorkBuddy accounts with credits, plan and check-in status."},
			{Method: http.MethodPost, Path: base + "/refresh", Description: "Force refresh quota/cache for all accounts."},
			{Method: http.MethodPost, Path: base + "/checkin", Description: "Manually check in one account (auth_index) or all."},
			{Method: http.MethodPost, Path: base + "/checkin/config", Description: "Toggle auto check-in (enabled: true/false)."},
			{Method: http.MethodGet, Path: base + "/credits", Description: "Get real-time credits for one (auth_index query) or all accounts."},
			{Method: http.MethodPost, Path: base + "/import", Description: "Import WorkBuddy credential JSON (nested or flat) into host auth store."},
			{Method: http.MethodPost, Path: base + "/trial", Description: "Claim expert trial pack for one Global account (auth_index). One-time 250 credits / 14 days."},
			{Method: http.MethodPost, Path: base + "/select", Description: "Select the active account card used for chat routing (body: {auth_index})."},
		},
		Resources: []resourceRoute{
			{Path: "/panel", Menu: "WorkBuddy", Description: "WorkBuddy dashboard: credits, check-in, plan, import."},
		},
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	path := strings.TrimRight(req.Path, "/")

	// Browser UI resource routes (unauthenticated).
	resPrefix := "/v0/resource/plugins/" + providerName
	if req.Method == http.MethodGet && strings.HasPrefix(path, resPrefix) {
		sub := strings.TrimPrefix(path, resPrefix)
		return okEnvelope(mgmtHTMLResponse(servePanel(sub)))
	}

	// Plugin-layer auth + rate limit for mutating endpoints (v0.6.31).
	// Only enforced when management_key is configured; otherwise host middleware
	// is the sole guard (historical default).
	if req.Method == http.MethodPost || mutatingManagementPath(path) {
		ip := managementClientIP(req)
		if !allowManagementRequest(ip) {
			return okEnvelope(mgmtJSONResponse(http.StatusTooManyRequests, map[string]any{
				"error": "rate limit exceeded, try again later",
			}))
		}
		if status, msg := checkManagementAuth(req); status != 0 {
			return okEnvelope(mgmtJSONResponse(status, map[string]any{"error": msg}))
		}
	}

	base := loadedManagementBasePath() + "/plugins/" + providerName
	switch {
	case req.Method == http.MethodGet && path == base+"/accounts":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, buildDashboardEx(false, false)))
	case req.Method == http.MethodPost && path == base+"/refresh":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, buildDashboardEx(true, true)))
	case req.Method == http.MethodPost && path == base+"/checkin":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleManualCheckin(req)))
	case req.Method == http.MethodPost && path == base+"/checkin/config":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleCheckinConfig(req)))
	case req.Method == http.MethodGet && path == base+"/credits":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleCreditsQuery(req)))
	case req.Method == http.MethodPost && path == base+"/import":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleImportAuth(req)))
	case req.Method == http.MethodPost && path == base+"/trial":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleClaimTrial(req)))
	case req.Method == http.MethodPost && path == base+"/select":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleSelectAuth(req)))
	}
	return okEnvelope(mgmtJSONResponse(http.StatusNotFound, map[string]any{"error": "not found: " + path}))
}

// -----------------------------------------------------------------------------
// Plugin-layer management auth + rate limit (v0.6.31)
// -----------------------------------------------------------------------------
//
// When management_key is configured (config_yaml or WB_MANAGEMENT_KEY env), all
// mutating endpoints under /v0/management/plugins/workbuddy/* require a matching
// Bearer token. Read-only GET endpoints (accounts/credits/panel) pass through so
// the panel can render before the user has pasted a key — the panel itself
// supplies the key on every call via Authorization header.
//
// A per-IP token-bucket rate limiter guards against brute-force when the key
// check fails repeatedly.

const (
	mgmtRateLimitCapacity = 5                // burst
	mgmtRateLimitRefill   = time.Minute / 10 // 1 token per 6s
	mgmtRateLimitTTL      = 10 * time.Minute // idle entry eviction
)

type mgmtRateEntry struct {
	tokens   float64
	lastSeen time.Time
}

var (
	mgmtRateLimit   = map[string]*mgmtRateEntry{}
	mgmtRateLimitMu sync.Mutex
)

func loadedManagementKey() string {
	managementAPIKeyMu.RLock()
	defer managementAPIKeyMu.RUnlock()
	return managementAPIKey
}

// checkManagementAuth returns an HTTP status + error message when the request
// should be rejected. status=0 means allow.
func checkManagementAuth(req pluginapi.ManagementRequest) (int, string) {
	want := loadedManagementKey()
	if want == "" {
		return 0, "" // plugin-layer auth disabled; rely on host middleware
	}
	got := strings.TrimSpace(req.Headers.Get("Authorization"))
	if !strings.HasPrefix(got, "Bearer ") {
		return http.StatusUnauthorized, "missing Bearer token"
	}
	token := strings.TrimSpace(strings.TrimPrefix(got, "Bearer "))
	if subtle.ConstantTimeCompare([]byte(token), []byte(want)) != 1 {
		return http.StatusForbidden, "invalid management key"
	}
	return 0, ""
}

// allowManagementRequest applies a per-IP token bucket. ip may be empty when the
// host doesn't forward X-Forwarded-For / RemoteAddr — in that case use a single
// global bucket.
func allowManagementRequest(ip string) bool {
	if ip == "" {
		ip = "_global"
	}
	mgmtRateLimitMu.Lock()
	defer mgmtRateLimitMu.Unlock()
	now := time.Now()
	e, ok := mgmtRateLimit[ip]
	if !ok {
		e = &mgmtRateEntry{tokens: mgmtRateLimitCapacity, lastSeen: now}
		mgmtRateLimit[ip] = e
	}
	// Refill.
	elapsed := now.Sub(e.lastSeen)
	e.tokens += float64(elapsed) / float64(mgmtRateLimitRefill)
	if e.tokens > mgmtRateLimitCapacity {
		e.tokens = mgmtRateLimitCapacity
	}
	e.lastSeen = now
	if e.tokens < 1 {
		return false
	}
	e.tokens--
	// Lazy eviction of idle entries (don't grow the map forever).
	if len(mgmtRateLimit) > 1024 {
		for k, v := range mgmtRateLimit {
			if now.Sub(v.lastSeen) > mgmtRateLimitTTL {
				delete(mgmtRateLimit, k)
			}
		}
	}
	return true
}

// managementClientIP extracts a best-effort client identifier for rate limiting.
// CPA host doesn't currently forward RemoteAddr, so fall back to X-Forwarded-For
// / X-Real-IP headers if the deployment adds them via a reverse proxy.
func managementClientIP(req pluginapi.ManagementRequest) string {
	if xff := strings.TrimSpace(req.Headers.Get("X-Forwarded-For")); xff != "" {
		if i := strings.Index(xff, ","); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return xff
	}
	if xr := strings.TrimSpace(req.Headers.Get("X-Real-Ip")); xr != "" {
		return xr
	}
	return ""
}

// mutatingManagementPath reports whether the path performs a write (checkin,
// import, trial claim, select, refresh, config toggle). Read endpoints pass.
func mutatingManagementPath(path string) bool {
	base := loadedManagementBasePath() + "/plugins/" + providerName
	switch path {
	case base + "/refresh",
		base + "/checkin",
		base + "/checkin/config",
		base + "/import",
		base + "/trial",
		base + "/select":
		return true
	}
	return false
}

func mgmtJSONResponse(status int, v any) pluginapi.ManagementResponse {
	body, _ := json.Marshal(v)
	h := http.Header{}
	h.Set("Content-Type", "application/json; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: status, Headers: h, Body: body}
}

func mgmtHTMLResponse(body []byte) pluginapi.ManagementResponse {
	h := http.Header{}
	h.Set("Content-Type", "text/html; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: h, Body: body}
}

// checkinLocks serializes per-account manual check-in (B4).
// Entries are pruned during dashboard prune to avoid unbounded growth
// when auth accounts are deleted/rotated.
var (
	checkinLocks sync.Map // auth_index -> *sync.Mutex
)
// Web panel (self-contained HTML, no external assets)
// -----------------------------------------------------------------------------

func servePanel(sub string) []byte {
	if sub != "" && sub != "/" && sub != "/panel" && sub != "/panel.html" {
		return []byte("<h1>404</h1>")
	}
	return panelHTML
}

//go:embed panel.html
var panelHTML []byte
