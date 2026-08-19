package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func managementResponseForTest(t *testing.T, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	t.Helper()
	raw, err := handleManagement(mustJSON(req))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestManagementKeyProtectsReadOnlyStatusRoutes(t *testing.T) {
	managementAPIKeyMu.Lock()
	oldKey := managementAPIKey
	managementAPIKey = "secret"
	managementAPIKeyMu.Unlock()
	t.Cleanup(func() {
		managementAPIKeyMu.Lock()
		managementAPIKey = oldKey
		managementAPIKeyMu.Unlock()
	})

	base := loadedManagementBasePath() + "/plugins/" + providerName
	for _, path := range []string{base + "/accounts", base + "/credits", base + "/keepalive/status"} {
		resp := managementResponseForTest(t, pluginapi.ManagementRequest{Method: http.MethodGet, Path: path})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s status=%d want %d", path, resp.StatusCode, http.StatusUnauthorized)
		}
	}

	headers := http.Header{"Authorization": []string{"Bearer secret"}}
	resp := managementResponseForTest(t, pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Path:    base + "/keepalive/status",
		Headers: headers,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorized status=%d body=%s", resp.StatusCode, resp.Body)
	}
}

func TestManagementPanelRemainsPublicWithKeyConfigured(t *testing.T) {
	managementAPIKeyMu.Lock()
	oldKey := managementAPIKey
	managementAPIKey = "secret"
	managementAPIKeyMu.Unlock()
	t.Cleanup(func() {
		managementAPIKeyMu.Lock()
		managementAPIKey = oldKey
		managementAPIKeyMu.Unlock()
	})

	resp := managementResponseForTest(t, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   loadedResourceBasePath() + "/panel",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("panel status=%d body=%s", resp.StatusCode, resp.Body)
	}
}
