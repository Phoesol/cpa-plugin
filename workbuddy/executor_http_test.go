package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestExecutorHTTPRequestForwardsAllowedUpstream(t *testing.T) {
	oldClient := sharedHTTPClient()
	defer func() { sharedClient = oldClient }()
	var got *http.Request
	sharedClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		got = req
		return &http.Response{
			StatusCode: http.StatusCreated,
			Header:     http.Header{"X-Upstream": []string{"ok"}},
			Body:       io.NopCloser(strings.NewReader("response")),
			Request:    req,
		}, nil
	})}
	storage := mustJSON(&storedAuth{
		Auth:    storedTokens{AccessToken: "access-token"},
		Account: storedAccount{UID: "uid-1"},
	})
	raw, err := handleMethod(pluginabi.MethodExecutorHTTPRequest, mustJSON(pluginapi.ExecutorHTTPRequest{
		Method:      http.MethodPost,
		URL:         upstreamBaseCN + "/v2/plugin/ping",
		Headers:     http.Header{"Content-Type": []string{"application/json"}},
		Body:        []byte(`{"hello":"world"}`),
		StorageJSON: storage,
	}))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("unexpected envelope error: %+v", env.Error)
	}
	var resp pluginapi.ExecutorHTTPResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated || string(resp.Body) != "response" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if got == nil || got.Method != http.MethodPost || got.URL.String() != upstreamBaseCN+"/v2/plugin/ping" {
		t.Fatalf("unexpected forwarded request: %+v", got)
	}
	if got.Header.Get("Authorization") != "Bearer access-token" {
		t.Fatalf("missing derived authorization: %q", got.Header.Get("Authorization"))
	}
	if got.Header.Get("X-Refresh-Token") != "" {
		t.Fatal("refresh token must not be sent on executor HTTP requests")
	}
}

func TestExecutorHTTPRequestRejectsForeignHost(t *testing.T) {
	called := false
	oldClient := sharedHTTPClient()
	defer func() { sharedClient = oldClient }()
	sharedClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		return nil, nil
	})}
	storage := mustJSON(&storedAuth{Auth: storedTokens{AccessToken: "access-token"}})
	_, err := handleMethod(pluginabi.MethodExecutorHTTPRequest, mustJSON(pluginapi.ExecutorHTTPRequest{
		Method:      http.MethodGet,
		URL:         "https://attacker.example.invalid/steal",
		StorageJSON: storage,
	}))
	if err == nil || !strings.Contains(err.Error(), "upstream host") {
		t.Fatalf("expected foreign-host rejection, got %v", err)
	}
	if called {
		t.Fatal("foreign-host request must not reach HTTP client")
	}
}

func TestInboundRPCWrappersPreserveHostCallbackID(t *testing.T) {
	fixtures := []struct {
		name   string
		raw    []byte
		decode func([]byte) string
	}{
		{
			name: "executor.execute",
			raw:  mustJSON(map[string]any{"host_callback_id": "execute-1"}),
			decode: func(raw []byte) string {
				var wire executorRequestWire
				_ = json.Unmarshal(raw, &wire)
				return wire.HostCallbackID
			},
		},
		{
			name: "executor.http_request",
			raw:  mustJSON(map[string]any{"host_callback_id": "http-2"}),
			decode: func(raw []byte) string {
				var wire executorHTTPRequestWire
				_ = json.Unmarshal(raw, &wire)
				return wire.HostCallbackID
			},
		},
		{
			name: "auth.refresh",
			raw:  mustJSON(map[string]any{"host_callback_id": "refresh-3"}),
			decode: func(raw []byte) string {
				var wire authRefreshRequestWire
				_ = json.Unmarshal(raw, &wire)
				return wire.HostCallbackID
			},
		},
		{
			name: "management.handle",
			raw:  mustJSON(map[string]any{"host_callback_id": "management-4"}),
			decode: func(raw []byte) string {
				var wire managementRequestWire
				_ = json.Unmarshal(raw, &wire)
				return wire.HostCallbackID
			},
		},
	}

	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			if got := fixture.decode(fixture.raw); got == "" {
				t.Fatal("host_callback_id was dropped during decode")
			}
		})
	}
}

func TestInboundHandlersForwardHostCallbackID(t *testing.T) {
	tests := []struct {
		file         string
		signature    string
		requirements []string
	}{
		{
			file:      "main.go",
			signature: "func handleExecExecute(",
			requirements: []string{
				"var req executorRequestWire",
				"hostHTTPDoStreamWithCallback(httpReq, req.HostCallbackID)",
			},
		},
		{
			file:      "main.go",
			signature: "func handleExecStream(",
			requirements: []string{
				"collectUpstreamStream(body, sa, sseFramed, collector, req.HostCallbackID)",
				"pumpUpstreamStream(httpReq, cancel, req.StreamID, sseFramed, req.Model, upstreamModel, authUID, started, req.AuthID, req.HostCallbackID)",
			},
		},
		{
			file:      "executor_http.go",
			signature: "func handleExecHTTPRequest(",
			requirements: []string{
				"var req executorHTTPRequestWire",
				"hostHTTPDoWithCallback(httpReq, req.HostCallbackID)",
			},
		},
		{
			file:      "oauth.go",
			signature: "func handleRefreshAuth(",
			requirements: []string{
				"var req authRefreshRequestWire",
				"refreshCallWithCallback(sa, req.HostCallbackID)",
			},
		},
		{
			file:      "management.go",
			signature: "func handleManagement(",
			requirements: []string{
				"var req managementRequestWire",
				"fetchEgressIPWithCallback(req.HostCallbackID)",
				"buildDashboardExWithCallback(true, true, req.HostCallbackID)",
				"handleManualCheckinWithCallback(req.ManagementRequest, req.HostCallbackID)",
				"handleCreditsQueryWithCallback(req.ManagementRequest, req.HostCallbackID)",
				"handleClaimTrialWithCallback(req.ManagementRequest, req.HostCallbackID)",
				"handleKeepaliveNowWithCallback(req.ManagementRequest, req.HostCallbackID)",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.signature, func(t *testing.T) {
			body := handlerSource(t, test.file, test.signature)
			for _, requirement := range test.requirements {
				if !strings.Contains(body, requirement) {
					t.Fatalf("handler does not forward callback through %s", requirement)
				}
			}
		})
	}
}

func handlerSource(t *testing.T, file, signature string) string {
	t.Helper()
	source, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(source), signature)
	if start < 0 {
		t.Fatalf("handler %s not found", signature)
	}
	open := strings.Index(string(source[start:]), "{")
	if open < 0 {
		t.Fatalf("handler %s body not found", signature)
	}
	open += start
	depth := 0
	for end := open; end < len(source); end++ {
		switch source[end] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return string(source[start : end+1])
			}
		}
	}
	t.Fatalf("handler %s body is unclosed", signature)
	return ""
}
