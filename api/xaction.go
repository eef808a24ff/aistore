// Package api provides RESTful API to AIS object storage
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/nl"
	"github.com/NVIDIA/aistore/xaction"
)

const (
	xactRetryInterval = time.Second
)

type (
	NodesXactStat       map[string]*xaction.BaseXactStatsExt
	NodesXactMultiStats map[string][]*xaction.BaseXactStatsExt

	XactStatsHelper interface {
		Running() bool
		Finished() bool
		Aborted() bool
		ObjCount() int64
	}

	XactReqArgs struct {
		ID      string
		Kind    string    // Xaction kind, see: cmn.XactsDtor
		Bck     cmn.Bck   // Optional bucket
		Buckets []cmn.Bck // Optional: Xaction on list of buckets
		Timeout time.Duration
		Force   bool // Optional: force LRU
		Latest  bool // Determines if we should get latest or all xactions
	}
)

func (xs NodesXactStat) Running() bool {
	for _, stat := range xs {
		if stat.Running() {
			return true
		}
	}
	return false
}

func (xs NodesXactStat) Finished() bool { return !xs.Running() }

func (xs NodesXactStat) Aborted() bool {
	for _, stat := range xs {
		if stat.Aborted() {
			return true
		}
	}
	return false
}

func (xs NodesXactStat) ObjCount() (count int64) {
	for _, stat := range xs {
		count += stat.ObjCount()
	}
	return
}

func (xs NodesXactStat) BytesCount() (count int64) {
	for _, stat := range xs {
		count += stat.BytesCount()
	}
	return
}

func (xs NodesXactMultiStats) Running() bool {
	for _, targetStats := range xs {
		for _, xaction := range targetStats {
			if xaction.Running() {
				return true
			}
		}
	}
	return false
}

func (xs NodesXactMultiStats) Finished() bool { return !xs.Running() }

func (xs NodesXactMultiStats) Aborted() bool {
	for _, targetStats := range xs {
		for _, xaction := range targetStats {
			if xaction.Aborted() {
				return true
			}
		}
	}
	return false
}

func (xs NodesXactMultiStats) ObjCount() (count int64) {
	for _, targetStats := range xs {
		for _, xaction := range targetStats {
			count += xaction.ObjCount()
		}
	}
	return
}

func (xs NodesXactMultiStats) GetNodesXactStat(id string) (xactStat NodesXactStat) {
	xactStat = make(NodesXactStat)
	for target, stats := range xs {
		for _, stat := range stats {
			if stat.ID() == id {
				xactStat[target] = stat
				break
			}
		}
	}
	return
}

// StartXaction starts a given xaction.
func StartXaction(baseParams BaseParams, args XactReqArgs) (id string, err error) {
	if !xaction.XactsDtor[args.Kind].Startable {
		return id, fmt.Errorf("cannot start \"kind=%s\" xaction", args.Kind)
	}

	xactMsg := xaction.XactReqMsg{
		Kind: args.Kind,
		Bck:  args.Bck,
	}

	if args.Buckets != nil {
		xactMsg.Buckets = args.Buckets
		xactMsg.Force = Bool(args.Force)
	}

	msg := cmn.ActionMsg{
		Action: cmn.ActXactStart,
		Value:  xactMsg,
	}

	baseParams.Method = http.MethodPut
	err = DoHTTPRequest(ReqParams{
		BaseParams: baseParams,
		Path:       cmn.JoinWords(cmn.Version, cmn.Cluster),
		Body:       cmn.MustMarshal(msg),
		Query:      cmn.AddBckToQuery(nil, args.Bck),
	}, &id)
	return id, err
}

// AbortXaction aborts a given xaction.
func AbortXaction(baseParams BaseParams, args XactReqArgs) error {
	msg := cmn.ActionMsg{
		Action: cmn.ActXactStop,
		Value: xaction.XactReqMsg{
			ID:   args.ID,
			Kind: args.Kind,
			Bck:  args.Bck,
		},
	}
	baseParams.Method = http.MethodPut
	return DoHTTPRequest(ReqParams{
		BaseParams: baseParams,
		Path:       cmn.JoinWords(cmn.Version, cmn.Cluster),
		Body:       cmn.MustMarshal(msg),
		Query:      cmn.AddBckToQuery(nil, args.Bck),
	})
}

// GetXactionStatsByID gets all xaction stats for given id.
func GetXactionStatsByID(baseParams BaseParams, id string) (xactStat NodesXactStat, err error) {
	xactStats, err := QueryXactionStats(baseParams, XactReqArgs{ID: id})
	if err != nil {
		return
	}
	xactStat = xactStats.GetNodesXactStat(id)
	return
}

// QueryXactionStats gets all xaction stats for given kind and bucket (optional).
func QueryXactionStats(baseParams BaseParams, args XactReqArgs) (xactStats NodesXactMultiStats, err error) {
	msg := xaction.XactReqMsg{
		ID:   args.ID,
		Kind: args.Kind,
		Bck:  args.Bck,
	}
	if args.Latest {
		msg.OnlyRunning = Bool(true)
	}
	baseParams.Method = http.MethodGet
	err = DoHTTPRequest(ReqParams{
		BaseParams: baseParams,
		Path:       cmn.JoinWords(cmn.Version, cmn.Cluster),
		Body:       cmn.MustMarshal(msg),
		Query:      url.Values{cmn.URLParamWhat: []string{cmn.QueryXactStats}},
	}, &xactStats)
	return xactStats, err
}

// GetXactionStatus retrieves the status of the xaction.
func GetXactionStatus(baseParams BaseParams, args XactReqArgs) (status *nl.NotifStatus, err error) {
	baseParams.Method = http.MethodGet
	msg := xaction.XactReqMsg{
		ID:   args.ID,
		Kind: args.Kind,
		Bck:  args.Bck,
	}
	if args.Latest {
		msg.OnlyRunning = Bool(true)
	}

	status = &nl.NotifStatus{}
	err = DoHTTPRequest(ReqParams{
		BaseParams: baseParams,
		Path:       cmn.JoinWords(cmn.Version, cmn.Cluster),
		Body:       cmn.MustMarshal(msg),
		Query: url.Values{
			cmn.URLParamWhat: []string{cmn.GetWhatStatus},
		},
	}, status)
	return
}

// WaitForXaction waits for a given xaction to complete.
func WaitForXaction(baseParams BaseParams, args XactReqArgs,
	refreshIntervals ...time.Duration) (status *nl.NotifStatus, err error) {
	var (
		ctx           = context.Background()
		retryInterval = xactRetryInterval
	)

	if len(refreshIntervals) > 0 {
		retryInterval = refreshIntervals[0]
	}

	if args.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, args.Timeout)
		defer cancel()
	}

	for {
		status, err = GetXactionStatus(baseParams, args)
		if err != nil || status.Finished() {
			return
		}

		time.Sleep(retryInterval)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			break
		}
	}
}

// WaitForXactionToStart waits for a given xaction to start.
func WaitForXactionToStart(baseParams BaseParams, args XactReqArgs) error {
	ctx := context.Background()
	if args.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, args.Timeout)
		defer cancel()
	}

	for {
		xactStats, err := QueryXactionStats(baseParams, args)
		if err != nil {
			return err
		}
		if xactStats.Running() {
			break
		}
		if len(xactStats) > 0 && xactStats.Finished() {
			break
		}

		time.Sleep(xactRetryInterval)
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			break
		}
	}
	return nil
}

// MakeNCopies starts an extended action (xaction) to bring a given bucket to a
// certain redundancy level (num copies).
func MakeNCopies(baseParams BaseParams, bck cmn.Bck, copies int) (xactID string, err error) {
	baseParams.Method = http.MethodPost
	err = DoHTTPRequest(ReqParams{
		BaseParams: baseParams,
		Path:       cmn.JoinWords(cmn.Version, cmn.Buckets, bck.Name),
		Body:       cmn.MustMarshal(cmn.ActionMsg{Action: cmn.ActMakeNCopies, Value: copies}),
	}, &xactID)
	return
}
