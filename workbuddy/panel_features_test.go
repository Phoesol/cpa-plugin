package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestManagementReturnsEffectiveDesensitizeSettings(t *testing.T) {
	old := featureRuntime.Load()
	cfg, err := parseFeatureRuntime([]byte("desensitize: true\ndesensitize_terms: [Codex]\n"))
	if err != nil {
		t.Fatal(err)
	}
	featureRuntime.Store(cfg)
	t.Cleanup(func() { featureRuntime.Store(old) })

	path := loadedManagementBasePath() + "/plugins/workbuddy/desensitize"
	resp := managementResponseForTest(t, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   path,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", resp.StatusCode, resp.Body)
	}
	var got struct {
		Enabled bool     `json:"enabled"`
		Terms   []string `json:"terms"`
		Source  string   `json:"source"`
	}
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.Source != "custom" || !reflect.DeepEqual(got.Terms, []string{"Codex"}) {
		t.Fatalf("effective settings = %#v", got)
	}

	if resp := managementResponseForTest(t, pluginapi.ManagementRequest{Method: http.MethodPost, Path: path}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST status=%d body=%s", resp.StatusCode, resp.Body)
	}
}

func TestPanelEditsDesensitizeThroughGenericPluginConfigAPI(t *testing.T) {
	html := string(panelHTML)
	for _, want := range []string{
		`id="desensitizeModal"`,
		`id="desensitizeEnabled"`,
		`id="desensitizeTerms"`,
		`id="desensitizeSource"`,
		`async function managementAPI(path, opts={})`,
		`fetch("/v0/management"+path`,
		`api("/desensitize")`,
		`managementAPI("/plugins/workbuddy/config",{method:"PATCH"`,
		`JSON.stringify({desensitize_terms:null})`,
		`JSON.stringify({desensitize:false,desensitize_terms:null})`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("panel missing %q", want)
		}
	}
}
