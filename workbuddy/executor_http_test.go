package main

import (
	"encoding/json"
	"io"
	"net/http"
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
