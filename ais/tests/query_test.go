// Package integration contains AIS integration tests.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package integration

import (
	"bytes"
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/query"
	"github.com/NVIDIA/aistore/tutils"
	"github.com/NVIDIA/aistore/tutils/tassert"
	jsoniter "github.com/json-iterator/go"
)

func checkQueryDone(t *testing.T, handle string) {
	baseParams = tutils.BaseAPIParams(proxyURL)

	_, err := api.NextQueryResults(baseParams, handle, 1)
	tassert.Fatalf(t, err != nil, "expected an error to occur")
	httpErr, ok := err.(*cmn.HTTPError)
	tassert.Fatalf(t, ok, "expected the error to be an http error")
	tassert.Errorf(t, httpErr.Status == http.StatusGone, "expected 410 on finished query")
}

func TestQueryBck(t *testing.T) {
	var (
		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams(proxyURL)
		bck        = cmn.Bck{
			Name:     "TESTQUERYBUCKET",
			Provider: cmn.ProviderAIS,
		}
		numObjects       = 1000
		chunkSize        = 50
		queryObjectNames = make(cmn.StringSet, numObjects-1)
	)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	putObjects := tutils.PutRR(t, baseParams, cmn.KiB, cmn.ChecksumNone, bck, "", numObjects, fnlen)
	sort.Strings(putObjects)
	for _, obj := range putObjects {
		queryObjectNames.Add(obj)
	}

	handle, err := api.InitQuery(baseParams, "", bck, nil)
	tassert.CheckFatal(t, err)

	objectsLeft := numObjects
	for objectsLeft > 0 {
		var (
			// Get random proxy for next request to simulate load balancer.
			baseParams   = tutils.BaseAPIParams()
			requestedCnt = cmn.Min(chunkSize, objectsLeft)
		)
		objects, err := api.NextQueryResults(baseParams, handle, uint(requestedCnt))
		tassert.CheckFatal(t, err)
		tassert.Fatalf(t, len(objects) == requestedCnt, "expected %d to be returned, got %d", requestedCnt, len(objects))
		objectsLeft -= requestedCnt

		for _, object := range objects {
			tassert.Fatalf(t, queryObjectNames.Contains(object.Name), "unexpected object %s", object.Name)
			queryObjectNames.Delete(object.Name)
		}
	}

	checkQueryDone(t, handle)
}

func TestQueryVersionFilter(t *testing.T) {
	var (
		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams(proxyURL)
		bck        = cmn.Bck{
			Name:     "TESTQUERYBUCKET",
			Provider: cmn.ProviderAIS,
		}
		numObjects       = 10
		queryObjectNames = make(cmn.StringSet, numObjects-1)
	)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	objName := fmt.Sprintf("object-%d.txt", 0)
	putRandomFile(t, baseParams, bck, objName, cmn.KiB)
	// increase version of object-0.txt, so filter should discard it
	putRandomFile(t, baseParams, bck, objName, cmn.KiB)
	for i := 1; i < numObjects; i++ {
		objName = fmt.Sprintf("object-%d.txt", i)
		queryObjectNames.Add(objName)
		putRandomFile(t, baseParams, bck, objName, cmn.KiB)
	}

	filter := query.VersionLEFilterMsg(1)
	handle, err := api.InitQuery(baseParams, "object-{0..100}.txt", bck, filter)
	tassert.CheckFatal(t, err)

	objectsNames, err := api.NextQueryResults(baseParams, handle, uint(numObjects))
	tassert.CheckFatal(t, err)
	tassert.Fatalf(t, len(objectsNames) == numObjects-1, "expected %d to be returned, got %d", numObjects-1, len(objectsNames))
	for _, object := range objectsNames {
		tassert.Errorf(t, queryObjectNames.Contains(object.Name), "unexpected object %s", objName)
		queryObjectNames.Delete(object.Name)
	}

	checkQueryDone(t, handle)
}

func TestQueryVersionAndAtime(t *testing.T) {
	var (
		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams(proxyURL)
		bck        = cmn.Bck{
			Name:     "TESTQUERYBUCKET",
			Provider: cmn.ProviderAIS,
		}
		numObjects       = 10
		queryObjectNames = make(cmn.StringSet, numObjects-1)
	)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	objName := fmt.Sprintf("object-%d.txt", 0)
	putRandomFile(t, baseParams, bck, objName, cmn.KiB)
	// increase version of object-0.txt, so filter should discard it
	putRandomFile(t, baseParams, bck, objName, cmn.KiB)
	for i := 1; i < numObjects; i++ {
		objName = fmt.Sprintf("object-%d.txt", i)
		queryObjectNames.Add(objName)
		putRandomFile(t, baseParams, bck, objName, cmn.KiB)
	}

	timestamp := time.Now()

	// object with Atime > timestamp
	objName = fmt.Sprintf("object-%d.txt", numObjects+1)
	putRandomFile(t, baseParams, bck, objName, cmn.KiB)

	filter := query.NewAndFilter(query.VersionLEFilterMsg(1), query.ATimeBeforeFilterMsg(timestamp))
	handle, err := api.InitQuery(baseParams, "object-{0..100}.txt", bck, filter)
	tassert.CheckFatal(t, err)

	objectsNames, err := api.NextQueryResults(baseParams, handle, uint(numObjects))
	tassert.CheckFatal(t, err)
	tassert.Fatalf(t, len(objectsNames) == numObjects-1, "expected %d to be returned, got %d", numObjects-1, len(objectsNames))
	for _, object := range objectsNames {
		tassert.Errorf(t, queryObjectNames.Contains(object.Name), "unexpected object %s", objName)
		queryObjectNames.Delete(object.Name)
	}

	checkQueryDone(t, handle)
}

func TestQueryWorkersTargets(t *testing.T) {
	var (
		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams(proxyURL)
		bck        = cmn.Bck{
			Name:     "TESTQUERYWORKERBUCKET",
			Provider: cmn.ProviderAIS,
		}
		smapDaemonIDs = cmn.StringSet{}
		objName       = "object.txt"
	)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	smap, err := api.GetClusterMap(baseParams)
	tassert.CheckError(t, err)
	for _, t := range smap.Tmap {
		smapDaemonIDs.Add(t.DaemonID)
	}

	putRandomFile(t, baseParams, bck, objName, cmn.KiB)
	handle, err := api.InitQuery(baseParams, "", bck, nil, uint(smap.CountTargets()))
	tassert.CheckFatal(t, err)
	for i := 1; i <= smap.CountTargets(); i++ {
		daemonID, err := api.QueryWorkerTarget(baseParams, handle, uint(i))
		tassert.CheckFatal(t, err)
		tassert.Errorf(t, smapDaemonIDs.Contains(daemonID), "unexpected daemonID %s", daemonID)
		smapDaemonIDs.Delete(daemonID)
	}

	entries, err := api.NextQueryResults(baseParams, handle, 1)
	tassert.CheckFatal(t, err)
	tassert.Fatalf(t, len(entries) == 1, "expected query to have 1 object, got %d", len(entries))
	tassert.Errorf(t, entries[0].Name == objName, "expected object name to be %q, got %q", objName, entries[0].Name)
}

func TestQueryWorkersTargetDown(t *testing.T) {
	var (
		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams(proxyURL)
		bck        = cmn.Bck{
			Name:     "TESTQUERYWORKERBUCKET",
			Provider: cmn.ProviderAIS,
		}
		smapDaemonIDs = cmn.StringSet{}
		objName       = "object.txt"
	)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	smap, err := api.GetClusterMap(baseParams)
	tassert.CheckError(t, err)
	for _, t := range smap.Tmap {
		smapDaemonIDs.Add(t.DaemonID)
	}

	putRandomFile(t, baseParams, bck, objName, cmn.KiB)
	handle, err := api.InitQuery(baseParams, "", bck, nil, uint(smap.CountTargets()))
	tassert.CheckFatal(t, err)

	_, err = api.QueryWorkerTarget(baseParams, handle, 1)
	tassert.CheckFatal(t, err)

	target, err := smap.GetRandTarget()
	tassert.CheckFatal(t, err)
	err = tutils.UnregisterNode(proxyURL, target.DaemonID)
	tassert.CheckError(t, err)

	smap, err = tutils.WaitForPrimaryProxy(
		proxyURL,
		"target is gone",
		smap.Version, testing.Verbose(),
		smap.CountProxies(),
		smap.CountTargets()-1,
	)
	tassert.CheckError(t, err)
	defer func() {
		tutils.RegisterNode(proxyURL, target, smap)
		tutils.WaitForRebalanceToComplete(t, baseParams, rebalanceTimeout)
	}()

	_, err = api.QueryWorkerTarget(baseParams, handle, 1)
	tassert.Errorf(t, err != nil, "expected error to occur when target went down")
}

func TestQuerySingleWorkerNext(t *testing.T) {
	var (
		baseParams = tutils.BaseAPIParams()
		m          = ioContext{
			t:        t,
			num:      100,
			fileSize: 5 * cmn.KiB,
		}
	)

	m.init()
	tutils.CreateFreshBucket(t, m.proxyURL, m.bck)
	defer tutils.DestroyBucket(t, m.proxyURL, m.bck)

	m.puts()

	smap, err := api.GetClusterMap(baseParams)
	tassert.CheckError(t, err)

	handle, err := api.InitQuery(baseParams, "", m.bck, nil, uint(smap.CountTargets()))
	tassert.CheckFatal(t, err)

	si, err := smap.GetRandTarget()
	tassert.CheckFatal(t, err)
	baseParams.URL = si.URL(cmn.NetworkPublic)

	buf := bytes.NewBuffer(nil)
	err = api.DoHTTPRequest(api.ReqParams{
		BaseParams: baseParams,
		Path:       cmn.JoinWords(cmn.Version, cmn.Query, cmn.Next),
		Body:       cmn.MustMarshal(query.NextMsg{Handle: handle, Size: 10}),
	}, buf)
	tassert.CheckFatal(t, err)

	objList := &cmn.BucketList{}
	err = jsoniter.Unmarshal(buf.Bytes(), objList)
	tassert.CheckFatal(t, err)
}
