// Package integration contains AIS integration tests.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package integration

import (
	"fmt"
	"net/http"
	"os"
	"path"
	"sync"
	"testing"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/tutils"
	"github.com/NVIDIA/aistore/tutils/tassert"
)

const (
	fshcDetectTimeMax      = time.Second * 20
	fshcDetectTimeDisabled = time.Second * 10
	fshcRunTimeMax         = time.Second * 20
	fshcDir                = "fschecker"
)

type checkerMD struct {
	t          *testing.T
	seed       int64
	numObjs    int
	proxyURL   string
	bck        cmn.Bck
	smap       *cluster.Smap
	mpList     cluster.NodeMap
	allMps     map[*cluster.Snode]*cmn.MountpathList
	origAvail  int
	fileSize   int64
	baseParams api.BaseParams
	chstop     chan struct{}
	chfail     chan struct{}
	wg         *sync.WaitGroup
}

func newCheckerMD(t *testing.T) *checkerMD {
	md := &checkerMD{
		t:        t,
		seed:     baseseed + 300,
		numObjs:  100,
		proxyURL: tutils.RandomProxyURL(),
		bck: cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		},
		fileSize: 64 * cmn.KiB,
		mpList:   make(cluster.NodeMap, 10),
		allMps:   make(map[*cluster.Snode]*cmn.MountpathList, 10),
		chstop:   make(chan struct{}),
		chfail:   make(chan struct{}),
		wg:       &sync.WaitGroup{},
	}

	md.init()

	return md
}

func (md *checkerMD) init() {
	md.baseParams = tutils.BaseAPIParams(md.proxyURL)
	md.smap = tutils.GetClusterMap(md.t, md.proxyURL)

	for targetID, tinfo := range md.smap.Tmap {
		tutils.Logf("Target: %s\n", targetID)
		lst, err := api.GetMountpaths(md.baseParams, tinfo)
		tassert.CheckFatal(md.t, err)
		tutils.Logf("    Mountpaths: %v\n", lst)

		for _, fqn := range lst.Available {
			md.mpList[fqn] = tinfo
		}
		md.allMps[tinfo] = lst

		md.origAvail += len(lst.Available)
	}
}

func (md *checkerMD) randomTargetMpath() (target *cluster.Snode, mpath string, mpathMap *cmn.MountpathList) {
	// select random target and mountpath
	for m, t := range md.mpList {
		target, mpath = t, m
		mpathMap = md.allMps[target]
		break
	}
	return
}

func (md *checkerMD) runTestAsync(method string, target *cluster.Snode, mpath string, mpathList *cmn.MountpathList, suffix string) {
	md.wg.Add(1)
	go runAsyncJob(md.t, md.bck, md.wg, method, mpath, fileNames, md.chfail, md.chstop, suffix)
	// let the job run for a while and then make a mountpath broken
	time.Sleep(2 * time.Second)
	md.chfail <- struct{}{}
	if detected := waitForMountpathChanges(md.t, target, len(mpathList.Available)-1, len(mpathList.Disabled)+1, true); detected {
		// let the job run for a while with broken mountpath, so FSHC detects the trouble
		time.Sleep(2 * time.Second)
		md.chstop <- struct{}{}
	}
	md.wg.Wait()

	repairMountpath(md.t, target, mpath, len(mpathList.Available), len(mpathList.Disabled), suffix)
}

func (md *checkerMD) runTestSync(method string, target *cluster.Snode, mpath string, mpathList *cmn.MountpathList,
	objList []string, suffix string) {
	breakMountpath(md.t, mpath, suffix)
	defer repairMountpath(md.t, target, mpath, len(mpathList.Available), len(mpathList.Disabled), suffix)

	switch method {
	case http.MethodPut:
		p, err := api.HeadBucket(md.baseParams, md.bck)
		tassert.CheckFatal(md.t, err)
		tutils.PutObjsFromList(md.proxyURL, md.bck, fshcDir, uint64(md.fileSize), objList, nil, nil, p.Cksum.Type)
	case http.MethodGet:
		for _, objName := range objList {
			// GetObject must fail - so no error checking
			_, err := api.GetObject(md.baseParams, md.bck, objName)
			if err == nil {
				md.t.Errorf("Get %q must fail", objName)
			}
		}
	}

	if detected := waitForMountpathChanges(md.t, target, len(mpathList.Available)-1, len(mpathList.Disabled)+1, false, fshcDetectTimeDisabled); detected {
		md.t.Error("PUT objects to a broken mountpath should not disable the mountpath when FSHC is disabled")
	}
}

func waitForMountpathChanges(t *testing.T, target *cluster.Snode, availLen, disabledLen int, failIfDiffer bool, timeout ...time.Duration) bool {
	var (
		err        error
		newMpaths  *cmn.MountpathList
		baseParams = tutils.BaseAPIParams()
	)

	maxWaitTime := fshcDetectTimeMax
	if len(timeout) != 0 && timeout[0] != 0 {
		maxWaitTime = timeout[0]
	}

	detectStart := time.Now()
	detectLimit := time.Now().Add(maxWaitTime)

	for detectLimit.After(time.Now()) {
		newMpaths, err = api.GetMountpaths(baseParams, target)
		if err != nil {
			t.Errorf("Failed to read target mountpaths: %v\n", err)
			break
		}
		if len(newMpaths.Disabled) == disabledLen {
			break
		}
		time.Sleep(time.Millisecond * 100)
	}
	detectTime := time.Since(detectStart)
	tutils.Logf("passed %v\n", detectTime)

	if len(newMpaths.Disabled) == disabledLen && len(newMpaths.Available) == availLen {
		tutils.Logf("Check is successful in %v\n", detectTime)
		return true
	}

	if !failIfDiffer {
		return false
	}

	tutils.Logf("Current mpath list: %v\n", newMpaths)
	if len(newMpaths.Disabled) != disabledLen {
		t.Errorf("Disabled mpath count mismatch, old count: %v, new list: %v",
			disabledLen, newMpaths.Disabled)
	} else if len(newMpaths.Available) != availLen {
		t.Errorf("Available mpath count mismatch, old count: %v, new list: %v",
			availLen, newMpaths.Available)
	}
	return false
}

// Simulating mountpath death requested.
// It is the easiest way to simulate: stop putting data and
// replace the mountpath with regular file. If we do not stop
// putting objects it recreates the mountpath and does not fail
func breakMountpath(t *testing.T, mpath, suffix string) {
	os.Rename(mpath, mpath+suffix)
	f, err := os.OpenFile(mpath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Errorf("Failed to create file: %v", err)
	}
	f.Close()
}

func repairMountpath(t *testing.T, target *cluster.Snode, mpath string, availLen, disabledLen int, suffix string) {
	var (
		err        error
		baseParams = tutils.BaseAPIParams()
	)

	// "broken" mpath does no exist, nothing to restore
	if _, err := os.Stat(mpath + suffix); err != nil {
		return
	}
	// cleanup
	// restore original mountpath
	os.Remove(mpath)
	cmn.Rename(mpath+suffix, mpath)

	// ask fschecker to check all mountpath - it should make disabled
	// mountpath back to available list
	api.EnableMountpath(baseParams, target, mpath)
	tutils.Logln("Recheck mountpaths")
	detectStart := time.Now()
	detectLimit := time.Now().Add(fshcDetectTimeMax)
	var mpaths *cmn.MountpathList
	// Wait for fsckeeper detects that the mountpath is accessible now
	for detectLimit.After(time.Now()) {
		mpaths, err = api.GetMountpaths(baseParams, target)
		if err != nil {
			t.Errorf("Failed to read target mountpaths: %v\n", err)
			break
		}
		if len(mpaths.Disabled) == disabledLen && len(mpaths.Available) == availLen {
			break
		}
		time.Sleep(time.Second)
	}

	// final test checks - available and disabled lists must equal list
	// before starting the test
	if len(mpaths.Disabled) != disabledLen {
		t.Errorf("Failed mountpath is still disabled in %v\nExpected disabled count: %d\nNew list:%v\n",
			time.Since(detectStart), disabledLen, mpaths.Disabled)
	} else if len(mpaths.Available) != availLen {
		t.Errorf("Failed mountpath is not back in %v.\nExpected available count: %d\nNew list:%v\n",
			time.Since(detectStart), availLen, mpaths.Available)
	}
}

func runAsyncJob(t *testing.T, bck cmn.Bck, wg *sync.WaitGroup, op, mpath string, filelist []string, chfail,
	chstop chan struct{}, suffix string) {
	defer wg.Done()

	const fileSize = 64 * cmn.KiB
	var (
		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams(proxyURL)
	)

	tutils.Logf("Testing mpath fail detection on %s\n", op)
	stopTime := time.Now().Add(fshcRunTimeMax)

	p, err := api.HeadBucket(baseParams, bck)
	tassert.CheckFatal(t, err)

	for stopTime.After(time.Now()) {
		errCh := make(chan error, len(filelist))
		objsPutCh := make(chan string, len(filelist))

		for _, fname := range filelist {
			select {
			case <-chfail:
				breakMountpath(t, mpath, suffix)
			case <-chstop:
				return
			default:
				// do nothing and just start the next loop
			}

			switch op {
			case "PUT":
				fileList := []string{fname}
				tutils.PutObjsFromList(proxyURL, bck, fshcDir, fileSize, fileList, errCh, objsPutCh, p.Cksum.Type)
			case "GET":
				api.GetObject(baseParams, bck, path.Join(fshcDir, fname))
				time.Sleep(time.Millisecond * 10)
			default:
				t.Errorf("Invalid operation: %s", op)
			}
		}

		close(errCh)
		close(objsPutCh)
	}
}

func TestFSCheckerDetectionEnabled(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{Long: true})

	var (
		md     = newCheckerMD(t)
		suffix = "-" + cmn.RandString(5)
	)

	if md.origAvail == 0 {
		t.Fatal("No available mountpaths found")
	}

	tutils.CreateFreshBucket(t, md.proxyURL, md.bck)
	selectedTarget, selectedMpath, selectedMpathList := md.randomTargetMpath()
	tutils.Logf("mountpath %s of %s is selected for the test\n", selectedMpath, selectedTarget)
	defer func() {
		tutils.DestroyBucket(t, md.proxyURL, md.bck)
		if err := api.RemoveMountpath(md.baseParams, selectedTarget.ID(), selectedMpath); err != nil {
			t.Logf("Failed to remove mpath %s of %s: %v", selectedMpath, selectedTarget, err)
		}
		if err := api.AddMountpath(md.baseParams, selectedTarget, selectedMpath); err != nil {
			t.Logf("Failed to add mpath %s of %s: %v", selectedMpath, selectedTarget, err)
		}

		tutils.WaitForRebalanceToComplete(t, md.baseParams, rebalanceTimeout)
	}()

	// generate some filenames to PUT to them in a loop
	generateRandomNames(md.numObjs)

	// Checking detection on object PUT
	md.runTestAsync(http.MethodPut, selectedTarget, selectedMpath, selectedMpathList, suffix)
	// Checking detection on object GET
	md.runTestAsync(http.MethodGet, selectedTarget, selectedMpath, selectedMpathList, suffix)

	// Checking that reading "bad" objects does not disable mpath if the mpath is OK
	tutils.Logf("Reading non-existing objects: read is expected to fail but mountpath must be available\n")
	for n := 1; n < 10; n++ {
		objName := fmt.Sprintf("%s/o%d", fshcDir, n)
		if _, err := api.GetObject(md.baseParams, md.bck, objName); err == nil {
			t.Error("Should not be able to GET non-existing objects")
		}
	}
	if detected := waitForMountpathChanges(t, selectedTarget, len(selectedMpathList.Available), len(selectedMpathList.Disabled), false); !detected {
		t.Error("GETting non-existing objects should not disable mountpath")
		repairMountpath(t, selectedTarget, selectedMpath, len(selectedMpathList.Available), len(selectedMpathList.Disabled), suffix)
	}
}

func TestFSCheckerDetectionDisabled(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{Long: true})

	var (
		md     = newCheckerMD(t)
		suffix = "-" + cmn.RandString(5)
	)

	if md.origAvail == 0 {
		t.Fatal("No available mountpaths found")
	}

	tutils.Logf("*** Testing with disabled FSHC***\n")
	tutils.SetClusterConfig(t, cmn.SimpleKVs{"fshc.enabled": "false"})
	defer tutils.SetClusterConfig(t, cmn.SimpleKVs{"fshc.enabled": "true"})

	selectedTarget, selectedMpath, selectedMap := md.randomTargetMpath()
	tutils.Logf("mountpath %s of %s is selected for the test\n", selectedMpath, selectedTarget)
	tutils.CreateFreshBucket(t, md.proxyURL, md.bck)
	defer func() {
		tutils.DestroyBucket(t, md.proxyURL, md.bck)
		if err := api.RemoveMountpath(md.baseParams, selectedTarget.ID(), selectedMpath); err != nil {
			t.Logf("Failed to remove mpath %s of %s: %v", selectedMpath, selectedTarget, err)
		}
		if err := api.AddMountpath(md.baseParams, selectedTarget, selectedMpath); err != nil {
			t.Logf("Failed to add mpath %s of %s: %v", selectedMpath, selectedTarget, err)
		}

		tutils.WaitForRebalanceToComplete(t, md.baseParams, rebalanceTimeout)
	}()

	// generate a short list of file to run the test (to avoid flooding the log with false errors)
	objList := make([]string, 0, 5)
	for n := 0; n < 5; n++ {
		objName := fmt.Sprintf("obj-fshc-%d", n)
		objList = append(objList, objName)
	}

	// Checking detection on object PUT
	md.runTestSync(http.MethodPut, selectedTarget, selectedMpath, selectedMap, objList, suffix)
	// Checking detection on object GET
	md.runTestSync(http.MethodGet, selectedTarget, selectedMpath, selectedMap, objList, suffix)

	tutils.WaitForRebalanceToComplete(t, tutils.BaseAPIParams(md.proxyURL), rebalanceTimeout)
}

func TestFSCheckerEnablingMpath(t *testing.T) {
	var (
		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams(proxyURL)
		smap       = tutils.GetClusterMap(t, proxyURL)
		mpList     = make(cluster.NodeMap, 10)
		origAvail  = 0
	)

	for targetID, tinfo := range smap.Tmap {
		tutils.Logf("Target: %s\n", targetID)
		lst, err := api.GetMountpaths(baseParams, tinfo)
		tassert.CheckFatal(t, err)
		tutils.Logf("    Mountpaths: %v\n", lst)

		for _, fqn := range lst.Available {
			mpList[fqn] = tinfo
		}

		origAvail += len(lst.Available)
	}

	if origAvail == 0 {
		t.Fatal("No available mountpaths found")
	}

	// select random target and mountpath
	var (
		selectedTarget *cluster.Snode
		selectedMpath  string
	)
	for m, t := range mpList {
		selectedTarget, selectedMpath = t, m
		break
	}

	err := api.EnableMountpath(baseParams, selectedTarget, selectedMpath)
	if err != nil {
		t.Errorf("Enabling available mountpath should return success, got: %v", err)
	}

	err = api.EnableMountpath(baseParams, selectedTarget, selectedMpath+"some_text")
	if err == nil {
		t.Errorf("Enabling non-existing mountpath should return error")
	} else {
		status := api.HTTPStatus(err)
		if status != http.StatusNotFound {
			t.Errorf("Expected status %d, got %d, %v", http.StatusNotFound, status, err)
		}
	}
}

func TestFSCheckerTargetDisable(t *testing.T) {
	var (
		target *cluster.Snode

		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams()
		smap       = tutils.GetClusterMap(t, proxyURL)
		proxyCnt   = smap.CountProxies()
		targetCnt  = smap.CountTargets()
	)

	if targetCnt < 2 {
		t.Skip("The number of targets must be at least 2")
	}

	for _, tinfo := range smap.Tmap {
		target = tinfo
		break
	}
	oldMpaths, err := api.GetMountpaths(baseParams, target)
	tassert.CheckFatal(t, err)
	if len(oldMpaths.Available) == 0 {
		t.Fatalf("Target %s does not have available mountpaths", target)
	}

	tutils.Logf("Removing all mountpaths from target: %s\n", target)
	for _, mpath := range oldMpaths.Available {
		err = api.DisableMountpath(baseParams, target.ID(), mpath)
		tassert.CheckFatal(t, err)
	}

	smap, err = tutils.WaitForPrimaryProxy(proxyURL, "all mpath disabled", smap.Version, false, proxyCnt, targetCnt-1)
	tassert.CheckFatal(t, err)

	tutils.Logf("Restoring target %s mountpaths\n", target.ID())
	for _, mpath := range oldMpaths.Available {
		err = api.EnableMountpath(baseParams, target, mpath)
		tassert.CheckFatal(t, err)
	}

	_, err = tutils.WaitForPrimaryProxy(proxyURL, "all mpath enabled", smap.Version, false, proxyCnt, targetCnt)
	tassert.CheckFatal(t, err)
}
