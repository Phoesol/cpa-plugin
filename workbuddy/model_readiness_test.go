package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	modelRuntimeRawWorkBuddyTransport = "raw-workbuddy-transport-secret"
	modelRuntimeRawWorkBuddyBody      = "raw-workbuddy-response-body-secret"
	modelRuntimeRawMetadataTransport  = "raw-metadata-transport-secret"
	modelRuntimeRawMetadataBody       = "raw-metadata-response-body-secret"
)

func TestModelRuntimeFreshBootstrapReady(t *testing.T) {
	store := newModelStore(t.TempDir())
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		if callbackID != "callback-fresh" {
			t.Fatalf("callback ID = %q", callbackID)
		}
		switch {
		case req.URL.Host == "copilot.tencent.com" && req.URL.Path == "/v3/config":
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`)}, nil
		case req.URL.Host == "models.dev" && req.URL.Path == "/models.json":
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: http.Header{"ETag": []string{`"fresh-etag"`}}, Body: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha","name":"Alpha","limit":{"context":32768,"output":4096}}}`)}, nil
		default:
			t.Fatalf("unexpected model request %s", req.URL)
			return nil, nil
		}
	}
	sa := syntheticStoredAuth(t, workBuddyRealmCN)
	runtime := newModelRuntime(store, do)
	got := runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{
			AuthID:      "auth-fresh",
			StorageJSON: mustJSON(sa),
		},
		HostCallbackID: "callback-fresh",
	})
	if got.State != modelReady || got.ModelSource != modelSourceFresh || got.MetadataSource != modelSourceFresh {
		t.Fatalf("snapshot = %#v", got)
	}
	if len(got.Models) != 1 || got.Models[0].ID != "serve-alpha" || got.Models[0].ContextLength != 32768 {
		t.Fatalf("models = %#v", got.Models)
	}
	identity, err := modelAuthIdentityFor("auth-fresh", sa)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.loadModels(identity.sha256()); err != nil || !found {
		t.Fatalf("model cache found=%v err=%v", found, err)
	}
	if _, found, err := store.loadMetadata(); err != nil || !found {
		t.Fatalf("metadata cache found=%v err=%v", found, err)
	}
}

func TestModelRuntimeFreshBootstrapFailuresFailClosed(t *testing.T) {
	tests := []struct {
		name        string
		invalidAuth bool
		fault       modelRuntimeFreshFault
		wantCode    modelErrorCode
		do          func(*testing.T, string, modelRuntimeFreshFault) modelHTTPDo
	}{
		{name: "invalid auth", invalidAuth: true, wantCode: modelErrorAuthInvalid, do: modelRuntimeFreshFaultDo},
		{name: "WorkBuddy transport", fault: modelRuntimeFaultWorkBuddyTransport, wantCode: modelErrorWorkBuddyTransport, do: modelRuntimeFreshFaultDo},
		{name: "WorkBuddy HTTP", fault: modelRuntimeFaultWorkBuddyHTTP, wantCode: modelErrorWorkBuddyHTTP, do: modelRuntimeFreshFaultDo},
		{name: "WorkBuddy schema", fault: modelRuntimeFaultWorkBuddySchema, wantCode: modelErrorWorkBuddySchema, do: modelRuntimeFreshFaultDo},
		{name: "WorkBuddy save", fault: modelRuntimeFaultWorkBuddySave, wantCode: modelErrorCacheWrite, do: modelRuntimeFreshFaultDo},
		{name: "models.dev transport", fault: modelRuntimeFaultMetadataTransport, wantCode: modelErrorModelsDevTransport, do: modelRuntimeFreshFaultDo},
		{name: "models.dev HTTP", fault: modelRuntimeFaultMetadataHTTP, wantCode: modelErrorModelsDevHTTP, do: modelRuntimeFreshFaultDo},
		{name: "models.dev schema", fault: modelRuntimeFaultMetadataSchema, wantCode: modelErrorModelsDevSchema, do: modelRuntimeFreshFaultDo},
		{name: "metadata save", fault: modelRuntimeFaultMetadataSave, wantCode: modelErrorCacheWrite, do: modelRuntimeFreshFaultDo},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "model-store")
			store := newModelStore(root)
			sa := syntheticStoredAuth(t, workBuddyRealmCN)
			storageJSON := mustJSON(sa)
			if tt.invalidAuth {
				storageJSON = []byte(`{"auth":{"accessToken":""},"raw":"raw-invalid-auth-body-secret"}`)
			}
			runtime := newModelRuntime(store, tt.do(t, root, tt.fault))
			got := runtime.ensureForAuth(authModelRequestWire{
				AuthModelRequest: pluginapi.AuthModelRequest{
					AuthID:      "auth-failure",
					StorageJSON: storageJSON,
				},
				HostCallbackID: "callback-failure",
			})
			if got.State != modelFailed || got.executable() || got.Models == nil || len(got.Models) != 0 {
				t.Fatalf("snapshot = %#v", got)
			}
			if got.ErrorCode != tt.wantCode {
				t.Fatalf("error code = %q, want %q; snapshot = %#v", got.ErrorCode, tt.wantCode, got)
			}
			assertModelRuntimeSnapshotRedacted(t, got, sa.Auth.AccessToken)
		})
	}
}

func TestModelRuntimeFreshBootstrapRetainsValidModelCache(t *testing.T) {
	root := t.TempDir()
	store := newModelStore(root)
	sa := syntheticStoredAuth(t, workBuddyRealmCN)
	identity, err := modelAuthIdentityFor("auth-cached", sa)
	if err != nil {
		t.Fatal(err)
	}
	cached := modelStoreTestCatalog(identity.sha256(), "cached")
	if err := store.saveModels(cached); err != nil {
		t.Fatal(err)
	}
	if err := store.saveMetadata(modelStoreTestMetadata("cached")); err != nil {
		t.Fatal(err)
	}

	runtime := newModelRuntime(store, modelRuntimeFreshFaultDo(t, root, modelRuntimeFaultWorkBuddyTransport))
	got := runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-cached", StorageJSON: mustJSON(sa)},
		HostCallbackID:   "callback-failure",
	})
	if got.ModelSource != modelSourceCache || !got.ModelsFetchedAt.Equal(cached.FetchedAt) {
		t.Fatalf("valid model cache was not retained: %#v", got)
	}
	if got.ErrorCode != modelErrorWorkBuddyTransport {
		t.Fatalf("error code = %q, want %q", got.ErrorCode, modelErrorWorkBuddyTransport)
	}
}

func TestModelRuntimeFreshBootstrapModelFutureSchemaIsCacheRead(t *testing.T) {
	root := t.TempDir()
	store := newModelStore(root)
	sa := syntheticStoredAuth(t, workBuddyRealmCN)
	identity, err := modelAuthIdentityFor("auth-future-models", sa)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "models", identity.sha256()+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	future := []byte(`{"schema_version":2}`)
	if err := os.WriteFile(path, future, 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	do := modelRuntimeFreshFaultDo(t, root, "")
	runtime := newModelRuntime(store, func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		if req.URL.Host == "copilot.tencent.com" {
			calls++
		}
		return do(req, callbackID)
	})

	got := runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-future-models", StorageJSON: mustJSON(sa)},
		HostCallbackID:   "callback-failure",
	})
	if calls != 1 {
		t.Fatalf("WorkBuddy refresh calls = %d, want 1", calls)
	}
	if got.State != modelFailed || got.ErrorCode != modelErrorCacheRead {
		t.Fatalf("snapshot = %#v", got)
	}
	if got.ModelSource != modelSourceNone {
		t.Fatalf("future model cache source = %q, want none", got.ModelSource)
	}
	if gotFile := modelStoreReadFile(t, path); string(gotFile) != string(future) {
		t.Fatalf("future model cache was overwritten: %s", gotFile)
	}
}

func TestModelRuntimeFreshBootstrapMetadataFutureSchemaIsCacheRead(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "metadata.json")
	future := []byte(`{"schema_version":2}`)
	if err := os.WriteFile(path, future, 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	do := modelRuntimeFreshFaultDo(t, root, "")
	runtime := newModelRuntime(newModelStore(root), func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		if req.URL.Host == "models.dev" {
			calls++
		}
		return do(req, callbackID)
	})

	sa := syntheticStoredAuth(t, workBuddyRealmCN)
	got := runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-future-metadata", StorageJSON: mustJSON(sa)},
		HostCallbackID:   "callback-failure",
	})
	if calls != 1 {
		t.Fatalf("models.dev refresh calls = %d, want 1", calls)
	}
	if got.State != modelFailed || got.ErrorCode != modelErrorCacheRead {
		t.Fatalf("snapshot = %#v", got)
	}
	if got.MetadataSource != modelSourceNone {
		t.Fatalf("future metadata cache source = %q, want none", got.MetadataSource)
	}
	if gotFile := modelStoreReadFile(t, path); string(gotFile) != string(future) {
		t.Fatalf("future metadata cache was overwritten: %s", gotFile)
	}
}

func TestModelRuntimeFreshBootstrapSnapshotsAreReadOnly(t *testing.T) {
	runtime := &modelRuntime{snapshots: map[string]modelReadinessSnapshot{
		"auth-copy": {
			State: modelReady,
			Models: []pluginapi.ModelInfo{
				{
					ID:                         "serve-alpha",
					SupportedGenerationMethods: []string{"chat"},
					SupportedParameters:        []string{"temperature"},
					SupportedInputModalities:   []string{"text"},
					SupportedOutputModalities:  []string{"text"},
					Thinking:                   &pluginapi.ThinkingSupport{Levels: []string{"low"}},
				},
			},
		},
	}}

	first := runtime.snapshotForAuthID("auth-copy")
	first.Models[0].SupportedGenerationMethods[0] = "changed"
	first.Models[0].SupportedParameters[0] = "changed"
	first.Models[0].SupportedInputModalities[0] = "changed"
	first.Models[0].SupportedOutputModalities[0] = "changed"
	first.Models[0].Thinking.Levels[0] = "changed"

	second := runtime.snapshotForAuthID("auth-copy")
	model := second.Models[0]
	if model.SupportedGenerationMethods[0] != "chat" || model.SupportedParameters[0] != "temperature" || model.SupportedInputModalities[0] != "text" || model.SupportedOutputModalities[0] != "text" || model.Thinking.Levels[0] != "low" {
		t.Fatalf("published snapshot was mutated: %#v", second)
	}

	if got := runtime.advanceConfigGeneration(); got != 1 {
		t.Fatalf("config generation = %d, want 1", got)
	}
	runtime.markAuthNotStarted("auth-copy")
	marked := runtime.snapshotForAuthID("auth-copy")
	if marked.State != modelNotStarted || marked.executable() || marked.Models == nil || len(marked.Models) != 0 {
		t.Fatalf("marked snapshot = %#v", marked)
	}
}

func TestModelRuntimeFreshBootstrapPreloadsMetadataWithoutSettlingRefresh(t *testing.T) {
	store := newModelStore(t.TempDir())
	cached := modelStoreTestMetadata("cached")
	if err := store.saveMetadata(cached); err != nil {
		t.Fatal(err)
	}
	runtime := newModelRuntime(store, func(*http.Request, string) (*hostHTTPResponse, error) {
		t.Fatal("metadata preload made an HTTP request")
		return nil, nil
	})
	status := runtime.metadataStatus()
	if status.Source != modelSourceCache || !status.FetchedAt.Equal(cached.FetchedAt) || status.ErrorCode != modelErrorNone {
		t.Fatalf("metadata status = %#v", status)
	}
	if runtime.metadataResult != nil {
		t.Fatalf("metadata preload settled refresh: %#v", runtime.metadataResult)
	}
}

func TestModelRuntimeFreshBootstrapCurrentRuntimeIsLazySingleton(t *testing.T) {
	previous := activeModelRuntime.Swap(nil)
	t.Cleanup(func() { activeModelRuntime.Swap(previous) })
	configHome := t.TempDir()
	t.Setenv("APPDATA", configHome)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("HOME", configHome)

	first := currentModelRuntime()
	second := currentModelRuntime()
	if first == nil || second != first || activeModelRuntime.Load() != first {
		t.Fatalf("runtime singleton: first=%p second=%p active=%p", first, second, activeModelRuntime.Load())
	}
}

type modelRuntimeFreshFault string

const (
	modelRuntimeFaultWorkBuddyTransport modelRuntimeFreshFault = "workbuddy_transport"
	modelRuntimeFaultWorkBuddyHTTP      modelRuntimeFreshFault = "workbuddy_http"
	modelRuntimeFaultWorkBuddySchema    modelRuntimeFreshFault = "workbuddy_schema"
	modelRuntimeFaultWorkBuddySave      modelRuntimeFreshFault = "workbuddy_save"
	modelRuntimeFaultMetadataTransport  modelRuntimeFreshFault = "metadata_transport"
	modelRuntimeFaultMetadataHTTP       modelRuntimeFreshFault = "metadata_http"
	modelRuntimeFaultMetadataSchema     modelRuntimeFreshFault = "metadata_schema"
	modelRuntimeFaultMetadataSave       modelRuntimeFreshFault = "metadata_save"
)

func modelRuntimeFreshFaultDo(t *testing.T, root string, fault modelRuntimeFreshFault) modelHTTPDo {
	t.Helper()
	return func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		if callbackID != "callback-failure" {
			t.Fatalf("callback ID = %q", callbackID)
		}
		switch {
		case req.URL.Host == "copilot.tencent.com" && req.URL.Path == "/v3/config":
			switch fault {
			case modelRuntimeFaultWorkBuddyTransport:
				return nil, errors.New(modelRuntimeRawWorkBuddyTransport)
			case modelRuntimeFaultWorkBuddyHTTP:
				return &hostHTTPResponse{StatusCode: http.StatusServiceUnavailable, Headers: make(http.Header), Body: []byte(modelRuntimeRawWorkBuddyBody)}, nil
			case modelRuntimeFaultWorkBuddySchema:
				return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(modelRuntimeRawWorkBuddyBody)}, nil
			case modelRuntimeFaultWorkBuddySave:
				modelRuntimeReplaceStoreRootWithFile(t, root)
			}
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}]}}`)}, nil
		case req.URL.Host == "models.dev" && req.URL.Path == "/models.json":
			switch fault {
			case modelRuntimeFaultMetadataTransport:
				return nil, errors.New(modelRuntimeRawMetadataTransport)
			case modelRuntimeFaultMetadataHTTP:
				return &hostHTTPResponse{StatusCode: http.StatusBadGateway, Headers: make(http.Header), Body: []byte(modelRuntimeRawMetadataBody)}, nil
			case modelRuntimeFaultMetadataSchema:
				return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(modelRuntimeRawMetadataBody)}, nil
			case modelRuntimeFaultMetadataSave:
				modelRuntimeReplaceStoreRootWithFile(t, root)
			}
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: http.Header{"ETag": []string{`"failure-etag"`}}, Body: []byte(`{"vendor/serve-alpha":{"id":"serve-alpha","name":"Alpha"}}`)}, nil
		default:
			t.Fatalf("unexpected model request %s", req.URL)
			return nil, nil
		}
	}
}

func modelRuntimeReplaceStoreRootWithFile(t *testing.T, root string) {
	t.Helper()
	if err := os.Rename(root, root+"-saved"); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("regular-file-store-root"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertModelRuntimeSnapshotRedacted(t *testing.T, got modelReadinessSnapshot, accessToken string) {
	t.Helper()
	rendered := fmt.Sprintf("%#v", got)
	for _, forbidden := range []string{
		accessToken,
		modelRuntimeRawWorkBuddyTransport,
		modelRuntimeRawWorkBuddyBody,
		modelRuntimeRawMetadataTransport,
		modelRuntimeRawMetadataBody,
		"raw-invalid-auth-body-secret",
		"https://",
		"copilot.tencent.com",
		"models.dev/models.json",
	} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("snapshot contains raw detail %q: %s", forbidden, rendered)
		}
	}
}
