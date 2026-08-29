package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestWBModelsReturnsOnlyAutoDefaultMetadata(t *testing.T) {
	models := wbModels()
	if len(models) != 1 {
		t.Fatalf("len(wbModels()) = %d, want 1", len(models))
	}
	want := pluginapi.ModelInfo{
		ID:                         "auto",
		Name:                       "auto",
		OwnedBy:                    providerName,
		SupportedGenerationMethods: []string{"chat"},
	}
	if !reflect.DeepEqual(models[0], want) {
		t.Fatalf("auto metadata = %#v, want %#v", models[0], want)
	}
}

func TestDefaultModelInfoUsesSourceNameThenID(t *testing.T) {
	if got := defaultModelInfo("serve-alpha", "Alpha"); got.Name != "Alpha" || got.ID != "serve-alpha" {
		t.Fatalf("named default = %#v", got)
	}
	if got := defaultModelInfo("serve-beta", ""); got.Name != "serve-beta" || got.ID != "serve-beta" {
		t.Fatalf("unnamed default = %#v", got)
	}
}

func TestHandleModelForAuthNeverFallsBackToAuto(t *testing.T) {
	raw, err := handleModelForAuth(mustJSON(pluginapi.AuthModelRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var resp pluginapi.ModelResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Provider != providerName || resp.Models == nil || len(resp.Models) != 0 {
		t.Fatalf("failed model response = %#v", resp)
	}
}

func TestHandleModelStaticReturnsOnlyAutoEveryTime(t *testing.T) {
	for i := 0; i < 2; i++ {
		raw, err := handleModelStatic(mustJSON(pluginapi.StaticModelRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatal(err)
		}
		var resp pluginapi.ModelResponse
		if err := json.Unmarshal(env.Result, &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Models) != 1 || resp.Models[0].ID != "auto" {
			t.Fatalf("static model response = %#v", resp)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
