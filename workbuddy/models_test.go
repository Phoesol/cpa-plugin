package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func modelByID(models []pluginapi.ModelInfo, id string) (pluginapi.ModelInfo, bool) {
	for _, m := range models {
		if m.ID == id {
			return m, true
		}
	}
	return pluginapi.ModelInfo{}, false
}

func TestWbModelsIncludesNewFixedModels(t *testing.T) {
	models := wbModels()
	tests := []struct {
		id     string
		name   string
		ctx    int64
		max    int64
		input  []string
		output []string
	}{
		{id: "kimi-k2.6", name: "Kimi K2.6", ctx: 262144, max: 8192, input: []string{"text", "image", "video"}, output: []string{"text"}},
		{id: "kimi-k3-1", name: "Kimi K3", ctx: 1048576, max: 8192, input: []string{"text", "image", "video"}, output: []string{"text"}},
		{id: "glm-5.3", name: "GLM-5.3", ctx: 1000000, max: 131072, input: []string{"text"}, output: []string{"text"}},
		{id: "glm-5.3-flash", name: "GLM-5.3-Flash", ctx: 1048576, max: 131072, input: []string{"text", "image", "video", "file"}, output: []string{"text"}},
		{id: "hy3-x", name: "Hy3 X", ctx: 262144, max: 8192},
		{id: "hy4-preview", name: "Hy4 Preview", ctx: 1048576, max: 8192},
		{id: "hy4-preview-x", name: "Hy4 Preview X", ctx: 1048576, max: 8192},
	}
	for _, tt := range tests {
		m, ok := modelByID(models, tt.id)
		if !ok {
			t.Fatalf("missing model %s", tt.id)
		}
		if m.Name != tt.name || m.ContextLength != tt.ctx || m.MaxCompletionTokens != tt.max {
			t.Fatalf("%s metadata = name %q ctx %d max %d", tt.id, m.Name, m.ContextLength, m.MaxCompletionTokens)
		}
		if !sameStrings(m.SupportedInputModalities, tt.input) {
			t.Fatalf("%s input modalities = %v", tt.id, m.SupportedInputModalities)
		}
		if !sameStrings(m.SupportedOutputModalities, tt.output) {
			t.Fatalf("%s output modalities = %v", tt.id, m.SupportedOutputModalities)
		}
		if !sameStrings(m.SupportedGenerationMethods, []string{"chat"}) {
			t.Fatalf("%s generation methods = %v", tt.id, m.SupportedGenerationMethods)
		}
	}
}

func TestModelForAuthUsesFixedListWithoutNetwork(t *testing.T) {
	called := false
	oldClient := sharedHTTPClient()
	sharedClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"code":0,"data":{"models":[],"agents":[]}}`)),
		}, nil
	})}
	defer func() { sharedClient = oldClient }()

	raw, err := handleModelForAuth(mustJSON(pluginapi.AuthModelRequest{
		StorageJSON: []byte(`{"accessToken":"header.payload.sig"}`),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("model.for_auth called upstream models API")
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var resp pluginapi.ModelResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if _, ok := modelByID(resp.Models, "kimi-k2.6"); !ok {
		t.Fatal("fixed model list not returned")
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
