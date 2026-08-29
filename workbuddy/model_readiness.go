package main

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type authModelRequestWire struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type modelReadinessState string

const (
	modelNotStarted modelReadinessState = "not_started"
	modelLoading    modelReadinessState = "loading"
	modelReady      modelReadinessState = "ready"
	modelStale      modelReadinessState = "stale"
	modelFailed     modelReadinessState = "failed"
)

type modelSnapshotSource string

const (
	modelSourceFresh modelSnapshotSource = "fresh"
	modelSourceCache modelSnapshotSource = "cache"
	modelSourceNone  modelSnapshotSource = "none"
)

func (s modelReadinessState) executable() bool {
	return s == modelReady || s == modelStale
}

type modelErrorCode string

const (
	modelErrorNone               modelErrorCode = ""
	modelErrorAuthInvalid        modelErrorCode = "auth_invalid"
	modelErrorWorkBuddyTransport modelErrorCode = "workbuddy_transport"
	modelErrorWorkBuddyHTTP      modelErrorCode = "workbuddy_http"
	modelErrorWorkBuddySchema    modelErrorCode = "workbuddy_schema"
	modelErrorModelsDevTransport modelErrorCode = "models_dev_transport"
	modelErrorModelsDevHTTP      modelErrorCode = "models_dev_http"
	modelErrorModelsDevSchema    modelErrorCode = "models_dev_schema"
	modelErrorCacheRead          modelErrorCode = "cache_read"
	modelErrorCacheWrite         modelErrorCode = "cache_write"
)

type modelReadinessSnapshot struct {
	State             modelReadinessState
	ModelSource       modelSnapshotSource
	MetadataSource    modelSnapshotSource
	ModelsFetchedAt   time.Time
	MetadataFetchedAt time.Time
	ErrorCode         modelErrorCode
	Models            []pluginapi.ModelInfo
	configGeneration  uint64
	authGeneration    uint64
	identitySHA256    string
}

func (s modelReadinessSnapshot) executable() bool {
	return s.State.executable()
}

type modelMetadataStatus struct {
	Source    modelSnapshotSource
	FetchedAt time.Time
	ErrorCode modelErrorCode
}

type modelCatalogSelection struct {
	cache     modelCatalogCacheV1
	source    modelSnapshotSource
	errorCode modelErrorCode
	ok        bool
}

func selectModelCatalog(
	fresh modelCatalogCacheV1,
	freshOK bool,
	cached modelCatalogCacheV1,
	cacheOK bool,
	failure modelErrorCode,
) modelCatalogSelection {
	if freshOK {
		return modelCatalogSelection{cache: fresh, source: modelSourceFresh, ok: true}
	}
	if cacheOK {
		return modelCatalogSelection{cache: cached, source: modelSourceCache, errorCode: failure, ok: true}
	}
	return modelCatalogSelection{source: modelSourceNone, errorCode: failure}
}

type metadataSelection struct {
	cache     metadataCacheV1
	source    modelSnapshotSource
	errorCode modelErrorCode
	ok        bool
}

func selectMetadata(
	fresh metadataCacheV1,
	freshOK bool,
	cached metadataCacheV1,
	cacheOK bool,
	failure modelErrorCode,
) metadataSelection {
	if freshOK {
		return metadataSelection{cache: fresh, source: modelSourceFresh, ok: true}
	}
	if cacheOK {
		return metadataSelection{cache: cached, source: modelSourceCache, errorCode: failure, ok: true}
	}
	return metadataSelection{source: modelSourceNone, errorCode: failure}
}

type modelMetadataResult = metadataSelection

type modelRuntime struct {
	store            *modelStore
	do               modelHTTPDo
	storeError       modelErrorCode
	mu               sync.Mutex
	snapshots        map[string]modelReadinessSnapshot
	metadataCache    *metadataCacheV1
	metadataResult   *modelMetadataResult
	configGeneration atomic.Uint64
}

var activeModelRuntime atomic.Pointer[modelRuntime]

func newModelRuntime(store *modelStore, do modelHTTPDo) *modelRuntime {
	runtime := &modelRuntime{
		store:     store,
		do:        do,
		snapshots: make(map[string]modelReadinessSnapshot),
	}
	if store == nil {
		runtime.storeError = modelErrorCacheRead
		return runtime
	}
	cache, found, err := store.loadMetadata()
	if err != nil {
		runtime.metadataResult = &modelMetadataResult{source: modelSourceNone, errorCode: modelErrorCacheRead}
	} else if found {
		runtime.metadataCache = &cache
	}
	return runtime
}

func currentModelRuntime() *modelRuntime {
	if runtime := activeModelRuntime.Load(); runtime != nil {
		return runtime
	}
	store, err := defaultModelStore()
	var candidate *modelRuntime
	if err != nil {
		candidate = newModelRuntime(nil, hostHTTPDoWithCallback)
	} else {
		candidate = newModelRuntime(store, hostHTTPDoWithCallback)
	}
	if activeModelRuntime.CompareAndSwap(nil, candidate) {
		return candidate
	}
	return activeModelRuntime.Load()
}

func (r *modelRuntime) ensureForAuth(req authModelRequestWire) modelReadinessSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()

	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return r.publishLocked(req.AuthID, modelReadinessSnapshot{
			State:          modelFailed,
			ErrorCode:      modelErrorAuthInvalid,
			Models:         []pluginapi.ModelInfo{},
			ModelSource:    modelSourceNone,
			MetadataSource: modelSourceNone,
		})
	}
	if _, err := workBuddyRealmFromAccessToken(sa.Auth.AccessToken); err != nil {
		return r.publishLocked(req.AuthID, modelReadinessSnapshot{
			State:          modelFailed,
			ModelSource:    modelSourceNone,
			MetadataSource: modelSourceNone,
			ErrorCode:      modelErrorAuthInvalid,
			Models:         []pluginapi.ModelInfo{},
		})
	}
	identity, err := modelAuthIdentityFor(req.AuthID, sa)
	if err != nil {
		return r.publishLocked(req.AuthID, modelReadinessSnapshot{
			State:          modelFailed,
			ModelSource:    modelSourceNone,
			MetadataSource: modelSourceNone,
			ErrorCode:      modelErrorAuthInvalid,
			Models:         []pluginapi.ModelInfo{},
		})
	}
	identitySHA256 := identity.sha256()
	tokenSHA256 := sha256.Sum256([]byte(sa.Auth.AccessToken))
	snapshot := modelReadinessSnapshot{
		State:            modelLoading,
		ModelSource:      modelSourceNone,
		MetadataSource:   modelSourceNone,
		Models:           []pluginapi.ModelInfo{},
		configGeneration: r.configGeneration.Load(),
		authGeneration:   binary.BigEndian.Uint64(tokenSHA256[:]),
		identitySHA256:   identitySHA256,
	}
	r.publishLocked(req.AuthID, snapshot)

	if r.storeError != modelErrorNone || r.store == nil {
		snapshot.State = modelFailed
		snapshot.ErrorCode = modelErrorCacheRead
		return r.publishLocked(req.AuthID, snapshot)
	}

	cachedModels, cachedModelsOK, modelCacheErr := r.store.loadModels(identitySHA256)
	var freshModels modelCatalogCacheV1
	freshModelsOK := false
	modelFailure := modelErrorNone
	catalog, err := fetchWorkBuddyCatalog(sa, req.HostCallbackID, r.do)
	if err != nil {
		modelFailure = workBuddyModelErrorCode(err)
	} else {
		freshModels = modelCatalogCacheV1{
			SchemaVersion:  modelCacheSchemaVersion,
			IdentitySHA256: identitySHA256,
			Realm:          catalog.Realm,
			FetchedAt:      time.Now().UTC(),
			Endpoint:       catalog.Endpoint,
			Models:         catalog.Models,
		}
		if err := r.store.saveModels(freshModels); err != nil {
			modelFailure = modelErrorCacheWrite
			if modelCacheErr != nil {
				modelFailure = modelErrorCacheRead
			}
		} else {
			freshModelsOK = true
		}
	}
	modelSelection := selectModelCatalog(freshModels, freshModelsOK, cachedModels, cachedModelsOK, modelFailure)

	metadata := r.metadataResult
	if metadata == nil || !metadata.ok {
		var cachedMetadata metadataCacheV1
		cachedMetadataOK := r.metadataCache != nil
		if cachedMetadataOK {
			cachedMetadata = *r.metadataCache
		}

		var freshMetadata metadataCacheV1
		freshMetadataOK := false
		metadataFailure := modelErrorNone
		fetched, err := fetchModelsDevMetadata(cachedMetadata.ETag, req.HostCallbackID, r.do)
		if err != nil {
			metadataFailure = modelsDevModelErrorCode(err)
		} else if fetched.NotModified {
			if cachedMetadataOK {
				freshMetadata = cachedMetadata
				freshMetadataOK = true
			} else {
				metadataFailure = modelErrorModelsDevSchema
			}
		} else {
			freshMetadata = metadataCacheV1{
				SchemaVersion: modelCacheSchemaVersion,
				ETag:          fetched.ETag,
				FetchedAt:     time.Now().UTC(),
				Records:       fetched.Records,
			}
			if err := r.store.saveMetadata(freshMetadata); err != nil {
				metadataFailure = modelErrorCacheWrite
				if metadata != nil && metadata.errorCode == modelErrorCacheRead {
					metadataFailure = modelErrorCacheRead
				}
			} else {
				freshMetadataOK = true
				r.metadataCache = &freshMetadata
			}
		}

		selected := selectMetadata(freshMetadata, freshMetadataOK, cachedMetadata, cachedMetadataOK, metadataFailure)
		metadata = &selected
		if selected.ok {
			r.metadataResult = metadata
		}
	}

	snapshot.ModelSource = modelSelection.source
	if modelSelection.ok {
		snapshot.ModelsFetchedAt = modelSelection.cache.FetchedAt
	}
	snapshot.MetadataSource = metadata.source
	if metadata.ok {
		snapshot.MetadataFetchedAt = metadata.cache.FetchedAt
	}
	snapshot.ErrorCode = modelSelection.errorCode
	if snapshot.ErrorCode == modelErrorNone {
		snapshot.ErrorCode = metadata.errorCode
	}
	if !modelSelection.ok || !metadata.ok {
		snapshot.State = modelFailed
		return r.publishLocked(req.AuthID, snapshot)
	}

	models := make([]pluginapi.ModelInfo, len(modelSelection.cache.Models))
	for i, model := range modelSelection.cache.Models {
		models[i] = modelInfoFromSources(model, matchModelsDevRecord(model.ID, metadata.cache.Records))
	}
	snapshot.Models = models
	if modelSelection.source == modelSourceFresh && metadata.source == modelSourceFresh {
		snapshot.State = modelReady
		snapshot.ErrorCode = modelErrorNone
	} else {
		snapshot.State = modelStale
	}
	return r.publishLocked(req.AuthID, snapshot)
}

func (r *modelRuntime) snapshotForAuthID(authID string) modelReadinessSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	if snapshot, ok := r.snapshots[authID]; ok {
		return cloneModelReadinessSnapshot(snapshot)
	}
	return modelReadinessSnapshot{
		State:            modelNotStarted,
		ModelSource:      modelSourceNone,
		MetadataSource:   modelSourceNone,
		Models:           []pluginapi.ModelInfo{},
		configGeneration: r.configGeneration.Load(),
	}
}

func (r *modelRuntime) metadataStatus() modelMetadataStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.metadataResult != nil {
		return modelMetadataStatus{
			Source:    r.metadataResult.source,
			FetchedAt: r.metadataResult.cache.FetchedAt,
			ErrorCode: r.metadataResult.errorCode,
		}
	}
	if r.metadataCache != nil {
		return modelMetadataStatus{Source: modelSourceCache, FetchedAt: r.metadataCache.FetchedAt}
	}
	return modelMetadataStatus{Source: modelSourceNone, ErrorCode: r.storeError}
}

func (r *modelRuntime) advanceConfigGeneration() uint64 {
	return r.configGeneration.Add(1)
}

func (r *modelRuntime) markAuthNotStarted(authID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snapshots[authID] = modelReadinessSnapshot{
		State:            modelNotStarted,
		ModelSource:      modelSourceNone,
		MetadataSource:   modelSourceNone,
		Models:           []pluginapi.ModelInfo{},
		configGeneration: r.configGeneration.Load(),
	}
}

func (r *modelRuntime) publishLocked(authID string, snapshot modelReadinessSnapshot) modelReadinessSnapshot {
	r.snapshots[authID] = cloneModelReadinessSnapshot(snapshot)
	return snapshot
}

func cloneModelInfo(info pluginapi.ModelInfo) pluginapi.ModelInfo {
	info.SupportedGenerationMethods = append([]string(nil), info.SupportedGenerationMethods...)
	info.SupportedParameters = append([]string(nil), info.SupportedParameters...)
	info.SupportedInputModalities = append([]string(nil), info.SupportedInputModalities...)
	info.SupportedOutputModalities = append([]string(nil), info.SupportedOutputModalities...)
	if info.Thinking != nil {
		thinking := *info.Thinking
		thinking.Levels = append([]string(nil), info.Thinking.Levels...)
		info.Thinking = &thinking
	}
	return info
}

func cloneModelInfos(models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	if models == nil {
		return nil
	}
	cloned := make([]pluginapi.ModelInfo, len(models))
	for i, model := range models {
		cloned[i] = cloneModelInfo(model)
	}
	return cloned
}

func cloneModelReadinessSnapshot(snapshot modelReadinessSnapshot) modelReadinessSnapshot {
	snapshot.Models = cloneModelInfos(snapshot.Models)
	return snapshot
}

func workBuddyModelErrorCode(err error) modelErrorCode {
	return modelSourceErrorCode(err, modelErrorWorkBuddyTransport, modelErrorWorkBuddyHTTP, modelErrorWorkBuddySchema)
}

func modelsDevModelErrorCode(err error) modelErrorCode {
	return modelSourceErrorCode(err, modelErrorModelsDevTransport, modelErrorModelsDevHTTP, modelErrorModelsDevSchema)
}

func modelSourceErrorCode(err error, transport, http, schema modelErrorCode) modelErrorCode {
	var sourceErr *modelSourceError
	if !errors.As(err, &sourceErr) {
		return schema
	}
	switch sourceErr.Kind {
	case modelSourceTransportFailure:
		return transport
	case modelSourceHTTPFailure:
		return http
	default:
		return schema
	}
}
