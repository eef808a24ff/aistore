// Package registry provides core functionality for the AIStore extended actions registry.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package registry

import (
	"fmt"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/hk"
	"github.com/NVIDIA/aistore/xaction"
)

const (
	// how often cleanup is called
	cleanupInterval = 10 * time.Minute

	// how long xaction had to finish to be considered to be removed
	entryOldAge = 1 * time.Hour

	// watermarks for entries size
	entriesSizeHW = 300

	// Initial capacity of entries array in registry.
	registryEntriesCap = 1000

	// Threshold (number of finished entries) to start `entries.active` slice cleanup
	hkFinishedCntThreshold = 50
)

type (
	XactFilter struct {
		ID          string
		Kind        string
		Bck         *cluster.Bck
		OnlyRunning *bool
	}

	// Selects subset of xactions to abort.
	abortArgs struct {
		// run on `bcks` buckets
		bcks []*cluster.Bck
		// mathcing `ty`
		ty string
		// all mountpath xactions - see cmn.IsMountpathXact()
		mountpaths bool
		// all or all matching `ty`, if defined
		all bool
	}
	// Represents result of renewing given xaction.
	renewRes struct {
		entry baseEntry // Depending on situation can be new or old entry.
		err   error     // Error that occurred during renewal.
		isNew bool      // true: a new entry has been created; false: old one returned
	}
	taskState struct {
		Result interface{} `json:"res"`
		Err    error       `json:"error"`
	}
	baseEntry interface {
		Start(bck cmn.Bck) error // starts an xaction, will be called when entry is stored into registry
		Kind() string
		Get() cluster.Xact
	}
	registryEntries struct {
		mtx       sync.RWMutex
		active    []baseEntry // running entries - finished entries are gradually removed
		entries   []baseEntry
		taskCount atomic.Int64
	}
	registry struct {
		mtx sync.RWMutex // lock for transactions
		// All entries in the registry. The entries are periodically cleaned up
		// to make sure that we don't keep old entries forever.
		entries *registryEntries

		bckXacts    map[string]BucketEntryProvider
		globalXacts map[string]GlobalEntryProvider
	}
)

func (rxf XactFilter) genericMatcher(xact cluster.Xact) bool {
	condition := true
	if rxf.OnlyRunning != nil {
		condition = condition && !xact.Finished() == *rxf.OnlyRunning
	}
	if rxf.Kind != "" {
		condition = condition && xact.Kind() == rxf.Kind
	}
	if rxf.Bck != nil {
		condition = condition && xact.Bck().Equal(rxf.Bck.Bck)
	}
	return condition
}

// Registry is a global registry (see above) that keeps track of all running xactions
// In addition, the registry retains already finished xactions subject to lazy cleanup via `hk`.
var Registry *registry

func init() {
	Registry = NewRegistry()
}

//
// registry
//

func newRegistryEntries() *registryEntries {
	return &registryEntries{
		entries: make([]baseEntry, 0, registryEntriesCap),
	}
}

func (e *registryEntries) findUnlocked(flt XactFilter) baseEntry {
	if flt.OnlyRunning == nil {
		// Loop in reverse to search for the latest (there is great chance
		// that searched xaction at the end rather at the beginning).
		for idx := len(e.entries) - 1; idx >= 0; idx-- {
			entry := e.entries[idx]
			if matchEntry(entry, flt) {
				return entry
			}
		}
	} else {
		cmn.AssertMsg(flt.Kind == "" || xaction.IsValidXaction(flt.Kind), flt.Kind)
		finishedCnt := 0
		for _, entry := range e.active {
			if entry.Get().Finished() {
				finishedCnt++
				continue
			}
			if matchEntry(entry, flt) {
				return entry
			}
		}
		if finishedCnt > hkFinishedCntThreshold {
			go e.housekeepActive()
		}
	}
	return nil
}

func (e *registryEntries) find(flt XactFilter) baseEntry {
	e.mtx.RLock()
	defer e.mtx.RUnlock()
	return e.findUnlocked(flt)
}

func (e *registryEntries) housekeepActive() {
	e.mtx.Lock()
	defer e.mtx.Unlock()
	newActive := e.active[:0]
	for _, entry := range e.active {
		if !entry.Get().Finished() {
			newActive = append(newActive, entry)
		}
	}
	e.active = newActive
}

func (e *registryEntries) forEach(matcher func(entry baseEntry) bool) {
	e.mtx.RLock()
	defer e.mtx.RUnlock()
	for _, entry := range e.entries {
		if !matcher(entry) {
			return
		}
	}
}

func (e *registryEntries) remove(id string) {
	e.mtx.Lock()
	defer e.mtx.Unlock()
	for idx, entry := range e.entries {
		if entry.Get().ID().String() == id {
			e.entries[idx] = e.entries[len(e.entries)-1]
			e.entries = e.entries[:len(e.entries)-1]

			if xaction.XactsDtor[entry.Kind()].Type == xaction.XactTypeTask {
				e.taskCount.Dec()
			}
			break
		}
	}
	for idx, entry := range e.active {
		if entry.Get().ID().String() == id {
			e.active[idx] = e.active[len(e.active)-1]
			e.active = e.active[:len(e.active)-1]
			return
		}
	}
}

func (e *registryEntries) insert(entry baseEntry) {
	e.mtx.Lock()
	e.active = append(e.active, entry)
	e.entries = append(e.entries, entry)
	e.mtx.Unlock()

	// Increase after cleanup to not force trigger it. If it was just added, for
	// sure it didn't yet finish.
	if xaction.XactsDtor[entry.Kind()].Type == xaction.XactTypeTask {
		e.taskCount.Inc()
	}
}

func (e *registryEntries) len() int {
	e.mtx.RLock()
	defer e.mtx.RUnlock()
	return len(e.entries)
}

// nolint:golint // It is used only by tests so no need to export the type.
func NewRegistry() *registry {
	xar := &registry{
		entries:     newRegistryEntries(),
		bckXacts:    make(map[string]BucketEntryProvider, 10),
		globalXacts: make(map[string]GlobalEntryProvider, 10),
	}
	hk.Reg("xactions", xar.cleanUpFinished)
	return xar
}

func (r *registry) GetXact(uuid string) (xact cluster.Xact) {
	r.entries.forEach(func(entry baseEntry) bool {
		x := entry.Get()
		if x != nil && x.ID().Compare(uuid) == 0 {
			xact = x
			return false
		}
		return true
	})
	return
}

func (r *registry) GetXactRunning(kind string) (xact cluster.Xact) {
	onlyRunning := false
	entry := r.entries.find(XactFilter{Kind: kind, OnlyRunning: &onlyRunning})
	if entry != nil {
		xact = entry.Get()
	}
	return
}

func (r *registry) GetRunning(flt XactFilter) baseEntry {
	onlyRunning := true
	flt.OnlyRunning = &onlyRunning
	entry := r.entries.find(flt)
	return entry
}

func (r *registry) GetLatest(flt XactFilter) baseEntry {
	entry := r.entries.find(flt)
	return entry
}

// AbortAllBuckets aborts all xactions that run with any of the provided bcks.
// It not only stops the "bucket xactions" but possibly "task xactions" which
// are running on given bucket.
func (r *registry) AbortAllBuckets(bcks ...*cluster.Bck) {
	r.abort(abortArgs{bcks: bcks})
}

// AbortAll waits until abort of all xactions is finished
// Every abort is done asynchronously
func (r *registry) AbortAll(tys ...string) {
	var ty string
	if len(tys) > 0 {
		ty = tys[0]
	}
	r.abort(abortArgs{all: true, ty: ty})
}

func (r *registry) AbortAllMountpathsXactions() {
	r.abort(abortArgs{mountpaths: true})
}

func (r *registry) abort(args abortArgs) {
	wg := &sync.WaitGroup{}
	r.entries.forEach(func(entry baseEntry) bool {
		xact := entry.Get()
		if xact.Finished() {
			return true
		}

		abort := false
		if args.mountpaths {
			if xact.IsMountpathXact() {
				abort = true
			}
		} else if len(args.bcks) > 0 {
			for _, bck := range args.bcks {
				if bck.Bck.Equal(xact.Bck()) {
					abort = true
					break
				}
			}
		} else if args.all {
			abort = true
			if args.ty != "" && args.ty != xaction.XactsDtor[xact.Kind()].Type {
				abort = false
			}
		}

		if abort {
			wg.Add(1)
			go func() {
				xact.Abort()
				wg.Done()
			}()
		}
		return true
	})
	wg.Wait()
}

func (r *registry) matchingXactsStats(match func(xact cluster.Xact) bool) []cluster.XactStats {
	matchingEntries := make([]baseEntry, 0, 20)
	r.entries.forEach(func(entry baseEntry) bool {
		if !match(entry.Get()) {
			return true
		}
		matchingEntries = append(matchingEntries, entry)
		return true
	})

	// TODO: we cannot do this inside `forEach` because possibly
	//  we have recursive RLock what can deadlock.
	sts := make([]cluster.XactStats, 0, len(matchingEntries))
	for _, entry := range matchingEntries {
		sts = append(sts, entry.Get().Stats())
	}
	return sts
}

func (r *registry) matchXactsStatsByID(xactID string) ([]cluster.XactStats, error) {
	matchedStat := r.matchingXactsStats(func(xact cluster.Xact) bool {
		return xact.ID().Compare(xactID) == 0
	})
	if len(matchedStat) == 0 {
		return nil, cmn.NewXactionNotFoundError("ID=" + xactID)
	}
	return matchedStat, nil
}

func (r *registry) GetStats(flt XactFilter) ([]cluster.XactStats, error) {
	if flt.ID != "" {
		if flt.OnlyRunning == nil || (flt.OnlyRunning != nil && *flt.OnlyRunning) {
			return r.matchXactsStatsByID(flt.ID)
		}
		return r.matchingXactsStats(func(xact cluster.Xact) bool {
			if xact.Kind() == cmn.ActRebalance {
				// Any rebalance after a given ID that finished and was not aborted
				return xact.ID().Compare(flt.ID) >= 0 && xact.Finished() && !xact.Aborted()
			}
			return xact.ID().Compare(flt.ID) == 0 && xact.Finished() && !xact.Aborted()
		}), nil
	}
	if flt.Bck != nil || flt.Kind != "" {
		// Error checks
		if flt.Kind != "" && !xaction.IsValidXaction(flt.Kind) {
			return nil, cmn.NewXactionNotFoundError(flt.Kind)
		}
		if flt.Bck != nil && !flt.Bck.HasProvider() {
			return nil, fmt.Errorf("xaction %q: unknown provider for bucket %s", flt.Kind, flt.Bck.Name)
		}

		// TODO: investigate how does the following fare against genericMatcher
		if flt.OnlyRunning != nil && *flt.OnlyRunning {
			matching := make([]cluster.XactStats, 0, 10)
			if flt.Kind == "" {
				for kind := range xaction.XactsDtor {
					entry := r.GetRunning(XactFilter{Kind: kind, Bck: flt.Bck})
					if entry != nil {
						matching = append(matching, entry.Get().Stats())
					}
				}
			} else {
				entry := r.GetRunning(XactFilter{Kind: flt.Kind, Bck: flt.Bck})
				if entry != nil {
					matching = append(matching, entry.Get().Stats())
				}
			}
			return matching, nil
		}
		return r.matchingXactsStats(flt.genericMatcher), nil
	}
	return r.matchingXactsStats(flt.genericMatcher), nil
}

func (r *registry) DoAbort(kind string, bck *cluster.Bck) (aborted bool) {
	if kind == "" {
		if bck == nil {
			// No bucket and no kind - request for all available xactions.
			r.AbortAll()
		} else {
			// Bucket present and no kind - request for all available bucket's xactions.
			r.AbortAllBuckets(bck)
		}
		aborted = true
	} else {
		entry := r.GetRunning(XactFilter{Kind: kind, Bck: bck})
		if entry == nil {
			return false
		}
		entry.Get().Abort()
		return true
	}
	return
}

func (r *registry) DoAbortByID(uuid string) (aborted bool) {
	entry := r.GetXact(uuid)
	if entry == nil || entry.Finished() {
		return false
	}
	entry.Abort()
	return true
}

func (r *registry) removeFinishedByID(id string) error {
	entry := r.entries.find(XactFilter{ID: id})
	if entry == nil {
		return nil
	}

	xact := entry
	if !xact.Get().Finished() {
		return fmt.Errorf("xaction %s(%s, %T) is running - duplicate ID?", xact.Kind(), id, xact.Get())
	}
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("cleanup: removing xaction %s (ID %s)", xact.Get(), id)
	}
	r.entries.remove(id)
	return nil
}

func (r *registry) storeEntry(entry baseEntry) {
	r.entries.insert(entry)
}

// FIXME: cleanup might not remove the most old entries for each kind
// creating 'holes' in xactions history. Fix should probably use heap
// or change in structure of byID
// cleanup is made when size of r.byID is bigger then entriesSizeHW
// but not more often than cleanupInterval
func (r *registry) cleanUpFinished() time.Duration {
	startTime := time.Now()
	if r.entries.taskCount.Load() == 0 {
		if r.entries.len() <= entriesSizeHW {
			return cleanupInterval
		}
	}
	toRemove := make([]string, 0, 100)
	r.entries.forEach(func(entry baseEntry) bool {
		var (
			xact = entry.Get()
			eID  = xact.ID()
		)

		if !xact.Finished() {
			return true
		}

		// if entry is type of task the task must be cleaned up always - no extra
		// checks besides it is finished at least entryOldAge ago.
		//
		// We need to check if the entry is not the most recent entry for
		// given kind. If it is we want to keep it anyway.
		onlyRunning := true
		switch xaction.XactsDtor[entry.Kind()].Type {
		case xaction.XactTypeGlobal:
			entry := r.entries.findUnlocked(XactFilter{Kind: entry.Kind(), OnlyRunning: &onlyRunning})
			if entry != nil && entry.Get().ID() == eID {
				return true
			}
		case xaction.XactTypeBck:
			bck := cluster.NewBckEmbed(xact.Bck())
			cmn.Assert(bck.HasProvider())
			entry := r.entries.findUnlocked(XactFilter{Kind: entry.Kind(), Bck: bck, OnlyRunning: &onlyRunning})
			if entry != nil && entry.Get().ID() == eID {
				return true
			}
		}

		if xact.EndTime().Add(entryOldAge).Before(startTime) {
			// xaction has finished more than entryOldAge ago
			toRemove = append(toRemove, eID.String())
			return true
		}
		return true
	})

	for _, id := range toRemove {
		r.entries.remove(id)
	}

	return cleanupInterval
}

//
// renew methods
//

func (r *registry) renewBucketXaction(entry BucketEntry, bck *cluster.Bck, uuids ...string) (res renewRes) {
	var uuid string
	if len(uuids) != 0 {
		uuid = uuids[0]
	}
	r.mtx.RLock()
	if e := r.GetRunning(XactFilter{ID: uuid, Kind: entry.Kind(), Bck: bck}); e != nil {
		prevEntry := e.(BucketEntry)
		if keep, err := entry.PreRenewHook(prevEntry); keep || err != nil {
			r.mtx.RUnlock()
			return renewRes{entry: prevEntry, isNew: false, err: err}
		}
	}
	r.mtx.RUnlock()

	r.mtx.Lock()
	defer r.mtx.Unlock()
	var (
		running   = false
		prevEntry BucketEntry
	)
	if e := r.GetRunning(XactFilter{ID: uuid, Kind: entry.Kind(), Bck: bck}); e != nil {
		prevEntry = e.(BucketEntry)
		running = true
		if keep, err := entry.PreRenewHook(prevEntry); keep || err != nil {
			return renewRes{entry: prevEntry, isNew: false, err: err}
		}
	}

	if err := entry.Start(bck.Bck); err != nil {
		return renewRes{entry: nil, isNew: true, err: err}
	}
	r.storeEntry(entry)
	if running {
		entry.PostRenewHook(prevEntry)
	}
	return renewRes{entry: entry, isNew: true, err: nil}
}

func (r *registry) renewGlobalXaction(entry GlobalEntry) (res renewRes) {
	r.mtx.RLock()
	if e := r.GetRunning(XactFilter{Kind: entry.Kind()}); e != nil {
		prevEntry := e.(GlobalEntry)
		if entry.PreRenewHook(prevEntry) {
			r.mtx.RUnlock()
			return renewRes{entry: prevEntry, isNew: false, err: nil}
		}
	}
	r.mtx.RUnlock()

	r.mtx.Lock()
	defer r.mtx.Unlock()
	var (
		running   = false
		prevEntry GlobalEntry
	)
	if e := r.GetRunning(XactFilter{Kind: entry.Kind()}); e != nil {
		prevEntry = e.(GlobalEntry)
		running = true
		if entry.PreRenewHook(prevEntry) {
			return renewRes{entry: prevEntry, isNew: false, err: nil}
		}
	}

	if err := entry.Start(cmn.Bck{}); err != nil {
		return renewRes{entry: nil, isNew: true, err: err}
	}
	r.storeEntry(entry)
	if running {
		entry.PostRenewHook(prevEntry)
	}
	return renewRes{entry: entry, isNew: true, err: nil}
}

func matchEntry(entry baseEntry, flt XactFilter) (matches bool) {
	if flt.ID != "" {
		return entry.Get().ID().Compare(flt.ID) == 0
	}
	if entry.Kind() == flt.Kind {
		if flt.Bck == nil || flt.Bck.IsEmpty() {
			return true
		}
		if !flt.Bck.IsEmpty() && entry.Get().Bck().Equal(flt.Bck.Bck) {
			return true
		}
	}
	return false
}
