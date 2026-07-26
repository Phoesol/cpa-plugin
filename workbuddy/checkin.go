// checkin.go implements daily check-in for CN accounts: the manual
// handleManualCheckin endpoint, the 09:00 / 21:00 auto scheduler, and the
// per-account mutex that prevents duplicate check-ins from racing browser
// tabs. Global accounts are excluded — they use one-shot trial claims instead.
package main

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// check-in schedule: 09:00 and 21:00 local time.
var checkinHours = []int{9, 21}


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

