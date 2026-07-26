// oauth.go implements QoderWork's auth flows. Two entry points, one outcome:
//
//  1. PAT import (management panel): user pastes a PAT (`pt-...`) created on
//     qoder.com.cn; the plugin exchanges it for a jobToken pair (jt-/jrt-)
//     and stores both.
//
//  2. OAuth-like flow (AuthProvider.StartLogin): QoderWork has no real
//     OAuth authorization-code flow — its web login is Aliyun SSO SMS →
//     web session cookie → manual PAT creation. So handleStartLogin returns
//     the PAT-creation page URL; the user creates a PAT in their browser
//     and pastes it back. handlePollLogin accepts the pasted PAT and runs
//     the same exchange as path 1.
//
// Refresh uses jrt- (48h TTL) to get a fresh jt- (24h TTL). When jrt-
// expires, the underlying PAT is still valid — we fall back to a fresh
// exchange using the stored PAT.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// doJSON sends method to fullURL with the given headers, parses the
// {code,msg,data} envelope, and returns the inner data payload. httpStatus
// is the upstream HTTP code.
func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		commonHeaders(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d (location: %s)", resp.StatusCode, resp.Header.Get("Location"))
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, truncateRedacted(env.Msg, 120))
	}
	return env.Data, resp.StatusCode, nil
}

// jobTokenResponse mirrors the upstream /api/v1/jobToken/{exchange,refresh}
// response payload.
type jobTokenResponse struct {
	Token                  string `json:"token"`                    // jt-..., 24h
	RefreshToken           string `json:"refresh_token"`            // jrt-..., 48h
	ExpiresAt              string `json:"expires_at"`               // RFC3339
	ExpiresIn              int64  `json:"expires_in"`               // ms
	RefreshTokenExpiresAt  string `json:"refresh_token_expires_at"`
	RefreshTokenExpiresIn  int64  `json:"refresh_token_expires_in"` // ms
}

// doRawJSON sends method to fullURL with the given headers and returns the
// raw response body. Used for endpoints that return plain JSON (no envelope)
// like /api/v1/jobToken/exchange and /api/v1/jobToken/refresh.
func doRawJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		commonHeaders(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d body=%s", resp.StatusCode, truncateRedacted(string(raw), 200))
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d (location: %s)", resp.StatusCode, resp.Header.Get("Location"))
	}
	return raw, resp.StatusCode, nil
}

// exchangePATForJobToken calls POST /api/v1/jobToken/exchange with a PAT and
// returns the resulting jt-/jrt- pair.
func exchangePATForJobToken(pat string) (*jobTokenResponse, error) {
	body, _ := json.Marshal(map[string]string{"personal_token": pat})
	data, _, err := doRawJSON(sharedHTTPClient(), http.MethodPost, endpointJobTokenExchange, nil, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var out jobTokenResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("jobToken exchange parse: %w", err)
	}
	if out.Token == "" || out.RefreshToken == "" {
		return nil, fmt.Errorf("jobToken exchange: empty token pair in response")
	}
	return &out, nil
}

// refreshJobToken calls POST /api/v1/jobToken/refresh with a jrt-.
func refreshJobToken(jrt string) (*jobTokenResponse, error) {
	body, _ := json.Marshal(map[string]string{"refresh_token": jrt})
	data, _, err := doRawJSON(sharedHTTPClient(), http.MethodPost, endpointJobTokenRefresh, nil, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var out jobTokenResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("jobToken refresh parse: %w", err)
	}
	if out.Token == "" {
		return nil, fmt.Errorf("jobToken refresh: empty token in response")
	}
	return &out, nil
}

// userInfoResponse is the minimal subset of GET /api/v1/userinfo we need to
// populate auth identity (uid, nickname).
// Upstream returns: {"id":"...","name":"aliyun...","username":"...",
// "organization_id":"","organization_name":"",...}
type userInfoResponse struct {
	ID               string `json:"id"`         // uuid — used as our UID
	Name             string `json:"name"`       // display name
	Username         string `json:"username"`   // alternate uuid
	Avatar           string `json:"avatar"`
	OrganizationID   string `json:"organization_id"`
	OrganizationName string `json:"organization_name"`
	UserType         string `json:"user_type"` // may be absent; falls back to "personal_professional_trial"
}

// fetchUserInfo queries /api/v1/userinfo with a jt- Bearer to populate the
// auth's identity fields (uid, nickname, user_type for COSY signing).
// The endpoint returns plain JSON (no envelope) — same as jobToken/exchange.
func fetchUserInfo(jt string) (*userInfoResponse, error) {
	req, err := http.NewRequest(http.MethodGet, endpointUserInfo, nil)
	if err != nil {
		return nil, err
	}
	commonHeaders(req)
	req.Header.Set("Authorization", "Bearer "+jt)
	resp, err := sharedHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("userinfo: http %d body=%s", resp.StatusCode, truncateRedacted(string(raw), 200))
	}
	var out userInfoResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("userinfo parse: %w (body=%s)", err, truncateRedacted(string(raw), 200))
	}
	return &out, nil
}

// buildStoredAuthFromJobToken constructs a storedAuth from a PAT + jobToken
// pair + user identity. Called by both PAT import and OAuth-like poll.
func buildStoredAuthFromJobToken(pat string, tok *jobTokenResponse, ui *userInfoResponse) *storedAuth {
	expiresAt := time.Now().Add(24 * time.Hour).Unix()
	if tok.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Millisecond).Unix()
	}

	nickname := ""
	uid := ""
	if ui != nil {
		uid = ui.ID
		nickname = ui.Name
	}

	// PersonalToken is stored inside RefreshToken only when refresh is via PAT
	// (no jrt available). For QoderWork we have a real jrt- (refresh token),
	// so RefreshToken carries the jrt- and the PAT itself is kept in Domain
	// alongside the realm for later re-exchange when jrt- expires (48h).
	return &storedAuth{
		Auth: storedTokens{
			AccessToken:   tok.Token,
			RefreshToken:  tok.RefreshToken,
			PersonalToken: pat, // PAT stored for automatic re-exchange when jrt- expires (48h)
			ExpiresAt:     expiresAt,
			Domain:        "qoder.com.cn",
		},
		Account: storedAccount{
			UID:      uid,
			Nickname: nickname,
		},
	}
}

func uiUserType(ui *userInfoResponse) string {
	if ui == nil || ui.UserType == "" {
		return "personal_professional_trial"
	}
	return ui.UserType
}

// handleStartLogin implements AuthProvider.StartLogin. QoderWork has no real
// OAuth authorization-code flow (KNOWLEDGE §8: web login is Aliyun SSO SMS
// → cookie → manual PAT creation; OAuth client flow is desktop-app only).
// We return the PAT-creation page URL plus metadata pointing the user at the
// management panel's "导入 QoderWork 凭证" button, which is the smoother path
// (single click, no polling). The polling-modal fallback below still accepts
// a pasted PAT for hosts that drive StartLogin/PollLogin flows.
func handleStartLogin(raw []byte) ([]byte, error) {
	state := fmt.Sprintf("qw-%d", time.Now().UnixNano())
	loginStates.Store(state, &loginCtx{client: nil, expires: time.Now().Add(loginTTL)})
	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerName,
		URL:       "https://qoder.com.cn/settings/personal-access-tokens",
		State:     state,
		ExpiresAt: time.Now().Add(loginTTL).UTC(),
		Metadata: map[string]any{
			"logo":        pluginLogoURL,
			"prompt":      "推荐：打开 QoderWork 面板 → 右上角「导入 QoderWork 凭证」→ 粘贴 PAT。或者：在打开的页面创建一个 PAT (pt- 开头)，粘贴到此处。",
			"input_label": "Personal Access Token (pt-...)",
			"input_key":   "pat",
		},
	})
}

// handlePollLogin implements AuthProvider.PollLogin. For QoderWork this is
// not a real poll — the user pastes a PAT (in req.State we piggy-back the
// state; the pasted PAT comes via req.Code which the host forwards from
// the modal's input field).
func handlePollLogin(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		return nil, fmt.Errorf("poll: empty state")
	}
	v, ok := loginStates.Load(state)
	if !ok {
		return nil, fmt.Errorf("poll: unknown state (restart login) — the login session was lost; please re-initiate login")
	}
	lc := v.(*loginCtx)
	if time.Now().After(lc.expires) {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: login expired (5 min timeout) — please re-initiate login and complete within 5 minutes")
	}

	// The user pastes the PAT into the modal's input field. Host forwards it
	// via Metadata["pat"] (the plugin-declared input key from StartLogin's
	// Metadata; AuthLoginPollRequest has no Code field).
	pat := ""
	if req.Metadata != nil {
		if v, ok := req.Metadata["pat"].(string); ok {
			pat = strings.TrimSpace(v)
		}
	}
	if pat == "" {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待粘贴 PAT（pt-开头）",
		})
	}
	if !strings.HasPrefix(pat, "pt-") {
		return nil, fmt.Errorf("poll: 输入不是有效 PAT (应以 pt- 开头)")
	}

	tok, err := exchangePATForJobToken(pat)
	if err != nil {
		return nil, fmt.Errorf("PAT 换 jobToken 失败: %w", err)
	}
	ui, _ := fetchUserInfo(tok.Token) // best-effort; auth still works without

	sa := buildStoredAuthFromJobToken(pat, tok, ui)
	loginStates.Delete(state)
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth:   toAuthData(sa),
	})
}

// handleRefreshAuth implements AuthProvider.Refresh. QoderWork jobTokens are
// 24h / refresh tokens 48h. Two-tier refresh:
//  1. Try jrt- refresh (normal path, valid for 48h after exchange).
//  2. If jrt- refresh fails, fall back to PAT re-exchange (PAT is long-lived —
//     up to whatever expiry the user set on qoder.com.cn, often "never").
//     This makes accounts survive indefinitely without manual re-import as
//     long as the PAT stays valid.
func handleRefreshAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	// Tier 1: try jrt- refresh.
	tok, err := refreshJobToken(sa.Auth.RefreshToken)
	if err != nil && sa.Auth.PersonalToken != "" {
		// Tier 2: jrt- expired — fall back to PAT re-exchange.
		tok, err = exchangePATForJobToken(sa.Auth.PersonalToken)
	}
	if err != nil {
		return nil, fmt.Errorf("refresh rejected: %w — both jrt- refresh and PAT re-exchange failed; re-import PAT required", err)
	}
	sa.Auth.AccessToken = tok.Token
	if tok.RefreshToken != "" {
		sa.Auth.RefreshToken = tok.RefreshToken
	}
	sa.Auth.ExpiresAt = preserveExpiry(
		time.Now().Add(time.Duration(tok.ExpiresIn)*time.Millisecond).Unix(),
		sa.Auth.ExpiresAt,
	)
	// Host persists the refreshed credential itself after Refresh returns
	// (conductor.go refreshAuth → m.Update → persist). Writing from the
	// plugin too would double-write the file.
	return okEnvelope(pluginapi.AuthRefreshResponse{Auth: toAuthDataForRefresh(sa)})
}

// preserveExpiry reuses the previous token's expiresAt when the refresh
// response omits expiresIn. Zero would tell the host the credential is
// permanently expired and trigger a refresh storm on every request.
func preserveExpiry(newExpiry, oldExpiry int64) int64 {
	if newExpiry > 0 {
		return newExpiry
	}
	return oldExpiry
}

// toAuthDataForRefresh mirrors the workbuddy helper: blank out FileName and
// ID so the host backfills from the original auth path (prevents ID mismatch
// duplicate files when Refresh round-trips the record).
func toAuthDataForRefresh(sa *storedAuth) pluginapi.AuthData {
	ad := toAuthDataOpts(sa, nil, false)
	ad.FileName = "" // let host backfill original
	ad.ID = ""       // let host compute from path (prevents ID mismatch dupes)
	return ad
}
