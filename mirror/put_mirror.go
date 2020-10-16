// Package mirror provides local mirroring and replica management
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package mirror

import (
	"fmt"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/memsys"
	"github.com/NVIDIA/aistore/xaction"
	"github.com/NVIDIA/aistore/xaction/registry"
)

type (
	putMirrorProvider struct {
		registry.BaseBckEntry
		xact *XactPut

		t    cluster.Target
		uuid string
		lom  *cluster.LOM
	}
	XactPut struct {
		// implements cluster.Xact and cmn.Runner interfaces
		xaction.XactDemandBase
		// runtime
		workCh   chan *cluster.LOM
		mpathers map[string]mpather
		// init
		mirror  cmn.MirrorConf
		slab    *memsys.Slab
		total   atomic.Int64
		dropped int64
	}
	xputJogger struct { // one per mountpath
		parent    *XactPut
		mpathInfo *fs.MountpathInfo
		workCh    chan *cluster.LOM
		stopCh    *cmn.StopCh
	}
)

func (*putMirrorProvider) New(args registry.XactArgs) registry.BucketEntry {
	return &putMirrorProvider{t: args.T, uuid: args.UUID, lom: args.Custom.(*cluster.LOM)}
}

func (p *putMirrorProvider) Start(_ cmn.Bck) error {
	slab, err := p.t.MMSA().GetSlab(memsys.MaxPageSlabSize) // TODO: estimate
	cmn.AssertNoErr(err)
	xact, err := RunXactPut(p.lom, slab)
	if err != nil {
		glog.Error(err)
		return err
	}
	p.xact = xact
	return nil
}
func (*putMirrorProvider) Kind() string        { return cmn.ActPutCopies }
func (p *putMirrorProvider) Get() cluster.Xact { return p.xact }

//
// public methods
//

func RunXactPut(lom *cluster.LOM, slab *memsys.Slab) (r *XactPut, err error) {
	var (
		availablePaths, _ = fs.Get()
		mpathCount        = len(availablePaths)
	)
	r = &XactPut{
		XactDemandBase: *xaction.NewXactDemandBaseBck(cmn.ActPutCopies, lom.Bck().Bck),
		slab:           slab,
		mirror:         *lom.MirrorConf(),
	}
	if err = checkInsufficientMpaths(r, mpathCount); err != nil {
		r = nil
		return
	}
	r.workCh = make(chan *cluster.LOM, r.mirror.Burst)
	r.mpathers = make(map[string]mpather, mpathCount)
	r.InitIdle()

	// Run
	for _, mpathInfo := range availablePaths {
		mpathLC := mpathInfo.MakePathCT(r.Bck(), fs.ObjectType)
		r.mpathers[mpathLC] = newXputJogger(r, mpathInfo)
	}
	go func() {
		err := r.Run()
		r.Finish(err)
	}()
	for _, mpather := range r.mpathers {
		xputJogger := mpather.(*xputJogger)
		go xputJogger.jog()
	}
	return
}

func (r *XactPut) IsMountpathXact() bool { return true }

func (r *XactPut) Run() error {
	glog.Infoln(r.String())
	for {
		select {
		case src := <-r.workCh:
			lom := src.Clone(src.FQN)
			if err := lom.Load(); err != nil {
				glog.Error(err)
				break
			}
			path := lom.ParsedFQN.MpathInfo.MakePathCT(r.Bck(), fs.ObjectType)
			if mpather, ok := r.mpathers[path]; ok {
				mpather.post(lom)
			} else {
				glog.Errorf("failed to get mpather with path: %s", path)
			}
		case <-r.IdleTimer():
			return r.stop()
		case <-r.ChanAbort():
			if err := r.stop(); err != nil {
				return cmn.NewAbortedError(err.Error())
			}
			return cmn.NewAbortedError(r.String())
		}
	}
}

// main method: replicate a given locally stored object
func (r *XactPut) Repl(lom *cluster.LOM) (err error) {
	if r.Finished() {
		err = xaction.NewErrXactExpired("Cannot replicate: " + r.String())
		return
	}
	r.total.Inc()
	// [throttle]
	// when the optimization objective is write perf,
	// we start dropping requests to make sure callers don't block
	pending, max := r.Pending(), r.mirror.Burst
	if r.mirror.OptimizePUT {
		if pending > 1 && pending >= max {
			r.dropped++
			if (r.dropped % logNumProcessed) == 0 {
				glog.Errorf("%s: pending=%d, total=%d, dropped=%d", r, pending, r.total.Load(), r.dropped)
			}
			return
		}
	}
	r.IncPending() // ref-count via base to support on-demand action
	r.workCh <- lom

	// [throttle]
	// a bit of back-pressure when approaching the fixed boundary
	if pending > 1 && max > 10 {
		// increase the chances for the burst of PUTs to subside
		// but only to a point
		if pending > max/2 && pending < max-max/8 {
			time.Sleep(cmn.ThrottleAvg)
		}
	}
	return
}

//
// private methods
//

// =================== load balancing and self-throttling ========================
// Generally,
// load balancing decision must (... TODO ...) be configurable and a function of:
// - current utilization (%) of the filesystem's disks;
// - current disk queue lengths and their respective minimums and maximums during
//   the reporting period (config.Periodic.IostatTimeLong);
// - previous values of the same, and their corresponding averages.
//
// Further, load balancers must take into account relative priorities of
// other workloads that are simultaneously present in the system -
// and self-throttle accordingly. E.g., in most cases we'd want GET to have the
// top (default, configurable) priority which would mean that the filesystems that
// serve GETs are even less available for other extended actions than otherwise, etc.
// =================== load balancing and self-throttling ========================

func (r *XactPut) stop() (err error) {
	r.XactDemandBase.Stop()
	var n int
	for _, mpather := range r.mpathers {
		n += mpather.stop()
	}
	if nn := drainWorkCh(r.workCh, r.String()+" drop"); nn > 0 {
		n += nn
		r.SubPending(nn)
	}
	if n > 0 {
		err = fmt.Errorf("%s: dropped %d object(s)", r, n)
	}
	return
}

//
// xputJogger - main
//

func newXputJogger(parent *XactPut, mpathInfo *fs.MountpathInfo) *xputJogger {
	return &xputJogger{
		parent:    parent,
		mpathInfo: mpathInfo,
		workCh:    make(chan *cluster.LOM, parent.mirror.Burst),
		stopCh:    cmn.NewStopCh(),
	}
}

func (j *xputJogger) jog() {
	buf := j.parent.slab.Alloc()
	glog.Infof("xputJogger[%s] started", j.mpathInfo)

	for {
		select {
		case src := <-j.workCh:
			lom := src.Clone(src.FQN)
			copies := int(lom.Bprops().Mirror.Copies)
			if _, err := addCopies(lom, copies, j.parent.mpathers, buf); err != nil {
				glog.Error(err)
			} else {
				if v := j.parent.ObjectsAdd(int64(copies)); (v % logNumProcessed) == 0 {
					glog.Infof("%s: total=%d, copied=%d", j.parent.String(), j.parent.total.Load(), v)
				}
				j.parent.BytesAdd(lom.Size() * int64(copies))
			}
			j.parent.DecPending() // to support action renewal on-demand
		case <-j.stopCh.Listen():
			j.parent.slab.Free(buf)
			return
		}
	}
}

//
// xputJogger - as mpather
//

func (j *xputJogger) mountpathInfo() *fs.MountpathInfo { return j.mpathInfo }
func (j *xputJogger) post(lom *cluster.LOM)            { j.workCh <- lom }

func (j *xputJogger) stop() (n int) {
	tag := fmt.Sprintf("%s/%s drop", j.parent, j.mpathInfo)
	n = drainWorkCh(j.workCh, tag)
	if n > 0 {
		j.parent.SubPending(n)
	}
	j.stopCh.Close()
	return
}
