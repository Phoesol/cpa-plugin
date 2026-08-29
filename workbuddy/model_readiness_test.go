package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestModelRuntimeStaleMatrix(t *testing.T) {
	tests := []struct {
		name                   string
		workBuddyFails         bool
		metadataFails          bool
		metadataNotModified    bool
		wantState              modelReadinessState
		wantModelSource        modelSnapshotSource
		wantMetadataSource     modelSnapshotSource
		wantID                 string
		wantName               string
		wantContext            int64
		wantCode               modelErrorCode
		wantCachedModelTime    bool
		wantCachedMetadataTime bool
	}{
		{
			name:               "fresh models and fresh metadata",
			wantState:          modelReady,
			wantModelSource:    modelSourceFresh,
			wantMetadataSource: modelSourceFresh,
			wantID:             "fresh-model",
			wantName:           "Fresh metadata for fresh",
			wantContext:        2222,
		},
		{
			name:                "cached models and fresh metadata",
			workBuddyFails:      true,
			wantState:           modelStale,
			wantModelSource:     modelSourceCache,
			wantMetadataSource:  modelSourceFresh,
			wantID:              "cached-model",
			wantName:            "Fresh metadata for cached",
			wantContext:         2222,
			wantCode:            modelErrorWorkBuddyTransport,
			wantCachedModelTime: true,
		},
		{
			name:                   "fresh models and cached metadata",
			metadataFails:          true,
			wantState:              modelStale,
			wantModelSource:        modelSourceFresh,
			wantMetadataSource:     modelSourceCache,
			wantID:                 "fresh-model",
			wantName:               "Cached metadata for fresh",
			wantContext:            1111,
			wantCode:               modelErrorModelsDevTransport,
			wantCachedMetadataTime: true,
		},
		{
			name:                   "cached models and cached metadata",
			workBuddyFails:         true,
			metadataFails:          true,
			wantState:              modelStale,
			wantModelSource:        modelSourceCache,
			wantMetadataSource:     modelSourceCache,
			wantID:                 "cached-model",
			wantName:               "Cached metadata for cached",
			wantContext:            1111,
			wantCode:               modelErrorWorkBuddyTransport,
			wantCachedModelTime:    true,
			wantCachedMetadataTime: true,
		},
		{
			name:                   "fresh models and not modified metadata",
			metadataNotModified:    true,
			wantState:              modelReady,
			wantModelSource:        modelSourceFresh,
			wantMetadataSource:     modelSourceFresh,
			wantID:                 "fresh-model",
			wantName:               "Cached metadata for fresh",
			wantContext:            1111,
			wantCachedMetadataTime: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			sa := syntheticStoredAuth(t, workBuddyRealmCN)
			lastGood := modelRuntimeSeedLastGood(t, root, "auth-stale", sa)
			workBuddyCalls := 0
			metadataCalls := 0
			do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
				if callbackID != "callback-stale" {
					t.Fatalf("callback ID = %q", callbackID)
				}
				switch {
				case req.URL.Host == "copilot.tencent.com" && req.URL.Path == "/v3/config":
					workBuddyCalls++
					if tt.workBuddyFails {
						return nil, errors.New(modelRuntimeRawWorkBuddyTransport)
					}
					return modelRuntimeFreshWorkBuddyResponse(), nil
				case req.URL.Host == "models.dev" && req.URL.Path == "/models.json":
					metadataCalls++
					if got := req.Header.Get("If-None-Match"); got != lastGood.metadata.ETag {
						t.Fatalf("If-None-Match = %q, want %q", got, lastGood.metadata.ETag)
					}
					if tt.metadataFails {
						return nil, errors.New(modelRuntimeRawMetadataTransport)
					}
					if tt.metadataNotModified {
						return &hostHTTPResponse{StatusCode: http.StatusNotModified, Headers: http.Header{"ETag": []string{`"ignored-etag"`}}}, nil
					}
					return modelRuntimeFreshMetadataResponse(), nil
				default:
					t.Fatalf("unexpected model request %s", req.URL)
					return nil, nil
				}
			}

			runtime := newModelRuntime(newModelStore(root), do)
			got := runtime.ensureForAuth(authModelRequestWire{
				AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-stale", StorageJSON: mustJSON(sa)},
				HostCallbackID:   "callback-stale",
			})
			if workBuddyCalls != 1 || metadataCalls != 1 {
				t.Fatalf("refresh calls: workbuddy=%d metadata=%d, want 1 each", workBuddyCalls, metadataCalls)
			}
			if got.State != tt.wantState || got.ModelSource != tt.wantModelSource || got.MetadataSource != tt.wantMetadataSource {
				t.Fatalf("snapshot = %#v", got)
			}
			if !got.executable() {
				t.Fatalf("state %q did not allow execution", got.State)
			}
			if got.ErrorCode != tt.wantCode {
				t.Fatalf("error code = %q, want %q", got.ErrorCode, tt.wantCode)
			}
			if len(got.Models) != 1 || got.Models[0].ID != tt.wantID || got.Models[0].Name != tt.wantName || got.Models[0].ContextLength != tt.wantContext {
				t.Fatalf("models = %#v", got.Models)
			}
			if got.ModelsFetchedAt.Equal(lastGood.catalog.FetchedAt) != tt.wantCachedModelTime {
				t.Fatalf("models fetched_at = %s, cached = %s", got.ModelsFetchedAt, lastGood.catalog.FetchedAt)
			}
			if got.MetadataFetchedAt.Equal(lastGood.metadata.FetchedAt) != tt.wantCachedMetadataTime {
				t.Fatalf("metadata fetched_at = %s, cached = %s", got.MetadataFetchedAt, lastGood.metadata.FetchedAt)
			}
		})
	}
}

func TestModelRuntimeStalePersistenceFailuresRetainOldPrimary(t *testing.T) {
	tests := []struct {
		name               string
		blockedSource      string
		wantModelSource    modelSnapshotSource
		wantMetadataSource modelSnapshotSource
		wantID             string
		wantName           string
		wantContext        int64
	}{
		{
			name:               "model catalog save",
			blockedSource:      "models",
			wantModelSource:    modelSourceCache,
			wantMetadataSource: modelSourceFresh,
			wantID:             "cached-model",
			wantName:           "Fresh metadata for cached",
			wantContext:        2222,
		},
		{
			name:               "metadata save",
			blockedSource:      "metadata",
			wantModelSource:    modelSourceFresh,
			wantMetadataSource: modelSourceCache,
			wantID:             "fresh-model",
			wantName:           "Cached metadata for fresh",
			wantContext:        1111,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			sa := syntheticStoredAuth(t, workBuddyRealmCN)
			lastGood := modelRuntimeSeedLastGood(t, root, "auth-save", sa)
			blockedPath := lastGood.modelPath
			if tt.blockedSource == "metadata" {
				blockedPath = lastGood.metadataPath
			}
			before := modelStoreReadFile(t, blockedPath)
			futureBackup := []byte(`{"schema_version":2}`)
			if err := os.WriteFile(blockedPath+".bak", futureBackup, 0o600); err != nil {
				t.Fatal(err)
			}

			runtime := newModelRuntime(newModelStore(root), modelRuntimeSuccessfulRefreshDo(t, "callback-save"))
			got := runtime.ensureForAuth(authModelRequestWire{
				AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-save", StorageJSON: mustJSON(sa)},
				HostCallbackID:   "callback-save",
			})
			if got.State != modelStale || !got.executable() || got.ModelSource != tt.wantModelSource || got.MetadataSource != tt.wantMetadataSource || got.ErrorCode != modelErrorCacheWrite {
				t.Fatalf("snapshot = %#v", got)
			}
			if len(got.Models) != 1 || got.Models[0].ID != tt.wantID || got.Models[0].Name != tt.wantName || got.Models[0].ContextLength != tt.wantContext {
				t.Fatalf("models = %#v", got.Models)
			}
			if after := modelStoreReadFile(t, blockedPath); string(after) != string(before) {
				t.Fatalf("old primary was replaced after failed save: before=%s after=%s", before, after)
			}
			if after := modelStoreReadFile(t, blockedPath+".bak"); string(after) != string(futureBackup) {
				t.Fatalf("future backup was replaced after failed save: %s", after)
			}
		})
	}
}

func TestModelRuntimeStaleRejectsPartialAndCorruptRefreshes(t *testing.T) {
	tests := []struct {
		name               string
		failedSource       string
		body               string
		wantModelSource    modelSnapshotSource
		wantMetadataSource modelSnapshotSource
		wantID             string
		wantName           string
		wantContext        int64
		wantCode           modelErrorCode
	}{
		{
			name:               "partial WorkBuddy body",
			failedSource:       "models",
			body:               `{"code":0,"data":{"agents":[{"name":"cli","models":["fresh-model",""]}]}}`,
			wantModelSource:    modelSourceCache,
			wantMetadataSource: modelSourceFresh,
			wantID:             "cached-model",
			wantName:           "Fresh metadata for cached",
			wantContext:        2222,
			wantCode:           modelErrorWorkBuddySchema,
		},
		{
			name:               "corrupt WorkBuddy body",
			failedSource:       "models",
			body:               `{"code":`,
			wantModelSource:    modelSourceCache,
			wantMetadataSource: modelSourceFresh,
			wantID:             "cached-model",
			wantName:           "Fresh metadata for cached",
			wantContext:        2222,
			wantCode:           modelErrorWorkBuddySchema,
		},
		{
			name:               "partial metadata body",
			failedSource:       "metadata",
			body:               `{"fresh-provider/fresh-model":{"id":"fresh-model","name":"Fresh metadata for fresh","limit":{"context":2222}},"fresh-provider/broken":{"id":""}}`,
			wantModelSource:    modelSourceFresh,
			wantMetadataSource: modelSourceCache,
			wantID:             "fresh-model",
			wantName:           "Cached metadata for fresh",
			wantContext:        1111,
			wantCode:           modelErrorModelsDevSchema,
		},
		{
			name:               "corrupt metadata body",
			failedSource:       "metadata",
			body:               `{"fresh-provider/fresh-model":`,
			wantModelSource:    modelSourceFresh,
			wantMetadataSource: modelSourceCache,
			wantID:             "fresh-model",
			wantName:           "Cached metadata for fresh",
			wantContext:        1111,
			wantCode:           modelErrorModelsDevSchema,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			sa := syntheticStoredAuth(t, workBuddyRealmCN)
			lastGood := modelRuntimeSeedLastGood(t, root, "auth-invalid-refresh", sa)
			failedPath := lastGood.modelPath
			if tt.failedSource == "metadata" {
				failedPath = lastGood.metadataPath
			}
			before := modelStoreReadFile(t, failedPath)
			do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
				if callbackID != "callback-invalid-refresh" {
					t.Fatalf("callback ID = %q", callbackID)
				}
				switch req.URL.Host {
				case "copilot.tencent.com":
					if tt.failedSource == "models" {
						return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(tt.body)}, nil
					}
					return modelRuntimeFreshWorkBuddyResponse(), nil
				case "models.dev":
					if tt.failedSource == "metadata" {
						return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(tt.body)}, nil
					}
					return modelRuntimeFreshMetadataResponse(), nil
				default:
					t.Fatalf("unexpected model request %s", req.URL)
					return nil, nil
				}
			}

			runtime := newModelRuntime(newModelStore(root), do)
			got := runtime.ensureForAuth(authModelRequestWire{
				AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-invalid-refresh", StorageJSON: mustJSON(sa)},
				HostCallbackID:   "callback-invalid-refresh",
			})
			if got.State != modelStale || !got.executable() || got.ModelSource != tt.wantModelSource || got.MetadataSource != tt.wantMetadataSource || got.ErrorCode != tt.wantCode {
				t.Fatalf("snapshot = %#v", got)
			}
			if len(got.Models) != 1 || got.Models[0].ID != tt.wantID || got.Models[0].Name != tt.wantName || got.Models[0].ContextLength != tt.wantContext {
				t.Fatalf("models = %#v", got.Models)
			}
			if after := modelStoreReadFile(t, failedPath); string(after) != string(before) {
				t.Fatalf("last-good primary was replaced: before=%s after=%s", before, after)
			}
		})
	}
}

func TestModelRuntimeNotModifiedKeepsMetadataCacheWithoutRewrite(t *testing.T) {
	root := t.TempDir()
	sa := syntheticStoredAuth(t, workBuddyRealmCN)
	lastGood := modelRuntimeSeedLastGood(t, root, "auth-not-modified", sa)
	before := modelStoreReadFile(t, lastGood.metadataPath)
	metadataCalls := 0
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		if callbackID != "callback-not-modified" {
			t.Fatalf("callback ID = %q", callbackID)
		}
		switch req.URL.Host {
		case "copilot.tencent.com":
			return modelRuntimeFreshWorkBuddyResponse(), nil
		case "models.dev":
			metadataCalls++
			if got := req.Header.Get("If-None-Match"); got != lastGood.metadata.ETag {
				t.Fatalf("If-None-Match = %q, want %q", got, lastGood.metadata.ETag)
			}
			return &hostHTTPResponse{StatusCode: http.StatusNotModified, Headers: http.Header{"ETag": []string{`"replacement-etag"`}}}, nil
		default:
			t.Fatalf("unexpected model request %s", req.URL)
			return nil, nil
		}
	}

	runtime := newModelRuntime(newModelStore(root), do)
	got := runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-not-modified", StorageJSON: mustJSON(sa)},
		HostCallbackID:   "callback-not-modified",
	})
	if metadataCalls != 1 {
		t.Fatalf("metadata calls = %d, want 1", metadataCalls)
	}
	if got.State != modelReady || got.ModelSource != modelSourceFresh || got.MetadataSource != modelSourceFresh || got.ErrorCode != modelErrorNone {
		t.Fatalf("snapshot = %#v", got)
	}
	if len(got.Models) != 1 || got.Models[0].ID != "fresh-model" || got.Models[0].Name != "Cached metadata for fresh" || got.Models[0].ContextLength != 1111 {
		t.Fatalf("models = %#v", got.Models)
	}
	if !got.MetadataFetchedAt.Equal(lastGood.metadata.FetchedAt) {
		t.Fatalf("metadata fetched_at = %s, want %s", got.MetadataFetchedAt, lastGood.metadata.FetchedAt)
	}
	if runtime.metadataResult == nil || runtime.metadataResult.cache.ETag != lastGood.metadata.ETag || !runtime.metadataResult.cache.FetchedAt.Equal(lastGood.metadata.FetchedAt) {
		t.Fatalf("metadata result = %#v", runtime.metadataResult)
	}
	if after := modelStoreReadFile(t, lastGood.metadataPath); string(after) != string(before) {
		t.Fatalf("metadata primary was rewritten: before=%s after=%s", before, after)
	}
	if _, err := os.Stat(lastGood.metadataPath + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata backup exists after 304: %v", err)
	}
}

func TestModelRuntimeNotModifiedWithoutMetadataCacheFailsAndRetries(t *testing.T) {
	metadataCalls := 0
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		if callbackID != "callback-not-modified-no-cache" {
			t.Fatalf("callback ID = %q", callbackID)
		}
		switch req.URL.Host {
		case "copilot.tencent.com":
			return modelRuntimeFreshWorkBuddyResponse(), nil
		case "models.dev":
			metadataCalls++
			if metadataCalls == 1 {
				return &hostHTTPResponse{StatusCode: http.StatusNotModified, Headers: make(http.Header)}, nil
			}
			return modelRuntimeFreshMetadataResponse(), nil
		default:
			t.Fatalf("unexpected model request %s", req.URL)
			return nil, nil
		}
	}

	runtime := newModelRuntime(newModelStore(t.TempDir()), do)
	sa := syntheticStoredAuth(t, workBuddyRealmCN)
	first := runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-not-modified-first", StorageJSON: mustJSON(sa)},
		HostCallbackID:   "callback-not-modified-no-cache",
	})
	if first.State != modelFailed || first.ModelSource != modelSourceFresh || first.MetadataSource != modelSourceNone || first.ErrorCode != modelErrorModelsDevSchema || first.executable() || first.Models == nil || len(first.Models) != 0 {
		t.Fatalf("first snapshot = %#v", first)
	}
	if runtime.metadataResult != nil {
		t.Fatalf("304 without cache settled the runtime: %#v", runtime.metadataResult)
	}

	second := runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-not-modified-second", StorageJSON: mustJSON(sa)},
		HostCallbackID:   "callback-not-modified-no-cache",
	})
	if metadataCalls != 2 {
		t.Fatalf("metadata calls = %d, want 2", metadataCalls)
	}
	if second.State != modelReady || second.ModelSource != modelSourceFresh || second.MetadataSource != modelSourceFresh || !second.executable() {
		t.Fatalf("second snapshot = %#v", second)
	}
}

func TestModelRuntimeRetriesMetadataWithoutCache(t *testing.T) {
	metadataCalls := 0
	do := func(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
		if callbackID != "callback-retry" {
			t.Fatalf("callback ID = %q", callbackID)
		}
		switch req.URL.Host {
		case "copilot.tencent.com":
			return modelRuntimeFreshWorkBuddyResponse(), nil
		case "models.dev":
			metadataCalls++
			if metadataCalls == 1 {
				return nil, errors.New(modelRuntimeRawMetadataTransport)
			}
			return modelRuntimeFreshMetadataResponse(), nil
		default:
			t.Fatalf("unexpected model request %s", req.URL)
			return nil, nil
		}
	}

	runtime := newModelRuntime(newModelStore(t.TempDir()), do)
	sa := syntheticStoredAuth(t, workBuddyRealmCN)
	first := runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-retry-first", StorageJSON: mustJSON(sa)},
		HostCallbackID:   "callback-retry",
	})
	if first.State != modelFailed || first.MetadataSource != modelSourceNone || first.ErrorCode != modelErrorModelsDevTransport || first.executable() {
		t.Fatalf("first snapshot = %#v", first)
	}
	if runtime.metadataResult != nil {
		t.Fatalf("metadata failure without cache settled the runtime: %#v", runtime.metadataResult)
	}

	second := runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-retry-second", StorageJSON: mustJSON(sa)},
		HostCallbackID:   "callback-retry",
	})
	if metadataCalls != 2 {
		t.Fatalf("metadata calls = %d, want 2", metadataCalls)
	}
	if second.State != modelReady || second.ModelSource != modelSourceFresh || second.MetadataSource != modelSourceFresh || !second.executable() {
		t.Fatalf("second snapshot = %#v", second)
	}
	if len(second.Models) != 1 || second.Models[0].ID != "fresh-model" || second.Models[0].ContextLength != 2222 {
		t.Fatalf("second models = %#v", second.Models)
	}
}

type modelRuntimeLastGood struct {
	catalog      modelCatalogCacheV1
	metadata     metadataCacheV1
	modelPath    string
	metadataPath string
}

func modelRuntimeSeedLastGood(t *testing.T, root, authID string, sa *storedAuth) modelRuntimeLastGood {
	t.Helper()
	identity, err := modelAuthIdentityFor(authID, sa)
	if err != nil {
		t.Fatal(err)
	}
	cachedContext := int64(1111)
	catalog := modelCatalogCacheV1{
		SchemaVersion:  1,
		IdentitySHA256: identity.sha256(),
		Realm:          workBuddyRealmCN,
		FetchedAt:      time.Date(2026, time.August, 28, 1, 2, 3, 0, time.UTC),
		Endpoint:       workBuddyEndpointV3Config,
		Models:         []modelFacts{{ID: "cached-model"}},
	}
	metadata := metadataCacheV1{
		SchemaVersion: 1,
		ETag:          `W/"cached-etag"`,
		FetchedAt:     time.Date(2026, time.August, 28, 4, 5, 6, 0, time.UTC),
		Records: map[string]modelFacts{
			"cached-provider/cached-model": {
				ID:            "cached-provider/cached-model",
				Name:          "Cached metadata for cached",
				ContextLength: &cachedContext,
			},
			"cached-provider/fresh-model": {
				ID:            "cached-provider/fresh-model",
				Name:          "Cached metadata for fresh",
				ContextLength: &cachedContext,
			},
		},
	}
	store := newModelStore(root)
	if err := store.saveModels(catalog); err != nil {
		t.Fatal(err)
	}
	if err := store.saveMetadata(metadata); err != nil {
		t.Fatal(err)
	}
	return modelRuntimeLastGood{
		catalog:      catalog,
		metadata:     metadata,
		modelPath:    filepath.Join(root, "models", identity.sha256()+".json"),
		metadataPath: filepath.Join(root, "metadata.json"),
	}
}

func modelRuntimeFreshWorkBuddyResponse() *hostHTTPResponse {
	return &hostHTTPResponse{
		StatusCode: http.StatusOK,
		Headers:    make(http.Header),
		Body:       []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["fresh-model"]}]}}`),
	}
}

func modelRuntimeFreshMetadataResponse() *hostHTTPResponse {
	return &hostHTTPResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"ETag": []string{`"fresh-etag"`}},
		Body: []byte(`{"fresh-provider/cached-model":{"id":"cached-model","name":"Fresh metadata for cached","limit":{"context":2222}},` +
			`"fresh-provider/fresh-model":{"id":"fresh-model","name":"Fresh metadata for fresh","limit":{"context":2222}}}`),
	}
}

func modelRuntimeSuccessfulRefreshDo(t *testing.T, callbackID string) modelHTTPDo {
	t.Helper()
	return func(req *http.Request, gotCallbackID string) (*hostHTTPResponse, error) {
		if gotCallbackID != callbackID {
			t.Fatalf("callback ID = %q, want %q", gotCallbackID, callbackID)
		}
		switch req.URL.Host {
		case "copilot.tencent.com":
			return modelRuntimeFreshWorkBuddyResponse(), nil
		case "models.dev":
			return modelRuntimeFreshMetadataResponse(), nil
		default:
			t.Fatalf("unexpected model request %s", req.URL)
			return nil, nil
		}
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
