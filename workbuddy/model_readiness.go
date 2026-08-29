package main

import (
	"crypto/sha256"
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

type modelGenerationKey struct {
	TokenSHA256    [sha256.Size]byte
	IdentitySHA256 string
}

type modelAuthSlot struct {
	mu       sync.Mutex
	current  atomic.Pointer[modelReadinessSnapshot]
	calls    map[uint64]*modelAuthCall
	nextAuth uint64
	key      modelGenerationKey
}

type modelAuthCall struct {
	done chan struct{}
}

type metadataCall struct {
	done   chan struct{}
	result modelMetadataResult
}

type modelRuntime struct {
	store            *modelStore
	do               modelHTTPDo
	storeError       modelErrorCode
	configGeneration atomic.Uint64
	authSlots        sync.Map
	metadataMu       sync.Mutex
	metadataCall     *metadataCall
	metadataCache    *metadataCacheV1
	metadataResult   *modelMetadataResult
}

var activeModelRuntime atomic.Pointer[modelRuntime]

func newModelRuntime(store *modelStore, do modelHTTPDo) *modelRuntime {
	runtime := &modelRuntime{store: store, do: do}
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

func (r *modelRuntime) authSlot(authID string) *modelAuthSlot {
	candidate := &modelAuthSlot{calls: make(map[uint64]*modelAuthCall)}
	loaded, _ := r.authSlots.LoadOrStore(authID, candidate)
	return loaded.(*modelAuthSlot)
}

func (r *modelRuntime) ensureForAuth(req authModelRequestWire) modelReadinessSnapshot {
	slot := r.authSlot(req.AuthID)
	sa, err := parseStored(req.StorageJSON)
	if err == nil {
		_, err = workBuddyRealmFromAccessToken(sa.Auth.AccessToken)
	}
	var identity modelAuthIdentity
	if err == nil {
		identity, err = modelAuthIdentityFor(req.AuthID, sa)
	}
	if err != nil {
		return storeModelReadinessSnapshot(slot, modelReadinessSnapshot{
			State:            modelFailed,
			ModelSource:      modelSourceNone,
			MetadataSource:   modelSourceNone,
			ErrorCode:        modelErrorAuthInvalid,
			Models:           []pluginapi.ModelInfo{},
			configGeneration: r.configGeneration.Load(),
		})
	}

	identitySHA256 := identity.sha256()
	key := modelGenerationKey{
		TokenSHA256:    sha256.Sum256([]byte(sa.Auth.AccessToken)),
		IdentitySHA256: identitySHA256,
	}
	slot.mu.Lock()
	if slot.key != key {
		slot.key = key
		slot.nextAuth++
	}
	authGeneration := slot.nextAuth
	if current := slot.current.Load(); current != nil && current.authGeneration == authGeneration && current.State != modelLoading {
		result := cloneModelReadinessSnapshot(*current)
		slot.mu.Unlock()
		return result
	}
	if call := slot.calls[authGeneration]; call != nil {
		slot.mu.Unlock()
		<-call.done
		return r.snapshotForAuthID(req.AuthID)
	}
	call := &modelAuthCall{done: make(chan struct{})}
	slot.calls[authGeneration] = call
	snapshot := modelReadinessSnapshot{
		State:            modelLoading,
		ModelSource:      modelSourceNone,
		MetadataSource:   modelSourceNone,
		Models:           []pluginapi.ModelInfo{},
		configGeneration: r.configGeneration.Load(),
		authGeneration:   authGeneration,
		identitySHA256:   identitySHA256,
	}
	storeModelReadinessSnapshot(slot, snapshot)
	slot.mu.Unlock()

	if r.storeError != modelErrorNone || r.store == nil {
		snapshot.State = modelFailed
		snapshot.ErrorCode = modelErrorCacheRead
		return r.finishAuthCall(slot, authGeneration, call, snapshot)
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
	metadata := r.metadataForAuth(req.HostCallbackID)

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
		return r.finishAuthCall(slot, authGeneration, call, snapshot)
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
	return r.finishAuthCall(slot, authGeneration, call, snapshot)
}

func (r *modelRuntime) finishAuthCall(slot *modelAuthSlot, authGeneration uint64, call *modelAuthCall, snapshot modelReadinessSnapshot) modelReadinessSnapshot {
	slot.mu.Lock()
	result := storeModelReadinessSnapshot(slot, snapshot)
	close(call.done)
	if slot.calls[authGeneration] == call {
		delete(slot.calls, authGeneration)
	}
	slot.mu.Unlock()
	return result
}

func (r *modelRuntime) metadataForAuth(callbackID string) modelMetadataResult {
	r.metadataMu.Lock()
	if r.metadataResult != nil && r.metadataResult.ok {
		result := *r.metadataResult
		r.metadataMu.Unlock()
		return result
	}
	if call := r.metadataCall; call != nil {
		r.metadataMu.Unlock()
		<-call.done
		return call.result
	}

	call := &metadataCall{done: make(chan struct{})}
	r.metadataCall = call
	var cached metadataCacheV1
	cachedOK := r.metadataCache != nil
	if cachedOK {
		cached = *r.metadataCache
	}
	cacheReadFailed := r.metadataResult != nil && r.metadataResult.errorCode == modelErrorCacheRead
	r.metadataMu.Unlock()

	var fresh metadataCacheV1
	freshOK := false
	failure := modelErrorNone
	fetched, err := fetchModelsDevMetadata(cached.ETag, callbackID, r.do)
	if err != nil {
		failure = modelsDevModelErrorCode(err)
	} else if fetched.NotModified {
		if cachedOK {
			fresh = cached
			freshOK = true
		} else {
			failure = modelErrorModelsDevSchema
		}
	} else {
		fresh = metadataCacheV1{
			SchemaVersion: modelCacheSchemaVersion,
			ETag:          fetched.ETag,
			FetchedAt:     time.Now().UTC(),
			Records:       fetched.Records,
		}
		if err := r.store.saveMetadata(fresh); err != nil {
			failure = modelErrorCacheWrite
			if cacheReadFailed {
				failure = modelErrorCacheRead
			}
		} else {
			freshOK = true
		}
	}
	selected := selectMetadata(fresh, freshOK, cached, cachedOK, failure)

	r.metadataMu.Lock()
	call.result = selected
	if selected.ok {
		settled := selected
		cache := selected.cache
		r.metadataResult = &settled
		r.metadataCache = &cache
	} else if cacheReadFailed {
		failed := modelMetadataResult{source: modelSourceNone, errorCode: modelErrorCacheRead}
		r.metadataResult = &failed
	} else {
		r.metadataResult = nil
	}
	close(call.done)
	if r.metadataCall == call {
		r.metadataCall = nil
	}
	r.metadataMu.Unlock()
	return selected
}

func (r *modelRuntime) snapshotForAuthID(authID string) modelReadinessSnapshot {
	if snapshot := r.authSlot(authID).current.Load(); snapshot != nil {
		return cloneModelReadinessSnapshot(*snapshot)
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
	r.metadataMu.Lock()
	defer r.metadataMu.Unlock()
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
	storeModelReadinessSnapshot(r.authSlot(authID), modelReadinessSnapshot{
		State:            modelNotStarted,
		ModelSource:      modelSourceNone,
		MetadataSource:   modelSourceNone,
		Models:           []pluginapi.ModelInfo{},
		configGeneration: r.configGeneration.Load(),
	})
}

func storeModelReadinessSnapshot(slot *modelAuthSlot, snapshot modelReadinessSnapshot) modelReadinessSnapshot {
	published := cloneModelReadinessSnapshot(snapshot)
	slot.current.Store(&published)
	return cloneModelReadinessSnapshot(published)
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
