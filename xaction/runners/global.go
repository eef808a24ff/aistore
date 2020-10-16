// Package runners provides implementation for the AIStore extended actions.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package runners

import (
	"fmt"
	"sync"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/stats"
	"github.com/NVIDIA/aistore/xaction"
	"github.com/NVIDIA/aistore/xaction/registry"
)

func init() {
	registry.Registry.RegisterGlobalXact(&electionProvider{})
	registry.Registry.RegisterGlobalXact(&resilverProvider{})
	registry.Registry.RegisterGlobalXact(&rebalanceProvider{})
}

type (
	getMarked = func() xaction.XactMarked
	RebBase   struct {
		xaction.XactBase
		wg *sync.WaitGroup
	}

	rebalanceProvider struct {
		xact *Rebalance
		args *registry.RebalanceArgs
	}
	Rebalance struct {
		RebBase
		statsRunner  *stats.Trunner // extended stats
		getRebMarked getMarked
	}

	resilverProvider struct {
		xact *Resilver
		id   string
	}
	Resilver struct {
		RebBase
	}

	electionProvider struct {
		xact *Election
	}
	Election struct {
		xaction.XactBase
	}
)

func (xact *RebBase) MarkDone()      { xact.wg.Done() }
func (xact *RebBase) WaitForFinish() { xact.wg.Wait() }

func (xact *RebBase) String() string {
	s := xact.XactBase.String()
	if xact.Bck().Name != "" {
		s += ", bucket " + xact.Bck().String()
	}
	return s
}

//
// resilver|rebalance helper
//
func makeXactRebBase(id cluster.XactID, kind string) RebBase {
	wg := &sync.WaitGroup{}
	wg.Add(1)
	return RebBase{
		XactBase: *xaction.NewXactBase(id, kind),
		wg:       wg,
	}
}

// Rebalance

func (p *rebalanceProvider) New(args registry.XactArgs) registry.GlobalEntry {
	return &rebalanceProvider{args: args.Custom.(*registry.RebalanceArgs)}
}

func (p *rebalanceProvider) Start(_ cmn.Bck) error {
	p.xact = NewRebalance(p.args.ID, p.Kind(), p.args.StatsRunner, registry.GetRebMarked)
	return nil
}
func (*rebalanceProvider) Kind() string        { return cmn.ActRebalance }
func (p *rebalanceProvider) Get() cluster.Xact { return p.xact }
func (p *rebalanceProvider) PreRenewHook(previousEntry registry.GlobalEntry) (keep bool) {
	xreb := previousEntry.(*rebalanceProvider)
	if xreb.args.ID > p.args.ID {
		glog.Errorf("(reb: %s) g%d is greater than g%d", xreb.xact, xreb.args.ID, p.args.ID)
		keep = true
	} else if xreb.args.ID == p.args.ID {
		if glog.FastV(4, glog.SmoduleAIS) {
			glog.Infof("%s already running, nothing to do", xreb.xact)
		}
		keep = true
	}
	return
}

func (p *rebalanceProvider) PostRenewHook(previousEntry registry.GlobalEntry) {
	xreb := previousEntry.(*rebalanceProvider).xact
	xreb.Abort()
	xreb.WaitForFinish()
}

func NewRebalance(id cluster.XactID, kind string, statsRunner *stats.Trunner, getMarked getMarked) *Rebalance {
	return &Rebalance{
		RebBase:      makeXactRebBase(id, kind),
		statsRunner:  statsRunner,
		getRebMarked: getMarked,
	}
}

func (xact *Rebalance) IsMountpathXact() bool { return false }

func (xact *Rebalance) String() string {
	return fmt.Sprintf("%s, %s", xact.RebBase.String(), xact.ID())
}

// override/extend cmn.XactBase.Stats()
func (xact *Rebalance) Stats() cluster.XactStats {
	var (
		baseStats   = xact.XactBase.Stats().(*xaction.BaseXactStats)
		rebStats    = stats.RebalanceTargetStats{BaseXactStats: *baseStats}
		statsRunner = xact.statsRunner
	)
	rebStats.Ext.RebTxCount = statsRunner.Get(stats.RebTxCount)
	rebStats.Ext.RebTxSize = statsRunner.Get(stats.RebTxSize)
	rebStats.Ext.RebRxCount = statsRunner.Get(stats.RebRxCount)
	rebStats.Ext.RebRxSize = statsRunner.Get(stats.RebRxSize)
	if marked := xact.getRebMarked(); marked.Xact != nil {
		rebStats.Ext.RebID = marked.Xact.ID().Int()
	} else {
		rebStats.Ext.RebID = 0
	}
	rebStats.ObjCountX = rebStats.Ext.RebTxCount + rebStats.Ext.RebRxCount
	rebStats.BytesCountX = rebStats.Ext.RebTxSize + rebStats.Ext.RebRxSize
	return &rebStats
}

// Resilver

func (*resilverProvider) New(args registry.XactArgs) registry.GlobalEntry {
	return &resilverProvider{id: args.UUID}
}

func (p *resilverProvider) Start(_ cmn.Bck) error {
	p.xact = NewResilver(p.id, p.Kind())
	return nil
}
func (*resilverProvider) Kind() string                               { return cmn.ActResilver }
func (p *resilverProvider) Get() cluster.Xact                        { return p.xact }
func (p *resilverProvider) PreRenewHook(_ registry.GlobalEntry) bool { return false }
func (p *resilverProvider) PostRenewHook(previousEntry registry.GlobalEntry) {
	xresilver := previousEntry.(*resilverProvider).xact
	xresilver.Abort()
	xresilver.WaitForFinish()
}

func NewResilver(uuid, kind string) *Resilver {
	return &Resilver{
		RebBase: makeXactRebBase(xaction.XactBaseID(uuid), kind),
	}
}

func (xact *Resilver) IsMountpathXact() bool { return true }

func (xact *Resilver) String() string {
	return xact.RebBase.String()
}

// Election

func (*electionProvider) New(_ registry.XactArgs) registry.GlobalEntry { return &electionProvider{} }

func (p *electionProvider) Start(_ cmn.Bck) error {
	p.xact = &Election{
		XactBase: *xaction.NewXactBase(xaction.XactBaseID(""), cmn.ActElection),
	}
	return nil
}
func (*electionProvider) Kind() string                               { return cmn.ActElection }
func (p *electionProvider) Get() cluster.Xact                        { return p.xact }
func (p *electionProvider) PreRenewHook(_ registry.GlobalEntry) bool { return true }
func (p *electionProvider) PostRenewHook(_ registry.GlobalEntry)     {}

func (e *Election) IsMountpathXact() bool { return false }
