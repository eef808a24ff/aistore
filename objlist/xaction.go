// Package objlist provides xaction and utilities for listing bucket objects.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package objlist

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"sync"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/objwalk"
	"github.com/NVIDIA/aistore/objwalk/walkinfo"
	"github.com/NVIDIA/aistore/xaction"
	"github.com/NVIDIA/aistore/xaction/registry"
)

// Xaction is on-demand one to avoid creating a new xaction per page even
// in passthrough mode. It just restarts `walk` if needed.
// Xaction is created once per bucket list request (per UUID)
type (
	xactProvider struct {
		registry.BaseBckEntry
		xact *Xact

		ctx  context.Context
		t    cluster.Target
		uuid string
		msg  *cmn.SelectMsg
	}
	Xact struct {
		xaction.XactDemandBase
		ctx context.Context
		t   cluster.Target
		bck *cluster.Bck
		msg *cmn.SelectMsg

		workCh chan *cmn.SelectMsg // Incoming requests.
		respCh chan *Resp          // Outgoing responses.
		stopCh *cmn.StopCh         // Informs about stopped xaction.

		objCache   chan *cmn.BucketEntry // local cache filled when idle
		lastPage   []*cmn.BucketEntry    // last sent page and a little more
		walkStopCh *cmn.StopCh           // to abort file walk
		token      string                // the continuation token for the last sent page (for re-requests)
		nextToken  string                // continuation token returned by Cloud to get the next page
		walkWg     sync.WaitGroup        // to wait until walk finishes
		walkDone   bool                  // true: done walking or Cloud returned all objects
		fromRemote bool                  // whether to request remote data (Cloud/Remote/Backend)
	}

	Resp struct {
		BckList *cmn.BucketList
		Status  int
		Err     error
	}
)

const (
	cacheSize = 128 // the size of local cache filled in advance when idle
)

var (
	errStopped = errors.New("stopped")
	ErrGone    = errors.New("gone")
)

func init() {
	registry.Registry.RegisterBucketXact(&xactProvider{})
}

func (*xactProvider) New(args registry.XactArgs) registry.BucketEntry {
	return &xactProvider{ctx: args.Ctx, t: args.T, uuid: args.UUID, msg: args.Custom.(*cmn.SelectMsg)}
}

func (p *xactProvider) Start(bck cmn.Bck) error {
	p.xact = newXact(p.ctx, p.t, bck, p.msg, p.uuid)
	return nil
}
func (*xactProvider) Kind() string        { return cmn.ActListObjects }
func (p *xactProvider) Get() cluster.Xact { return p.xact }

func newXact(ctx context.Context, t cluster.Target, bck cmn.Bck, smsg *cmn.SelectMsg, uuid string) *Xact {
	idleTime := cmn.GCO.Get().Timeout.SendFile
	xact := &Xact{
		ctx:      ctx,
		t:        t,
		bck:      cluster.NewBckEmbed(bck),
		msg:      smsg,
		workCh:   make(chan *cmn.SelectMsg),
		respCh:   make(chan *Resp),
		stopCh:   cmn.NewStopCh(),
		lastPage: make([]*cmn.BucketEntry, 0, cacheSize),
	}
	cmn.Assert(xact.bck.Props != nil)

	xact.XactDemandBase = *xaction.NewXactDemandBaseBckUUID(uuid, cmn.ActListObjects, bck, idleTime)
	xact.InitIdle()
	return xact
}

func (r *Xact) String() string {
	return fmt.Sprintf("%s: %s", r.t.Snode(), &r.XactDemandBase)
}

func (r *Xact) Do(msg *cmn.SelectMsg) *Resp {
	// The guarantee here is that we either put something on the channel and our
	// request will be processed (since the `workCh` is unbuffered) or we receive
	// message that the xaction has been stopped.
	select {
	case r.workCh <- msg:
		return <-r.respCh
	case <-r.stopCh.Listen():
		return &Resp{Err: ErrGone}
	}
}

func (r *Xact) init() {
	r.fromRemote = !r.bck.IsAIS() && !r.msg.IsFlagSet(cmn.SelectCached)
	if r.fromRemote {
		return
	}

	// Start fs.Walk beforehand if needed so that by the time we read
	// the next page local cache is populated.
	r.initTraverse()
}

func (r *Xact) initTraverse() {
	if r.walkStopCh != nil {
		r.walkStopCh.Close()
		r.walkWg.Wait()
	}

	r.objCache = make(chan *cmn.BucketEntry, cacheSize)
	r.walkDone = false
	r.walkStopCh = cmn.NewStopCh()
	r.walkWg.Add(1)
	go r.traverseBucket()
}

func (r *Xact) Run() error {
	glog.Infoln(r.String())

	r.init()

	for {
		select {
		case msg := <-r.workCh:
			// Copy only the values that can change between calls
			debug.Assert(r.msg.UseCache == msg.UseCache)
			debug.Assert(r.msg.Prefix == msg.Prefix)
			debug.Assert(r.msg.Flags == msg.Flags)
			r.msg.ContinuationToken = msg.ContinuationToken
			r.msg.PageSize = msg.PageSize
			r.respCh <- r.dispatchRequest()
		case <-r.IdleTimer():
			r.stop()
			return nil
		case <-r.ChanAbort():
			r.stop()
			return nil
		}
	}
}

func (r *Xact) stopWalk() {
	if r.walkStopCh != nil {
		r.walkStopCh.Close()
		r.walkWg.Wait()
	}
}

func (r *Xact) stop() {
	r.XactDemandBase.Stop()
	r.Finish()
	r.stopCh.Close()
	// NOTE: Not closing `r.workCh` as it potentially could result in "sending on closed channel" panic.
	close(r.respCh)
	r.stopWalk()
}

func (r *Xact) dispatchRequest() *Resp {
	var (
		cnt   = r.msg.PageSize
		token = r.msg.ContinuationToken
	)

	debug.Assert(cnt != 0)

	r.IncPending()
	defer r.DecPending()

	if err := r.genNextPage(token, cnt); err != nil {
		return &Resp{
			Status: http.StatusInternalServerError,
			Err:    err,
		}
	}

	// TODO: We should remove it at some point.
	debug.Assert(r.pageIsValid(token, cnt))

	objList := r.getPage(token, cnt)
	return &Resp{
		BckList: objList,
		Status:  http.StatusOK,
	}
}

func (r *Xact) IsMountpathXact() bool { return true }

func (r *Xact) walkCallback(lom *cluster.LOM) {
	r.ObjectsInc()
	r.BytesAdd(lom.Size())
}

func (r *Xact) walkCtx() context.Context {
	return context.WithValue(
		context.Background(),
		walkinfo.CtxPostCallbackKey,
		walkinfo.PostCallbackFunc(r.walkCallback),
	)
}

func (r *Xact) nextPageAIS(cnt uint) error {
	if r.isPageCached(r.token, cnt) {
		return nil
	}
	for read := uint(0); read < cnt; {
		obj, ok := <-r.objCache
		if !ok {
			r.walkDone = true
			break
		}
		// Skip all objects until the requested marker.
		if cmn.TokenIncludesObject(r.token, obj.Name) {
			continue
		}
		read++
		r.lastPage = append(r.lastPage, obj)
	}
	return nil
}

// Returns an index of the first objects in the cache that follows marker
func (r *Xact) findMarker(marker string) uint {
	if r.fromRemote && r.token == marker {
		return 0
	}
	return uint(sort.Search(len(r.lastPage), func(i int) bool {
		return !cmn.TokenIncludesObject(marker, r.lastPage[i].Name)
	}))
}

func (r *Xact) isPageCached(marker string, cnt uint) bool {
	if r.walkDone {
		return true
	}
	idx := r.findMarker(marker)
	return idx+cnt < uint(len(r.lastPage))
}

func (r *Xact) nextPageCloud() error {
	walk := objwalk.NewWalk(r.walkCtx(), r.t, r.bck, r.msg)
	bckList, err := walk.CloudObjPage()
	if err != nil {
		r.nextToken = ""
		return err
	}
	if bckList.ContinuationToken == "" {
		r.walkDone = true
	}
	r.lastPage = bckList.Entries
	r.nextToken = bckList.ContinuationToken
	r.lastPage = append(r.lastPage, bckList.Entries...)
	return nil
}

// Called before generating a page for a proxy. It is OK if the page is
// still in progress. If the page is done, the function ensures that the
// local cache contains the requested data.
func (r *Xact) pageIsValid(marker string, cnt uint) bool {
	// The same page is re-requested
	if r.token == marker {
		return true
	}
	if r.fromRemote {
		return r.walkDone || uint(len(r.lastPage)) >= cnt
	}
	if cmn.TokenIncludesObject(r.token, marker) {
		// Requested a status about page returned a few pages ago
		return false
	}
	idx := r.findMarker(marker)
	inCache := idx+cnt <= uint(len(r.lastPage))
	return inCache || r.walkDone
}

func (r *Xact) getPage(marker string, cnt uint) *cmn.BucketList {
	debug.Assert(r.msg.UUID != "")
	if r.fromRemote {
		return &cmn.BucketList{
			UUID:              r.msg.UUID,
			Entries:           r.lastPage,
			ContinuationToken: r.nextToken,
		}
	}

	var (
		idx  = r.findMarker(marker)
		list = r.lastPage[idx:]
	)

	debug.Assert(uint(len(list)) >= cnt || r.walkDone)

	if uint(len(list)) >= cnt {
		entries := list[:cnt]
		return &cmn.BucketList{
			UUID:              r.msg.UUID,
			Entries:           entries,
			ContinuationToken: entries[cnt-1].Name,
		}
	}
	return &cmn.BucketList{Entries: list, UUID: r.msg.UUID}
}

// genNextPage calls DecPending either immediately on error or inside
// a goroutine if some work must be done.
func (r *Xact) genNextPage(token string, cnt uint) error {
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("[%s] token: %q", r, r.msg.ContinuationToken)
	}
	if token != "" && token == r.token {
		return nil
	}

	// Due to impossibility of getting object name from continuation token,
	// in case of Cloud, a target keeps only the entire last sent page.
	// The page is replaced with a new one when a client asks for next page.
	if r.fromRemote {
		r.token = token
		return r.nextPageCloud()
	}

	if r.token > token {
		r.initTraverse() // Restart traversing as we cannot go back in time :(.
		r.lastPage = r.lastPage[:0]
	} else {
		if r.walkDone {
			return nil
		}
		r.discardObsolete(token)
	}
	r.token = token
	return r.nextPageAIS(cnt)
}

// Removes from local cache, the objects that have been already sent.
// Use only for AIS buckets(or Cached:true requests) - in other cases
// the marker, in general, is not an object name
func (r *Xact) discardObsolete(token string) {
	if token == "" || len(r.lastPage) == 0 {
		return
	}
	j := r.findMarker(token)
	// Entire cache is "after" page marker, keep the whole cache
	if j == 0 {
		return
	}
	l := uint(len(r.lastPage))
	// All the cache data have been sent to clients, clean it up
	if j == l {
		r.lastPage = r.lastPage[:0]
		return
	}
	// To reuse local cache, copy items and fix the slice length
	copy(r.lastPage[0:], r.lastPage[j:])
	r.lastPage = r.lastPage[:l-j]
}

func (r *Xact) traverseBucket() {
	wi := walkinfo.NewWalkInfo(r.walkCtx(), r.t, r.msg)
	defer r.walkWg.Done()
	cb := func(fqn string, de fs.DirEntry) error {
		entry, err := wi.Callback(fqn, de)
		if err != nil || entry == nil {
			return err
		}
		if entry.Name <= r.msg.StartAfter {
			return nil
		}
		select {
		case r.objCache <- entry:
			/* do nothing */
		case <-r.walkStopCh.Listen():
			return errStopped
		}
		return nil
	}
	opts := &fs.WalkBckOptions{
		Options: fs.Options{
			Bck:      r.Bck(),
			CTs:      []string{fs.ObjectType},
			Callback: cb,
			Sorted:   true,
		},
		ValidateCallback: func(fqn string, de fs.DirEntry) error {
			if de.IsDir() {
				return wi.ProcessDir(fqn)
			}
			return nil
		},
	}

	if err := fs.WalkBck(opts); err != nil {
		if err != filepath.SkipDir && err != errStopped {
			glog.Errorf("%s walk failed, err %v", r, err)
		}
	}
	close(r.objCache)
}
