// management.go implements the WorkBuddy management API and web panel:
// account dashboard (nickname, credits, plan, check-in streak), manual/auto
// check-in (daily at 09:00 and 21:00 local time), and quota refresh.
package main

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
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

var (
	schedulerStop chan struct{}
	schedulerMu   sync.Mutex
)

func ensureScheduler() {
	schedulerMu.Lock()
	defer schedulerMu.Unlock()
	if schedulerStop != nil {
		return // already running
	}
	schedulerStop = make(chan struct{})
	go schedulerLoop(schedulerStop)
}

// Note: there is deliberately no stopCheckinScheduler. The plugin shutdown
// export is a no-op (see cliproxyPluginShutdown) because the host invokes it
// during its own runtime teardown, where touching Go sync primitives from the
// plugin's c-shared runtime caused SIGSEGV on every restart.

func nextCheckinTime(now time.Time) time.Time {
	var earliest time.Time
	for _, h := range checkinHours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour) // slot already passed today → tomorrow
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

func schedulerLoop(stop chan struct{}) {
	for {
		next := nextCheckinTime(time.Now())
		timer := time.NewTimer(time.Until(next))
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
			runAutoCheckin()
		}
	}
}

// runAutoCheckin is the scheduled lifecycle tick (09:00 / 21:00).
// CN: optional daily check-in, then reconcile (disable exhausted / reenable after credits).
// Global: no auto trial (one-shot claim is manual only); reconcile may delete exhausted auths.
//
// v0.6.31: per-account work runs concurrently (sem=4) — was serial, so N accounts
// meant 3N serial HTTP round-trips on the billing API. Matches the pattern used
// by buildDashboardEx and handleManualCheckin.
func runAutoCheckin() {
	checkinAutoMu.RLock()
	doCheckin := checkinAuto
	checkinAutoMu.RUnlock()
	// Lifecycle may still run when check-in is off (credit gate).
	if !doCheckin && !lifecycleEnabled() {
		return
	}
	files, err := hostAuthList()
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, f := range files {
		f := f
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			processAutoCheckinAccount(f, doCheckin)
		}()
	}
	wg.Wait()
}

// processAutoCheckinAccount handles one account's scheduled tick. Extracted so
// runAutoCheckin can fan out per-account work without duplicating logic.
func processAutoCheckinAccount(f pluginapi.HostAuthFileEntry, doCheckin bool) {
	// A-24: only fetch sa when needed (checkin). For lifecycle-only paths,
	// let reconcileOneAccount do the single hostAuthGetBundle internally.
	if doCheckin {
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			return
		}
		if isGlobalDomain(sa.Auth.Domain) {
			// Global: never check-in or auto-claim trial. Lifecycle only.
			// Invalidate cache (copy entry, set credits=nil, keep plan/checkin).
			if v, ok := accountCache.Load(f.ID); ok {
				if e, ok2 := v.(*accountCacheEntry); ok2 {
					fresh := *e
					fresh.credits = nil
					fresh.fetched = time.Now()
					accountCache.Store(f.ID, &fresh)
				}
			}
			if lifecycleEnabled() {
				_, _ = reconcileOneAccount(f.AuthIndex, f.ID, true)
			}
			return
		}
		// CN: daily check-in when enabled.
		ci, err := fetchCheckinStatus(sa)
		if err == nil && ci != nil && ci.Active && !ci.TodayCheckedIn {
			if _, callErr := performCheckinCall(sa); callErr == nil {
				// Refresh once after a successful checkin call so cache reflects
				// the post-call state. If the status call fails keep the pre-call
				// snapshot rather than dropping it (v0.6.31: avoid shadowing ci
				// with a second fetch that could race with concurrent readers).
				if ci2, _ := fetchCheckinStatus(sa); ci2 != nil {
					ci = ci2
				}
			}
		}
		// Refresh cache with latest checkin status (merge, don't wipe credits/plan).
		if ci != nil {
			var prev *accountCacheEntry
			if v, ok := accountCache.Load(f.ID); ok {
				prev, _ = v.(*accountCacheEntry)
			}
			entry := &accountCacheEntry{checkin: ci, fetched: time.Now()}
			if prev != nil {
				entry.credits = prev.credits
				entry.plan = prev.plan
			}
			accountCache.Store(f.ID, entry)
		}
		if lifecycleEnabled() {
			_, _ = reconcileOneAccount(f.AuthIndex, f.ID, true)
		}
		return
	}
	// Lifecycle-only (checkin off): reconcile handles its own get.
	if lifecycleEnabled() {
		_, _ = reconcileOneAccount(f.AuthIndex, f.ID, true)
	}
}

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

// checkinCandidate is a CN account that still needs daily check-in after prefilter.
type checkinCandidate struct {
	authIndex string
	authID    string
	nickname  string
	sa        *storedAuth
}

// handleManualCheckin prefilters before any check-in call:
//  1. Global → skip (trial pack, not daily check-in)
//  2. CN already checked in today → skip (not a failure)
//  3. Only remaining CN accounts call performCheckinCall
//
// Batch mode (empty auth_index) never returns Global/already as fake failures.
// Single-account mode still returns a clear skip message for Global/already.
func handleManualCheckin(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	single := authIndex != ""

	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var targets []pluginapi.HostAuthFileEntry
	for _, f := range files {
		if !single || f.AuthIndex == authIndex {
			targets = append(targets, f)
		}
	}
	if len(targets) == 0 {
		return map[string]any{"error": "no matching account"}
	}

	// Phase 1: classify each account concurrently (no side effects).
	phase1 := classifyCheckinTargets(targets, single)

	// Phase 2: check-in only eligible CN accounts concurrently.
	phase2 := executeCheckinBatch(phase1.eligible)

	// Phase 3: aggregate results + summary counters.
	return summarizeCheckinResults(phase1, phase2, len(targets))
}

// checkinPhase1Result is the outcome of classifyCheckinTargets: per-target
// classification plus the side-effect results (errors / global / already) that
// don't need a Phase 2 call.
type checkinPhase1Result struct {
	eligible      []checkinCandidate
	results       []map[string]any
	already       int
	skippedGlobal int
}

// classifyCheckinTargets concurrently determines which targets are CN and
// need a check-in call today. No state mutation happens here — Phase 2 owns
// the side effects.
//
// Concurrent classify: N accounts × status API; serial was multi-second and
// could trip browser/gateway cancel (502 context canceled).
func classifyCheckinTargets(targets []pluginapi.HostAuthFileEntry, single bool) checkinPhase1Result {
	type classResult struct {
		idx    int
		f      pluginapi.HostAuthFileEntry
		sa     *storedAuth
		kind   string // "err" | "global" | "already" | "eligible"
		nick   string
		errMsg string
	}
	classCh := make(chan classResult, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6) // bound upstream concurrency
	for i, f := range targets {
		wg.Add(1)
		go func(i int, f pluginapi.HostAuthFileEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			sa, err := hostAuthGet(f.AuthIndex)
			if err != nil {
				classCh <- classResult{idx: i, f: f, kind: "err", errMsg: err.Error()}
				return
			}
			nick := sa.Account.Nickname
			if isGlobalDomain(sa.Auth.Domain) {
				classCh <- classResult{idx: i, f: f, sa: sa, kind: "global", nick: nick}
				return
			}
			// Prefer fresh status; fall back to cache today_checked_in if status fails.
			ci, ciErr := fetchCheckinStatus(sa)
			if ciErr != nil {
				if cached := cachedCheckinToday(f.ID); cached != nil && *cached {
					classCh <- classResult{idx: i, f: f, sa: sa, kind: "already", nick: nick}
					return
				}
				// Status unknown → still try check-in (upstream is idempotent).
				classCh <- classResult{idx: i, f: f, sa: sa, kind: "eligible", nick: nick}
				return
			}
			if ci != nil && ci.TodayCheckedIn {
				classCh <- classResult{idx: i, f: f, sa: sa, kind: "already", nick: nick}
				return
			}
			classCh <- classResult{idx: i, f: f, sa: sa, kind: "eligible", nick: nick}
		}(i, f)
	}
	go func() { wg.Wait(); close(classCh) }()
	// Preserve input order for stable UI.
	classified := make([]classResult, len(targets))
	for cr := range classCh {
		classified[cr.idx] = cr
	}

	out := checkinPhase1Result{}
	for _, cr := range classified {
		switch cr.kind {
		case "err":
			out.results = append(out.results, map[string]any{
				"auth_index": cr.f.AuthIndex, "error": cr.errMsg, "skipped": false,
			})
		case "global":
			out.skippedGlobal++
			if single {
				out.results = append(out.results, map[string]any{
					"auth_index": cr.f.AuthIndex, "nickname": cr.nick,
					"success": false, "skipped": true, "reason": "global",
					"message": "国际版账号不支持签到，请使用领取专家加油包",
				})
			}
		case "already":
			out.already++
			// Keep checkin in cache so light-load dashboard shows 已签到.
			if ci2, _ := fetchCheckinStatus(cr.sa); ci2 != nil {
				var prev *accountCacheEntry
				if v, ok := accountCache.Load(cr.f.ID); ok {
					prev, _ = v.(*accountCacheEntry)
				}
				entry := &accountCacheEntry{checkin: ci2, fetched: time.Now()}
				if prev != nil {
					entry.credits = prev.credits
					entry.plan = prev.plan
				}
				accountCache.Store(cr.f.ID, entry)
			}
			if lifecycleEnabled() {
				_, _ = reconcileOneAccount(cr.f.AuthIndex, cr.f.ID, true)
			}
			if single {
				out.results = append(out.results, map[string]any{
					"auth_index": cr.f.AuthIndex, "nickname": cr.nick,
					"success": true, "skipped": true, "reason": "already",
					"message": "already checked in today",
				})
			}
		case "eligible":
			out.eligible = append(out.eligible, checkinCandidate{
				authIndex: cr.f.AuthIndex, authID: cr.f.ID, nickname: cr.nick, sa: cr.sa,
			})
		}
	}
	return out
}

// executeCheckinBatch runs the actual check-in calls for eligible CN accounts
// under per-account mutex (B4). Each goroutine re-reads sa + status under the
// lock so a parallel tab can't double-checkin.
func executeCheckinBatch(eligible []checkinCandidate) []map[string]any {
	type checkinOut struct {
		idx int
		out map[string]any
	}
	outCh := make(chan checkinOut, len(eligible))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, c := range eligible {
		wg.Add(1)
		go func(i int, c checkinCandidate) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			mu := checkinLockFor(c.authIndex)
			mu.Lock()
			defer mu.Unlock()

			// Re-read under lock: another tab may have just checked in.
			sa := c.sa
			if sa2, err := hostAuthGet(c.authIndex); err == nil && sa2 != nil {
				sa = sa2
			}
			if ci, _ := fetchCheckinStatus(sa); ci != nil && ci.TodayCheckedIn {
				outCh <- checkinOut{idx: i, out: map[string]any{
					"auth_index": c.authIndex, "nickname": c.nickname,
					"success": true, "skipped": true, "reason": "already",
					"message": "already checked in today",
				}}
				// Keep checkin in cache so light-load dashboard shows 已签到.
				// Merge with prev entry so credits/plan aren't dropped on this
				// fast path (v0.6.31 fix: early-already used to wipe them).
				var prev *accountCacheEntry
				if v, ok := accountCache.Load(c.authID); ok {
					prev, _ = v.(*accountCacheEntry)
				}
				entry := &accountCacheEntry{checkin: ci, fetched: time.Now()}
				if prev != nil {
					entry.credits = prev.credits
					entry.plan = prev.plan
				}
				accountCache.Store(c.authID, entry)
				if lifecycleEnabled() {
					_, _ = reconcileOneAccount(c.authIndex, c.authID, true)
				}
				return
			}
			res, err := performCheckinCall(sa)
			out := map[string]any{"auth_index": c.authIndex, "nickname": c.nickname, "skipped": false}
			if err != nil {
				out["error"] = err.Error()
				out["success"] = false
			} else {
				// performCheckinCall surfaces business errors as success=false+message.
				for k, v := range res {
					out[k] = v
				}
				if msg, _ := out["message"].(string); msg != "" && out["success"] == false {
					// Map "already" style business messages to done, not hard fail.
					low := strings.ToLower(msg)
					if strings.Contains(low, "already") || strings.Contains(msg, "已签") || strings.Contains(msg, "今日") {
						out["success"] = true
						out["skipped"] = true
						out["reason"] = "already"
					}
				}
				if _, ok := out["success"]; !ok {
					out["success"] = true
				}
			}
			// Keep checkin in cache so light-load dashboard shows 已签到.
			// Re-fetch checkin status after successful checkin call.
			if ci2, _ := fetchCheckinStatus(sa); ci2 != nil {
				var prev *accountCacheEntry
				if v, ok := accountCache.Load(c.authID); ok {
					prev, _ = v.(*accountCacheEntry)
				}
				entry := &accountCacheEntry{checkin: ci2, fetched: time.Now()}
				if prev != nil {
					entry.credits = prev.credits
					entry.plan = prev.plan
				}
				accountCache.Store(c.authID, entry)
			} else {
				// Fallback: keep existing cache (may be stale), don't delete.
				if _, ok := accountCache.Load(c.authID); !ok {
					accountCache.Store(c.authID, &accountCacheEntry{
						checkin: &checkinSummary{TodayCheckedIn: true},
						fetched: time.Now(),
					})
				}
			}
			if lifecycleEnabled() {
				_, _ = reconcileOneAccount(c.authIndex, c.authID, true)
			}
			outCh <- checkinOut{idx: i, out: out}
		}(i, c)
	}
	go func() { wg.Wait(); close(outCh) }()
	phase2 := make([]map[string]any, len(eligible))
	for o := range outCh {
		phase2[o.idx] = o.out
	}
	return phase2
}

// summarizeCheckinResults folds Phase 1 + Phase 2 outcomes into the response
// payload the panel expects. Counter rules (kept stable since v0.6.30):
//   - error non-nil                      → fail
//   - reason=="already"                  → already (counted once)
//   - success==true (no reason)          → success
//   - success==false,error==nil          → already if msg says so, else fail
func summarizeCheckinResults(p1 checkinPhase1Result, phase2 []map[string]any, total int) map[string]any {
	results := append([]map[string]any{}, p1.results...)
	successN, failN, already2 := 0, 0, 0
	for _, out := range phase2 {
		if out == nil {
			continue
		}
		results = append(results, out)
		if out["error"] != nil {
			failN++
			continue
		}
		reason, _ := out["reason"].(string)
		if reason == "already" {
			already2++
			continue
		}
		if out["success"] == true {
			successN++
			continue
		}
		if out["success"] == false {
			// business soft-fail without error field
			msg, _ := out["message"].(string)
			if strings.Contains(strings.ToLower(msg), "already") || strings.Contains(msg, "已签") {
				already2++
			} else {
				failN++
			}
		}
	}
	return map[string]any{
		"results": results,
		"summary": map[string]any{
			"total":          total,
			"eligible":       len(p1.eligible),
			"success":        successN,
			"already":        p1.already + already2,
			"skipped_global": p1.skippedGlobal,
			"fail":           failN,
			"attempted":      len(p1.eligible),
		},
	}
}

// checkinLocks serializes per-account manual check-in (B4).
// Entries are pruned during dashboard prune to avoid unbounded growth
// when auth accounts are deleted/rotated.
var (
	checkinLocks sync.Map // auth_index -> *sync.Mutex
)

func checkinLockFor(authIndex string) *sync.Mutex {
	v, _ := checkinLocks.LoadOrStore(authIndex, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// pruneCheckinLocks removes lock entries for auth indices that no longer
// exist in hostAuthList. Call after dashboard prune.
// Lock keys are auth_index (used for host RPC), so live map needs auth_index too.
func pruneCheckinLocks() {
	files, err := hostAuthList()
	if err != nil {
		return
	}
	live := make(map[string]struct{}, len(files))
	for _, f := range files {
		live[f.ID] = struct{}{}
		live[f.AuthIndex] = struct{}{} // checkinLockFor uses auth_index as key
	}
	checkinLocks.Range(func(key, _ any) bool {
		idx, _ := key.(string)
		if _, ok := live[idx]; !ok {
			checkinLocks.Delete(key)
		}
		return true
	})
}

// handleImportAuth accepts nested or flat credential JSON and persists via host.auth.save.
func handleImportAuth(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		JSON json.RawMessage `json:"json"`
		Raw  string          `json:"raw"`
	}
	_ = json.Unmarshal(req.Body, &body)
	raw := []byte(strings.TrimSpace(body.Raw))
	if len(body.JSON) > 0 {
		raw = body.JSON
	}
	if len(raw) == 0 {
		return map[string]any{"success": false, "error": "missing json/raw credential payload"}
	}
	sa, err := parseStored(raw)
	if err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	// Persist nested storage + top-level type/note/logo/disabled for Auth page.
	fileJSON, err := buildAuthFileJSON(sa, false, displayNote(sa, nil, false), nil)
	if err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	auth := toAuthData(sa)
	saveReq := pluginapi.HostAuthSaveRequest{
		Name: auth.FileName,
		JSON: fileJSON,
	}
	saveBody, _ := json.Marshal(saveReq)
	rawResp, err := hostCall(pluginabi.MethodHostAuthSave, saveBody)
	if err != nil {
		return map[string]any{"success": false, "error": "host.auth.save: " + err.Error()}
	}
	var env envelope
	if err := json.Unmarshal(rawResp, &env); err != nil || !env.OK {
		msg := "host.auth.save failed"
		if env.Error != nil && env.Error.Message != "" {
			msg = env.Error.Message
		}
		return map[string]any{"success": false, "error": msg}
	}
	var saveResp pluginapi.HostAuthSaveResponse
	_ = json.Unmarshal(env.Result, &saveResp)
	// Remove legacy workbuddy.json if it exists and differs from the saved name.
	if saveResp.Name != "" && !strings.EqualFold(saveResp.Name, authFileName) {
		legacyPath := strings.TrimSpace(saveResp.Path)
		// Best-effort: if auth dir is known via saveResp.Path parent, try removing sibling workbuddy.json.
		if legacyPath != "" {
			dir := filepath.Dir(legacyPath)
			legacyFile := filepath.Join(dir, authFileName)
			// A-35: use deleteAuthFileInDir for absolute path + directory confinement.
			_ = deleteAuthFileInDir(legacyFile, dir)
		}
	}
	return map[string]any{
		"success":  true,
		"name":     saveResp.Name,
		"path":     saveResp.Path,
		"uid":      sa.Account.UID,
		"nickname": sa.Account.Nickname,
		"file":     auth.FileName,
	}
}

func handleCheckinConfig(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	_ = json.Unmarshal(req.Body, &body)
	checkinAutoMu.Lock()
	if body.Enabled != nil {
		// Runtime-only toggle: the CPA host exposes no plugin-config write
		// callback, so persisting would mean editing the host's config.yaml
		// from inside the plugin (fragile under docker volume mounts). The
		// value from config_yaml wins again on CPA restart.
		checkinAuto = *body.Enabled
	}
	cur := checkinAuto
	checkinAutoMu.Unlock()
	return map[string]any{"checkin_auto": cur, "persistent": false}
}

// handleClaimTrial claims the expert trial pack for one Global account.
// CN accounts are rejected — the trial endpoint is Global-only.
func handleClaimTrial(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	if authIndex == "" {
		return map[string]any{"error": "auth_index is required"}
	}
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	for _, f := range files {
		if f.AuthIndex != authIndex {
			continue
		}
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			return map[string]any{"auth_index": authIndex, "error": err.Error()}
		}
		if !isGlobalDomain(sa.Auth.Domain) {
			return map[string]any{"auth_index": authIndex, "error": "专家加油包仅适用于国际版账号"}
		}
		res, err := performTrialCall(sa)
		out := map[string]any{"auth_index": authIndex, "nickname": sa.Account.Nickname}
		if err != nil {
			out["error"] = err.Error()
		} else {
			for k, v := range res {
				out[k] = v
			}
		}
		// Invalidate credits cache (copy entry, set credits=nil, keep plan/checkin).
		if v, ok := accountCache.Load(f.ID); ok {
			if e, ok2 := v.(*accountCacheEntry); ok2 {
				fresh := *e
				fresh.credits = nil
				fresh.fetched = time.Now()
				accountCache.Store(f.ID, &fresh)
			}
		}
		if lifecycleEnabled() {
			_, _ = reconcileOneAccount(authIndex, f.ID, true)
		}
		return out
	}
	return map[string]any{"error": "account not found"}
}

// handleSelectAuth sets the panel-selected account used for chat routing.
// Region (CN/Global) is read from that account's stored domain on each request.
func handleSelectAuth(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	if authIndex == "" {
		return map[string]any{"error": "auth_index is required", "active_auth": getActiveAuthID()}
	}
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	for _, f := range files {
		if f.AuthIndex != authIndex {
			continue
		}
		if f.Disabled {
			return map[string]any{"error": "账号已禁用，无法选中", "auth_index": authIndex}
		}
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			return map[string]any{"error": err.Error(), "auth_index": authIndex}
		}
		setActiveAuthID(f.ID)
		return map[string]any{
			"ok":          true,
			"active_auth": f.ID,
			"region":      accountRegion(sa),
			"nickname":    sa.Account.Nickname,
			"uid":         sa.Account.UID,
		}
	}
	return map[string]any{"error": "account not found", "auth_index": authIndex}
}

// handleCreditsQuery returns real-time credits for one or all accounts.
// Pass ?auth_index=<idx> to query a single account; omit for all.
// Single-account mode returns full account info (nickname, region, credits,
// exhausted, trial_claimed) so the panel can update one card without
// reloading the entire dashboard.
func handleCreditsQuery(req pluginapi.ManagementRequest) map[string]any {
	authIndex := ""
	if vals := req.Query["auth_index"]; len(vals) > 0 {
		authIndex = strings.TrimSpace(vals[0])
	}
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	// Single-account: return one full account row (like dashboard entry).
	if authIndex != "" {
		for _, f := range files {
			if f.AuthIndex != authIndex {
				continue
			}
			sa, err := hostAuthGet(f.AuthIndex)
			if err != nil {
				return map[string]any{"accounts": []map[string]any{{
					"auth_index": authIndex, "error": "load auth: " + err.Error(),
				}}}
			}
			cr, err := fetchUserResource(sa)
			acct := map[string]any{
				"auth_index": authIndex,
				"nickname":   sa.Account.Nickname,
				"uid":        sa.Account.UID,
				"region":     accountRegion(sa),
				"name":       f.Name,
				"label":      f.Label,
				"disabled":   f.Disabled,
				"selected":   getActiveAuthID() == f.ID,
			}
			if err != nil {
				acct["error"] = err.Error()
			} else {
				acct["credits"] = cr
				acct["exhausted"] = isCreditsExhausted(cr)
				if isGlobalDomain(sa.Auth.Domain) {
					acct["trial_claimed"] = hasTrialPack(cr)
				}
				// Also fetch plan so the badge updates on lazy load.
				acct["plan"] = fetchPaymentType(sa)
				// Update cache so subsequent dashboard loads see fresh data.
				now := time.Now()
				if cr != nil {
					cr.FetchedAt = now.UTC().Format(time.RFC3339)
				}
				// Merge into existing cache entry (keep checkin if present).
				var prev *accountCacheEntry
				if v, ok := accountCache.Load(f.ID); ok {
					prev, _ = v.(*accountCacheEntry)
				}
				var ci *checkinSummary
				if prev != nil {
					ci = prev.checkin
				}
				plan, _ := acct["plan"].(string)
				accountCache.Store(f.ID, &accountCacheEntry{
					checkin: ci, credits: cr, plan: plan, fetched: now,
				})
			}
			return map[string]any{"accounts": []map[string]any{acct}}
		}
		return map[string]any{"error": "account not found"}
	}
	// All accounts: return simplified list.
	type acctCredits struct {
		AuthIndex string          `json:"auth_index"`
		Nickname  string          `json:"nickname"`
		UID       string          `json:"uid"`
		Credits   *creditsSummary `json:"credits,omitempty"`
		Error     string          `json:"error,omitempty"`
	}
	var out []acctCredits
	for _, f := range files {
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			out = append(out, acctCredits{AuthIndex: f.AuthIndex, Error: "load auth: " + err.Error()})
			continue
		}
		cr, err := fetchUserResource(sa)
		ac := acctCredits{AuthIndex: f.AuthIndex, Nickname: sa.Account.Nickname, UID: sa.Account.UID}
		if err != nil {
			ac.Error = err.Error()
		} else {
			ac.Credits = cr
		}
		out = append(out, ac)
	}
	return map[string]any{"accounts": out}
}

// -----------------------------------------------------------------------------
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
