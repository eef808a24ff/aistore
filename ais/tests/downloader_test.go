// Package integration contains AIS integration tests.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package integration

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/downloader"
	"github.com/NVIDIA/aistore/tutils"
	"github.com/NVIDIA/aistore/tutils/readers"
	"github.com/NVIDIA/aistore/tutils/tassert"
)

// NOTE: TestDownload* can fail if link, content, version changes - should be super rare!

const (
	downloadDescAllPrefix = "downloader-test-integration"
	downloadDescAllRegex  = "^" + downloadDescAllPrefix
)

var downloadDescCurPrefix = fmt.Sprintf("%s-%d-", downloadDescAllPrefix, os.Getpid())

func generateDownloadDesc() string {
	return downloadDescCurPrefix + time.Now().Format(time.RFC3339Nano)
}

func clearDownloadList(t *testing.T) {
	listDownload, err := api.DownloadGetList(tutils.BaseAPIParams(), downloadDescAllRegex)
	tassert.CheckFatal(t, err)

	if len(listDownload) == 0 {
		return
	}

	for _, v := range listDownload {
		if v.JobRunning() {
			tutils.Logf("Canceling: %v...\n", v.ID)
			err := api.AbortDownload(tutils.BaseAPIParams(), v.ID)
			tassert.CheckFatal(t, err)
		}
	}

	// Wait for the jobs to complete.
	for running := true; running; {
		time.Sleep(time.Second)
		listDownload, err := api.DownloadGetList(tutils.BaseAPIParams(), downloadDescAllRegex)
		tassert.CheckFatal(t, err)

		running = false
		for _, v := range listDownload {
			if v.JobRunning() {
				running = true
				break
			}
		}
	}

	for _, v := range listDownload {
		tutils.Logf("Removing: %v...\n", v.ID)
		err := api.RemoveDownload(tutils.BaseAPIParams(), v.ID)
		tassert.CheckFatal(t, err)
	}
}

func checkDownloadList(t *testing.T, expNumEntries ...int) {
	defer clearDownloadList(t)

	expNumEntriesVal := 1
	if len(expNumEntries) > 0 {
		expNumEntriesVal = expNumEntries[0]
	}

	listDownload, err := api.DownloadGetList(tutils.BaseAPIParams(), downloadDescAllRegex)
	tassert.CheckFatal(t, err)
	actEntries := len(listDownload)

	if expNumEntriesVal != actEntries {
		t.Fatalf("Incorrect # of downloader entries: expected %d, actual %d", expNumEntriesVal, actEntries)
	}
}

func waitForDownload(t *testing.T, id string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)

	for {
		if time.Now().After(deadline) {
			t.Errorf("Timed out waiting for download %s.", id)
			return
		}

		all := true
		if resp, err := api.DownloadStatus(tutils.BaseAPIParams(), id, true); err == nil {
			if !resp.JobFinished() {
				all = false
			}
		}

		if all {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func checkDownloadedObjects(t *testing.T, id string, bck cmn.Bck, objects []string) {
	var (
		proxyURL   = tutils.RandomProxyURL(t)
		baseParams = tutils.BaseAPIParams(proxyURL)
	)
	resp, err := api.DownloadStatus(baseParams, id)
	tassert.CheckFatal(t, err)
	tassert.Errorf(
		t, resp.FinishedCnt == len(objects),
		"finished task mismatch (got: %d, expected: %d)",
		resp.FinishedCnt, len(objects),
	)

	objs, err := tutils.ListObjectNames(proxyURL, bck, "", 0)
	tassert.CheckFatal(t, err)
	tassert.Errorf(
		t, reflect.DeepEqual(objs, objects),
		"objects mismatch (got: %+v, expected: %+v)", objs, objects,
	)
}

func downloaderCompleted(t *testing.T, targetID string, targetsStats api.NodesXactMultiStats) bool {
	downloaderStat, exists := targetsStats[targetID]
	for _, xaction := range downloaderStat {
		if xaction.Running() {
			tutils.Logf("%s in progress for %s\n", xaction.Kind(), targetID)
			return false
		}
	}

	tassert.Fatalf(t, exists, "Target %s not found in downloader stats", targetID)
	return true
}

func waitForDownloaderToFinish(t *testing.T, baseParams api.BaseParams, targetID string, timeouts ...time.Duration) {
	start := time.Now()
	timeout := time.Duration(0)
	if len(timeouts) > 0 {
		timeout = timeouts[0]
	}

	tutils.Logf("Waiting %s for downloader to finish\n", timeout)
	time.Sleep(time.Second * 2)

	xactArgs := api.XactReqArgs{Kind: cmn.ActDownload}
	for {
		time.Sleep(time.Second)
		downloaderStats, err := api.QueryXactionStats(baseParams, xactArgs)
		tassert.CheckFatal(t, err)

		if downloaderCompleted(t, targetID, downloaderStats) {
			tutils.Logf("Downloader has finished\n")
			return
		}

		if timeout.Nanoseconds() != 0 && time.Since(start) > timeout {
			tassert.Fatalf(t, false, "downloader has not finished before %s", timeout)
			return
		}
	}
}

func downloadObject(t *testing.T, bck cmn.Bck, objName, link string, shouldBeSkipped bool) {
	id, err := api.DownloadSingle(tutils.BaseAPIParams(), generateDownloadDesc(), bck, objName, link)
	tassert.CheckError(t, err)
	waitForDownload(t, id, time.Minute)
	status, err := api.DownloadStatus(tutils.BaseAPIParams(), id)
	tassert.CheckFatal(t, err)
	tassert.Errorf(t, (status.SkippedCnt == 1) == shouldBeSkipped, "expected object to be [skipped: %t]", shouldBeSkipped)
}

func downloadObjectCloud(t *testing.T, body downloader.DlCloudBody, expectedFinished, expectedSkipped int) {
	baseParams := tutils.BaseAPIParams()
	body.Description = generateDownloadDesc()
	id, err := api.DownloadWithParam(baseParams, downloader.DlTypeCloud, body)
	tassert.CheckFatal(t, err)

	waitForDownload(t, id, 2*time.Minute)

	resp, err := api.DownloadStatus(baseParams, id)
	tassert.CheckFatal(t, err)
	tassert.Errorf(t, resp.FinishedCnt == expectedFinished, "finished objects mismatch (got: %d, expected: %d)", resp.FinishedCnt, expectedFinished)
	tassert.Errorf(t, resp.SkippedCnt == expectedSkipped, "skipped objects mismatch (got: %d, expected: %d)", resp.SkippedCnt, expectedSkipped)
}

func abortDownload(t *testing.T, id string) {
	baseParams := tutils.BaseAPIParams()

	err := api.AbortDownload(baseParams, id)
	tassert.CheckFatal(t, err)

	waitForDownload(t, id, 30*time.Second)

	status, err := api.DownloadStatus(baseParams, id)
	tassert.CheckFatal(t, err)
	tassert.Fatalf(t, status.Aborted, "download was not marked aborted")
	tassert.Fatalf(t, status.JobFinished(), "download should be finished")
	tassert.Fatalf(t, len(status.CurrentTasks) == 0, "current tasks should be empty")
}

func verifyProps(t *testing.T, bck cmn.Bck, objName string, size int64, version string) *cmn.ObjectProps {
	objProps, err := api.HeadObject(tutils.BaseAPIParams(), bck, objName)
	tassert.CheckFatal(t, err)

	tassert.Errorf(
		t, objProps.Size == size,
		"size mismatch (%d vs %d)", objProps.Size, size,
	)
	tassert.Errorf(
		t, objProps.Version == version,
		"version mismatch (%s vs %s)", objProps.Version, version,
	)
	return objProps
}

func TestDownloadSingle(t *testing.T) {
	var (
		bck = cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		}
		proxyURL      = tutils.RandomProxyURL(t)
		baseParams    = tutils.BaseAPIParams(proxyURL)
		objName       = "object"
		objNameSecond = "object-second"

		// Links below don't contain protocols to test that no error occurs
		// in case they are missing.
		linkLarge = "storage.googleapis.com/nvdata-openimages/openimages-train-000001.tar"
		linkSmall = "storage.googleapis.com/minikube/iso/minikube-v0.23.0.iso.sha256"
	)

	clearDownloadList(t)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	id, err := api.DownloadSingle(baseParams, generateDownloadDesc(), bck, objName, linkLarge)
	tassert.CheckError(t, err)

	time.Sleep(time.Second)

	// Schedule second object.
	idSecond, err := api.DownloadSingle(baseParams, generateDownloadDesc(), bck, objNameSecond, linkLarge)
	tassert.CheckError(t, err)

	// Cancel second object.
	err = api.AbortDownload(baseParams, idSecond)
	tassert.CheckError(t, err)

	// Cancel first object.
	abortDownload(t, id)

	time.Sleep(time.Second)

	// Check if the status is still available after some time.
	if resp, err := api.DownloadStatus(baseParams, id); err != nil {
		t.Errorf("got error when getting status for link that is not being downloaded: %v", err)
	} else if !resp.Aborted {
		t.Errorf("canceled link not marked: %v", resp)
	}

	err = api.AbortDownload(baseParams, id)
	tassert.CheckError(t, err)

	err = api.RemoveDownload(baseParams, id)
	tassert.CheckError(t, err)

	err = api.RemoveDownload(baseParams, id)
	tassert.Errorf(t, err != nil, "expected error when removing non-existent task")

	id, err = api.DownloadSingle(baseParams, generateDownloadDesc(), bck, objName, linkSmall)
	tassert.CheckError(t, err)

	waitForDownload(t, id, 30*time.Second)
	checkDownloadedObjects(t, id, bck, []string{objName})

	checkDownloadList(t, 2)
}

func TestDownloadRange(t *testing.T) {
	var (
		bck = cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		}
		proxyURL   = tutils.RandomProxyURL(t)
		baseParams = tutils.BaseAPIParams(proxyURL)

		template        = "storage.googleapis.com/minikube/iso/minikube-v0.23.{0..4}.iso.sha256"
		expectedObjects = []string{
			"minikube-v0.23.0.iso.sha256",
			"minikube-v0.23.1.iso.sha256",
			"minikube-v0.23.2.iso.sha256",
			"minikube-v0.23.3.iso.sha256",
			"minikube-v0.23.4.iso.sha256",
		}
	)

	clearDownloadList(t)

	// Create ais bucket
	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	id, err := api.DownloadRange(baseParams, generateDownloadDesc(), bck, template)
	tassert.CheckFatal(t, err)

	waitForDownload(t, id, 10*time.Second)
	checkDownloadedObjects(t, id, bck, expectedObjects)

	checkDownloadList(t)
}

func TestDownloadMultiRange(t *testing.T) {
	var (
		bck = cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		}
		proxyURL   = tutils.RandomProxyURL(t)
		baseParams = tutils.BaseAPIParams(proxyURL)

		template        = "storage.googleapis.com/minikube/iso/minikube-v0.{23..25..2}.{0..1}.iso.sha256"
		expectedObjects = []string{
			"minikube-v0.23.0.iso.sha256",
			"minikube-v0.23.1.iso.sha256",
			"minikube-v0.25.0.iso.sha256",
			"minikube-v0.25.1.iso.sha256",
		}
	)

	clearDownloadList(t)

	// Create ais bucket
	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	id, err := api.DownloadRange(baseParams, generateDownloadDesc(), bck, template)
	tassert.CheckFatal(t, err)

	waitForDownload(t, id, 10*time.Second)
	checkDownloadedObjects(t, id, bck, expectedObjects)

	checkDownloadList(t)
}

func TestDownloadMultiMap(t *testing.T) {
	var (
		bck = cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		}
		m = map[string]string{
			"ais": "https://raw.githubusercontent.com/NVIDIA/aistore/master/README.md",
			"k8s": "https://raw.githubusercontent.com/kubernetes/kubernetes/master/README.md",
		}
		expectedObjects = []string{"ais", "k8s"}
		proxyURL        = tutils.RandomProxyURL(t)
	)

	clearDownloadList(t)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	id, err := api.DownloadMulti(tutils.BaseAPIParams(), generateDownloadDesc(), bck, m)
	tassert.CheckFatal(t, err)

	waitForDownload(t, id, 10*time.Second)
	checkDownloadedObjects(t, id, bck, expectedObjects)

	checkDownloadList(t)
}

func TestDownloadMultiList(t *testing.T) {
	var (
		bck = cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		}
		l = []string{
			"https://raw.githubusercontent.com/NVIDIA/aistore/master/README.md",
			"https://raw.githubusercontent.com/kubernetes/kubernetes/master/LICENSE?query=values",
		}
		expectedObjs = []string{"LICENSE", "README.md"}
		proxyURL     = tutils.RandomProxyURL(t)
		baseParams   = tutils.BaseAPIParams(proxyURL)
	)

	clearDownloadList(t)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	id, err := api.DownloadMulti(baseParams, generateDownloadDesc(), bck, l)
	tassert.CheckFatal(t, err)

	waitForDownload(t, id, 10*time.Second)
	checkDownloadedObjects(t, id, bck, expectedObjs)

	checkDownloadList(t)
}

func TestDownloadTimeout(t *testing.T) {
	var (
		bck = cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		}
		objName    = "object"
		link       = "https://storage.googleapis.com/nvdata-openimages/openimages-train-000001.tar"
		proxyURL   = tutils.RandomProxyURL(t)
		baseParams = tutils.BaseAPIParams(proxyURL)
	)

	clearDownloadList(t)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	body := downloader.DlSingleBody{
		DlSingleObj: downloader.DlSingleObj{
			ObjName: objName,
			Link:    link,
		},
	}
	body.Bck = bck
	body.Description = generateDownloadDesc()
	body.Timeout = "1ms" // super small timeout to see if the request will be canceled

	id, err := api.DownloadWithParam(baseParams, downloader.DlTypeSingle, body)
	tassert.CheckFatal(t, err)

	time.Sleep(time.Second)

	status, err := api.DownloadStatus(baseParams, id)
	tassert.CheckFatal(t, err)

	objErr := status.Errs[0]
	tassert.Fatalf(t, status.ErrorCnt == 1, "expected task to be marked as an error")
	tassert.Errorf(
		t, objErr.Name == objName,
		"unexpected name for the object (expected: %q, got: %q)", objName, objErr.Name,
	)
	tassert.Errorf(
		t, strings.Contains(objErr.Err, "deadline exceeded") || strings.Contains(objErr.Err, "timeout"),
		"error mismatch (expected: %q, got: %q)", context.DeadlineExceeded, objErr.Err,
	)

	checkDownloadedObjects(t, id, bck, []string{})

	checkDownloadList(t)
}

func TestDownloadCloud(t *testing.T) {
	var (
		proxyURL   = tutils.RandomProxyURL(t)
		baseParams = tutils.BaseAPIParams(proxyURL)

		fileCnt = 5
		prefix  = "imagenet/imagenet_train-"
		suffix  = ".tgz"
	)

	tests := []struct {
		name   string
		srcBck cmn.Bck
		dstBck cmn.Bck
	}{
		{
			name:   "src==dst",
			srcBck: cliBck,
			dstBck: cliBck,
		},
		{
			name:   "src!=dst",
			srcBck: cliBck,
			dstBck: cmn.Bck{
				Name:     cmn.RandString(5),
				Provider: cmn.ProviderAIS,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tutils.CheckSkip(t, tutils.SkipTestArgs{Long: true, Cloud: true, Bck: test.srcBck})

			clearDownloadList(t)

			if test.dstBck.IsAIS() {
				tutils.CreateFreshBucket(t, proxyURL, test.dstBck)
				defer tutils.DestroyBucket(t, proxyURL, test.dstBck)
			}

			tutils.CleanupCloudBucket(t, proxyURL, test.srcBck, prefix)
			defer tutils.CleanupCloudBucket(t, proxyURL, test.srcBck, prefix)

			tutils.Logln("putting objects into cloud bucket...")

			expectedObjs := make([]string, 0, fileCnt)
			for i := 0; i < fileCnt; i++ {
				reader, err := readers.NewRandReader(256, cmn.ChecksumNone)
				tassert.CheckFatal(t, err)

				objName := fmt.Sprintf("%s%0*d%s", prefix, 5, i, suffix)
				err = api.PutObject(api.PutObjectArgs{
					BaseParams: baseParams,
					Bck:        test.srcBck,
					Object:     objName,
					Reader:     reader,
				})
				tassert.CheckFatal(t, err)

				expectedObjs = append(expectedObjs, objName)
			}

			tutils.Logln("evicting cloud bucket...")
			xactID, err := api.EvictList(baseParams, test.srcBck, expectedObjs)
			tassert.CheckFatal(t, err)
			args := api.XactReqArgs{ID: xactID, Kind: cmn.ActEvictObjects, Timeout: rebalanceTimeout}
			_, err = api.WaitForXaction(baseParams, args)
			tassert.CheckFatal(t, err)

			if test.dstBck.IsAIS() {
				tutils.SetBackendBck(t, baseParams, test.dstBck, test.srcBck)
			}

			tutils.Logln("starting cloud download...")
			id, err := api.DownloadWithParam(baseParams, downloader.DlTypeCloud, downloader.DlCloudBody{
				DlBase: downloader.DlBase{
					Bck:         test.dstBck,
					Description: generateDownloadDesc(),
				},
				Prefix: prefix,
				Suffix: suffix,
			})
			tassert.CheckFatal(t, err)

			tutils.Logln("wait for cloud download...")
			waitForDownload(t, id, time.Minute)

			objs, err := tutils.ListObjectNames(proxyURL, test.dstBck, prefix, 0)
			tassert.CheckFatal(t, err)
			tassert.Errorf(t, reflect.DeepEqual(objs, expectedObjs), "expected objs: %s, got: %s", expectedObjs, objs)

			// Test cancellation
			tutils.Logln("evicting cloud bucket...")
			xactID, err = api.EvictList(baseParams, test.srcBck, expectedObjs)
			tassert.CheckFatal(t, err)
			args = api.XactReqArgs{ID: xactID, Kind: cmn.ActEvictObjects, Timeout: rebalanceTimeout}
			_, err = api.WaitForXaction(baseParams, args)
			tassert.CheckFatal(t, err)

			tutils.Logln("starting cloud download...")
			id, err = api.DownloadWithParam(baseParams, downloader.DlTypeCloud, downloader.DlCloudBody{
				DlBase: downloader.DlBase{
					Bck:         test.dstBck,
					Description: generateDownloadDesc(),
				},
				Prefix: prefix,
				Suffix: suffix,
			})
			tassert.CheckFatal(t, err)

			time.Sleep(500 * time.Millisecond)

			tutils.Logln("aborting cloud download...")
			err = api.AbortDownload(baseParams, id)
			tassert.CheckFatal(t, err)

			resp, err := api.DownloadStatus(baseParams, id)
			tassert.CheckFatal(t, err)
			if !resp.Aborted {
				t.Errorf("canceled cloud download %v not marked", id)
			}

			checkDownloadList(t, 2)
		})
	}
}

func TestDownloadStatus(t *testing.T) {
	var (
		bck = cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		}
		baseParams = tutils.BaseAPIParams()
		m          = ioContext{t: t}
	)

	m.saveClusterState()
	if m.originalTargetCount < 2 {
		t.Errorf("At least 2 targets are required.")
		return
	}

	var (
		shortFileName = "shortFile"
		longFileName  = tutils.GenerateNotConflictingObjectName(shortFileName, "longFile", bck, m.smap)
	)

	files := map[string]string{
		shortFileName: "https://raw.githubusercontent.com/NVIDIA/aistore/master/README.md",
		longFileName:  "https://storage.googleapis.com/nvdata-openimages/openimages-train-000001.tar",
	}

	clearDownloadList(t)

	// Create ais bucket
	tutils.CreateFreshBucket(t, m.proxyURL, bck)
	defer tutils.DestroyBucket(t, m.proxyURL, bck)

	id, err := api.DownloadMulti(baseParams, generateDownloadDesc(), bck, files)
	tassert.CheckFatal(t, err)

	// Wait for the short file to be downloaded
	err = tutils.WaitForObjectToBeDowloaded(baseParams, bck, shortFileName, 5*time.Second)
	tassert.CheckFatal(t, err)

	resp, err := api.DownloadStatus(baseParams, id)
	tassert.CheckFatal(t, err)

	tassert.Errorf(t, resp.Total == 2, "expected %d objects, got %d", 2, resp.Total)
	tassert.Errorf(t, resp.FinishedCnt == 1, "expected the short file to be downloaded")
	tassert.Fatalf(t, len(resp.CurrentTasks) == 1, "did not expect the long file to be already downloaded")
	tassert.Fatalf(
		t, resp.CurrentTasks[0].Name == longFileName,
		"invalid file name in status message, expected: %s, got: %s",
		longFileName, resp.CurrentTasks[0].Name,
	)

	checkDownloadList(t)
}

func TestDownloadStatusError(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{Long: true})

	var (
		bck = cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		}
		files = map[string]string{
			"invalidURL":   "http://some.invalid.url",
			"notFoundFile": "https://google.com/404.tar",
		}

		proxyURL   = tutils.RandomProxyURL(t)
		baseParams = tutils.BaseAPIParams(proxyURL)
	)

	clearDownloadList(t)

	// Create ais bucket
	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	id, err := api.DownloadMulti(baseParams, generateDownloadDesc(), bck, files)
	tassert.CheckFatal(t, err)

	// Wait to make sure both files were processed by downloader
	waitForDownload(t, id, 10*time.Second)

	resp, err := api.DownloadStatus(baseParams, id)
	tassert.CheckFatal(t, err)

	tassert.Errorf(t, resp.Total == len(files), "expected %d objects, got %d", len(files), resp.Total)
	tassert.Errorf(t, resp.FinishedCnt == 0, "expected 0 files to be finished")
	tassert.Fatalf(
		t, resp.ErrorCnt == len(files),
		"expected 2 downloading errors, but got: %d errors: %v", len(resp.Errs), resp.Errs,
	)

	invalidAddressCausedError := resp.Errs[0].Name == "invalidURL" || resp.Errs[1].Name == "invalidURL"
	notFoundFileCausedError := resp.Errs[0].Name == "notFoundFile" || resp.Errs[1].Name == "notFoundFile"

	if !(invalidAddressCausedError && notFoundFileCausedError) {
		t.Errorf("expected objects that cause errors to be (%s, %s), but got: (%s, %s)",
			"invalidURL", "notFoundFile", resp.Errs[0].Name, resp.Errs[1].Name)
	}

	checkDownloadList(t)
}

func TestDownloadSingleValidExternalAndInternalChecksum(t *testing.T) {
	var (
		proxyURL   = tutils.RandomProxyURL(t)
		baseParams = tutils.BaseAPIParams(proxyURL)

		bck = cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		}
		objNameFirst  = "object-first"
		objNameSecond = "object-second"

		linkFirst  = "https://storage.googleapis.com/minikube/iso/minikube-v0.23.2.iso.sha256"
		linkSecond = "https://raw.githubusercontent.com/NVIDIA/aistore/master/README.md"

		expectedObjects = []string{objNameFirst, objNameSecond}
	)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	_, err := api.SetBucketProps(baseParams, bck, cmn.BucketPropsToUpdate{
		Cksum: &cmn.CksumConfToUpdate{ValidateWarmGet: api.Bool(true)},
	})
	tassert.CheckFatal(t, err)

	id, err := api.DownloadSingle(baseParams, generateDownloadDesc(), bck, objNameFirst, linkFirst)
	tassert.CheckError(t, err)
	id2, err := api.DownloadSingle(baseParams, generateDownloadDesc(), bck, objNameSecond, linkSecond)
	tassert.CheckError(t, err)

	waitForDownload(t, id, 10*time.Second)
	waitForDownload(t, id2, 10*time.Second)

	// If the file was successfully downloaded, it means that the external checksum was correct. Also because of the
	// ValidateWarmGet property being set to True, if it was downloaded without errors then the internal checksum was
	// also set properly
	tutils.EnsureObjectsExist(t, baseParams, bck, expectedObjects...)
}

func TestDownloadMultiValidExternalAndInternalChecksum(t *testing.T) {
	var (
		proxyURL   = tutils.RandomProxyURL(t)
		baseParams = tutils.BaseAPIParams(proxyURL)

		bck = cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		}
		objNameFirst  = "linkFirst"
		objNameSecond = "linkSecond"

		m = map[string]string{
			"linkFirst":  "https://storage.googleapis.com/minikube/iso/minikube-v0.23.2.iso.sha256",
			"linkSecond": "https://raw.githubusercontent.com/NVIDIA/aistore/master/README.md",
		}

		expectedObjects = []string{objNameFirst, objNameSecond}
	)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	_, err := api.SetBucketProps(baseParams, bck, cmn.BucketPropsToUpdate{
		Cksum: &cmn.CksumConfToUpdate{ValidateWarmGet: api.Bool(true)},
	})
	tassert.CheckFatal(t, err)

	id, err := api.DownloadMulti(baseParams, generateDownloadDesc(), bck, m)
	tassert.CheckFatal(t, err)

	waitForDownload(t, id, 30*time.Second)
	checkDownloadedObjects(t, id, bck, expectedObjects)

	tutils.EnsureObjectsExist(t, baseParams, bck, expectedObjects...)
}

func TestDownloadRangeValidExternalAndInternalChecksum(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{Long: true})

	var (
		proxyURL   = tutils.RandomProxyURL(t)
		baseParams = tutils.BaseAPIParams(proxyURL)

		bck = cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		}

		template        = "storage.googleapis.com/minikube/iso/minikube-v0.{21..25..2}.0.iso.sha256"
		expectedObjects = []string{
			"minikube-v0.21.0.iso.sha256",
			"minikube-v0.23.0.iso.sha256",
			"minikube-v0.25.0.iso.sha256",
		}
	)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	_, err := api.SetBucketProps(baseParams, bck, cmn.BucketPropsToUpdate{
		Cksum: &cmn.CksumConfToUpdate{ValidateWarmGet: api.Bool(true)},
	})
	tassert.CheckFatal(t, err)

	id, err := api.DownloadRange(baseParams, generateDownloadDesc(), bck, template)
	tassert.CheckFatal(t, err)

	waitForDownload(t, id, 10*time.Second)
	checkDownloadedObjects(t, id, bck, expectedObjects)

	tutils.EnsureObjectsExist(t, baseParams, bck, expectedObjects...)
}

func TestDownloadIntoNonexistentBucket(t *testing.T) {
	var (
		baseParams = tutils.BaseAPIParams()
		objName    = "object"
		obj        = "storage.googleapis.com/nvdata-openimages/openimages-train-000001.tar"
	)

	bucket, err := tutils.GenerateNonexistentBucketName("download", baseParams)
	tassert.CheckFatal(t, err)

	bck := cmn.Bck{
		Name:     bucket,
		Provider: cmn.ProviderAIS,
	}
	_, err = api.DownloadSingle(baseParams, generateDownloadDesc(), bck, objName, obj)
	tassert.Fatalf(t, err != nil, "expected an error, but got none")

	tutils.CheckErrIsNotFound(t, err)
}

func TestDownloadMpathEvents(t *testing.T) {
	var (
		proxyURL   = tutils.RandomProxyURL(t)
		baseParams = tutils.BaseAPIParams(proxyURL)
		bck        = cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		}
		objsCnt = 100

		template = "storage.googleapis.com/nvdata-openimages/openimages-train-{000000..000050}.tar"
		m        = make(map[string]string, objsCnt)
	)

	clearDownloadList(t)

	// Prepare objects to be downloaded to targets. Multiple objects to make
	// sure that at least one of them gets into target with disabled mountpath.
	for i := 0; i < objsCnt; i++ {
		m[strconv.FormatInt(int64(i), 10)] = "https://raw.githubusercontent.com/NVIDIA/aistore/master/README.md"
	}

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	id1, err := api.DownloadRange(baseParams, generateDownloadDesc(), bck, template)
	tassert.CheckFatal(t, err)
	tutils.Logf("Started large download job %s, meant to be aborted\n", id1)

	// Abort just in case something goes wrong.
	defer abortDownload(t, id1)

	smap := tutils.GetClusterMap(t, proxyURL)
	removeTarget := tutils.ExtractTargetNodes(smap)[0]

	mpathList, err := api.GetMountpaths(baseParams, removeTarget)
	tassert.CheckFatal(t, err)
	tassert.Fatalf(t, len(mpathList.Available) >= 2, "%s requires 2 or more mountpaths", t.Name())

	mpathID := cmn.NowRand().Intn(len(mpathList.Available))
	removeMpath := mpathList.Available[mpathID]
	tutils.Logf("Disabling a mountpath %s at target: %s\n", removeMpath, removeTarget.ID())
	err = api.DisableMountpath(baseParams, removeTarget.ID(), removeMpath)
	tassert.CheckFatal(t, err)

	defer func() {
		tutils.Logf("Enabling mountpath %s at target %s...\n", removeMpath, removeTarget.ID())
		err = api.EnableMountpath(baseParams, removeTarget, removeMpath)
		tassert.CheckFatal(t, err)
	}()

	// Wait until downloader is aborted.
	waitForDownloaderToFinish(t, baseParams, removeTarget.ID(), time.Second*30)

	// Downloader finished on required target, safe to abort the rest.
	tutils.Logf("Aborting download job %s\n", id1)
	abortDownload(t, id1)

	objs, err := tutils.ListObjectNames(proxyURL, bck, "", 0)
	tassert.CheckError(t, err)
	tassert.Fatalf(t, len(objs) == 0, "objects should not have been downloaded, download should have been aborted\n")

	id2, err := api.DownloadMulti(baseParams, generateDownloadDesc(), bck, m)
	tassert.CheckFatal(t, err)
	tutils.Logf("Started download job %s, waiting for it to finish\n", id2)

	waitForDownload(t, id2, 2*time.Minute)
	objs, err = tutils.ListObjectNames(proxyURL, bck, "", 0)
	tassert.CheckError(t, err)
	tassert.Fatalf(t, len(objs) == objsCnt, "Expected %d objects to be present, got: %d", objsCnt, len(objs))
}

func TestDownloadOverrideObject(t *testing.T) {
	var (
		proxyURL   = tutils.RandomProxyURL(t)
		baseParams = tutils.BaseAPIParams(proxyURL)
		bck        = cmn.Bck{
			Name:     cmn.RandString(10),
			Provider: cmn.ProviderAIS,
		}
		p = cmn.DefaultAISBckProps()

		objName = cmn.RandString(10)
		link    = "https://storage.googleapis.com/minikube/iso/minikube-v0.23.2.iso.sha256"

		expectedSize int64 = 65
	)

	clearDownloadList(t)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	downloadObject(t, bck, objName, link, false /*shouldBeSkipped*/)
	oldProps := verifyProps(t, bck, objName, expectedSize, "1")

	// Update the file
	r, _ := readers.NewRandReader(10, p.Cksum.Type)
	err := api.PutObject(api.PutObjectArgs{
		BaseParams: baseParams,
		Bck:        bck,
		Object:     objName,
		Cksum:      r.Cksum(),
		Reader:     r,
	})
	tassert.Fatalf(t, err != nil, "expected: err!=nil, got: nil")
	verifyProps(t, bck, objName, expectedSize, "1")

	downloadObject(t, bck, objName, link, true /*shouldBeSkipped*/)
	newProps := verifyProps(t, bck, objName, expectedSize, "1")
	tassert.Errorf(
		t, oldProps.Atime == newProps.Atime,
		"atime match (%v != %v)", oldProps.Atime, newProps.Atime,
	)
}

func TestDownloadOverrideObjectWeb(t *testing.T) {
	var (
		proxyURL   = tutils.RandomProxyURL(t)
		baseParams = tutils.BaseAPIParams(proxyURL)
		bck        = cmn.Bck{
			Name:     cmn.RandString(10),
			Provider: cmn.ProviderAIS,
		}
		p = cmn.DefaultAISBckProps()

		objName = cmn.RandString(10)
		link    = "https://raw.githubusercontent.com/NVIDIA/aistore/master/LICENSE"

		expectedSize int64 = 1075
		newSize      int64 = 10
	)

	clearDownloadList(t)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	downloadObject(t, bck, objName, link, false /*shouldBeSkipped*/)
	oldProps := verifyProps(t, bck, objName, expectedSize, "1")

	// Update the file
	r, _ := readers.NewRandReader(newSize, p.Cksum.Type)
	err := api.PutObject(api.PutObjectArgs{
		BaseParams: baseParams,
		Bck:        bck,
		Object:     objName,
		Cksum:      r.Cksum(),
		Reader:     r,
	})
	tassert.Fatalf(t, err == nil, "expected: err!=nil, got: %v", err)
	verifyProps(t, bck, objName, newSize, "2")

	downloadObject(t, bck, objName, link, false /*shouldBeSkipped*/)
	newProps := verifyProps(t, bck, objName, expectedSize, "3")
	tassert.Errorf(
		t, oldProps.Atime != newProps.Atime,
		"atime match (%v == %v)", oldProps.Atime, newProps.Atime,
	)
}

func TestDownloadOverrideObjectCloud(t *testing.T) {
	var (
		proxyURL   = tutils.RandomProxyURL(t)
		baseParams = tutils.BaseAPIParams(proxyURL)
		bck        = cmn.Bck{
			Name:     cmn.RandString(10),
			Provider: cmn.ProviderAIS,
		}
		dlBody = downloader.DlCloudBody{
			DlBase: downloader.DlBase{Bck: bck},
		}
		m = &ioContext{
			t:   t,
			num: 10,
			bck: cliBck,
		}
	)

	tutils.CheckSkip(t, tutils.SkipTestArgs{Cloud: true, Bck: m.bck})

	m.init()
	m.cloudPuts(false /*evict*/)
	defer m.del()

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	tutils.SetBackendBck(t, baseParams, bck, m.bck)

	downloadObjectCloud(t, dlBody, m.num, 0)
	m.cloudPuts(false /*evict*/)
	downloadObjectCloud(t, dlBody, m.num, 0)
}

func TestDownloadSkipObject(t *testing.T) {
	var (
		proxyURL = tutils.RandomProxyURL(t)
		bck      = cmn.Bck{
			Name:     cmn.RandString(10),
			Provider: cmn.ProviderAIS,
		}

		objName = cmn.RandString(10)
		link    = "https://storage.googleapis.com/minikube/iso/minikube-v0.23.2.iso.sha256"

		expectedSize    int64 = 65
		expectedVersion       = "1"
	)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	downloadObject(t, bck, objName, link, false /*shouldBeSkipped*/)
	oldProps := verifyProps(t, bck, objName, expectedSize, expectedVersion)

	downloadObject(t, bck, objName, link, true /*shouldBeSkipped*/)
	newProps := verifyProps(t, bck, objName, expectedSize, expectedVersion)
	tassert.Errorf(
		t, oldProps.Atime == newProps.Atime,
		"atime mismatch (%v vs %v)", oldProps.Atime, newProps.Atime,
	)
}

func TestDownloadSkipObjectCloud(t *testing.T) {
	var (
		proxyURL   = tutils.RandomProxyURL(t)
		baseParams = tutils.BaseAPIParams(proxyURL)
		bck        = cmn.Bck{
			Name:     cmn.RandString(10),
			Provider: cmn.ProviderAIS,
		}
		dlBody = downloader.DlCloudBody{
			DlBase: downloader.DlBase{Bck: bck},
		}
		m = &ioContext{
			t:   t,
			num: 10,
			bck: cliBck,
		}
	)

	tutils.CheckSkip(t, tutils.SkipTestArgs{Cloud: true, Bck: m.bck})

	m.init()
	m.cloudPuts(false /*evict*/)
	defer m.del()

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	tutils.SetBackendBck(t, baseParams, bck, m.bck)

	downloadObjectCloud(t, dlBody, m.num, 0)
	downloadObjectCloud(t, dlBody, m.num, m.num)

	// Put some more cloud objects (we expect that only the new ones will be downloaded)
	m.num += 10
	m.cloudRefill()

	downloadObjectCloud(t, dlBody, m.num, m.num-10)
}

func TestDownloadSync(t *testing.T) {
	var (
		proxyURL   = tutils.RandomProxyURL(t)
		baseParams = tutils.BaseAPIParams(proxyURL)
		bck        = cmn.Bck{
			Name:     cmn.RandString(10),
			Provider: cmn.ProviderAIS,
		}
		dlBody = downloader.DlCloudBody{
			DlBase: downloader.DlBase{Bck: bck},
		}
		m = &ioContext{
			t:   t,
			num: 10,
			bck: cliBck,
		}
		objsToDelete = 4
	)

	tutils.CheckSkip(t, tutils.SkipTestArgs{Cloud: true, Bck: m.bck})

	m.init()
	m.cloudPuts(false /*evict*/)
	defer m.del()

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	tutils.SetBackendBck(t, baseParams, bck, m.bck)

	tutils.Logln("1. initial sync of cloud bucket...")
	dlBody.Sync = true
	downloadObjectCloud(t, dlBody, m.num, 0)
	downloadObjectCloud(t, dlBody, m.num, m.num)

	m.del(objsToDelete)

	// Check that only deleted objects are replaced (deleted in this case).
	tutils.Logln("2. re-syncing cloud bucket...")
	downloadObjectCloud(t, dlBody, m.num, m.num-objsToDelete)
	downloadObjectCloud(t, dlBody, m.num-objsToDelete, m.num-objsToDelete)

	tutils.Logln("3. filling removed cloud objects...")
	m.cloudRefill()

	// Check that new objects are correctly downloaded.
	tutils.Logln("4. re-syncing cloud bucket...")
	downloadObjectCloud(t, dlBody, m.num, m.num-objsToDelete)
	downloadObjectCloud(t, dlBody, m.num, m.num)
	dlBody.Sync = false
	downloadObjectCloud(t, dlBody, m.num, m.num)

	tutils.Logln("5. overridding the objects and deleting some of them...")
	m.cloudPuts(false /*evict*/, true /*override*/)
	m.del(objsToDelete)

	// Check that all objects have been replaced.
	tutils.Logln("6. re-syncing cloud bucket...")
	dlBody.Sync = true
	downloadObjectCloud(t, dlBody, m.num, 0)
	downloadObjectCloud(t, dlBody, m.num-objsToDelete, m.num-objsToDelete)

	tutils.Logln("7. check that syncing with prefix and suffix works")
	dlBody.Prefix = "someprefix-"
	dlBody.Suffix = ""
	downloadObjectCloud(t, dlBody, 0, 0)
	dlBody.Prefix = ""
	dlBody.Suffix = "somesuffix-"
	downloadObjectCloud(t, dlBody, 0, 0)
}

func TestDownloadJobLimitConnections(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{Long: true})

	var (
		proxyURL   = tutils.RandomProxyURL(t)
		baseParams = tutils.BaseAPIParams(proxyURL)
		bck        = cmn.Bck{
			Name:     cmn.RandString(10),
			Provider: cmn.ProviderAIS,
		}

		template = "https://storage.googleapis.com/minikube/iso/minikube-v0.{18..35}.0.iso"
	)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	smap, err := api.GetClusterMap(baseParams)
	tassert.CheckFatal(t, err)

	id, err := api.DownloadWithParam(baseParams, downloader.DlTypeRange, downloader.DlRangeBody{
		DlBase: downloader.DlBase{
			Bck:         bck,
			Description: generateDownloadDesc(),
			Limits: downloader.DlLimits{
				Connections: 2,
			},
		},
		Template: template,
	})
	tassert.CheckFatal(t, err)
	defer abortDownload(t, id)

	time.Sleep(2 * time.Second) // wait for downloader to pick up the job

	resp, err := api.DownloadStatus(baseParams, id)
	tassert.CheckFatal(t, err)

	tassert.Errorf(
		t, len(resp.CurrentTasks) > smap.CountTargets(),
		"number of tasks mismatch (expected at least: %d, got: %d)",
		smap.CountTargets()+1, len(resp.CurrentTasks),
	)
	tassert.Errorf(
		t, len(resp.CurrentTasks) <= 2*smap.CountTargets(),
		"number of tasks mismatch (expected as most: %d, got: %d)",
		2*smap.CountTargets(), len(resp.CurrentTasks),
	)
}

func TestDownloadJobConcurrency(t *testing.T) {
	var (
		proxyURL   = tutils.RandomProxyURL(t)
		baseParams = tutils.BaseAPIParams(proxyURL)
		bck        = cmn.Bck{
			Name:     cmn.RandString(10),
			Provider: cmn.ProviderAIS,
		}

		template = "https://storage.googleapis.com/minikube/iso/minikube-v0.{18..35}.0.iso"
	)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	smap, err := api.GetClusterMap(baseParams)
	tassert.CheckFatal(t, err)

	id1, err := api.DownloadWithParam(baseParams, downloader.DlTypeRange, downloader.DlRangeBody{
		DlBase: downloader.DlBase{
			Bck:         bck,
			Description: generateDownloadDesc(),
			Limits: downloader.DlLimits{
				Connections: 1,
			},
		},
		Template: template,
	})
	tassert.CheckFatal(t, err)
	defer abortDownload(t, id1)

	id2, err := api.DownloadRange(baseParams, generateDownloadDesc(), bck, template)
	tassert.CheckFatal(t, err)
	defer abortDownload(t, id2)

	tutils.Logln("waiting for checks...")
	var (
		concurrentJobs bool
		resp1, resp2   downloader.DlStatusResp
	)
	for i := 0; i < 10; i++ {
		resp1, err = api.DownloadStatus(baseParams, id1)
		tassert.CheckFatal(t, err)

		// Expect that number of tasks never exceeds the defined limit.
		tassert.Errorf(
			t, len(resp1.CurrentTasks) <= smap.CountTargets(),
			"number of tasks mismatch (expected at most: %d, got: %d)",
			smap.CountTargets(), len(resp1.CurrentTasks),
		)

		// Expect that at some point the second job will be run concurrently.
		resp2, err = api.DownloadStatus(baseParams, id2)
		tassert.CheckFatal(t, err)

		if len(resp2.CurrentTasks) > 0 && len(resp1.CurrentTasks) > 0 {
			concurrentJobs = true
		}
		time.Sleep(time.Second)
	}

	tassert.Errorf(t, concurrentJobs, "expected jobs to run concurrently")
	tutils.Logln("done waiting")
}

// NOTE: Test may fail if the network is SUPER slow!!
func TestDownloadJobBytesThrottling(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{Long: true})

	const (
		link = "https://storage.googleapis.com/minikube/iso/minikube-v0.35.0.iso"

		// Bytes per hour limit.
		softLimit = 5 * cmn.KiB
		// Downloader could potentially download a little bit more but should
		// never exceed this.
		hardLimit = 7 * cmn.KiB
	)

	var (
		proxyURL   = tutils.RandomProxyURL(t)
		baseParams = tutils.BaseAPIParams(proxyURL)
		bck        = cmn.Bck{
			Name:     cmn.RandString(10),
			Provider: cmn.ProviderAIS,
		}
	)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	id, err := api.DownloadWithParam(baseParams, downloader.DlTypeSingle, downloader.DlSingleBody{
		DlBase: downloader.DlBase{
			Bck:         bck,
			Description: generateDownloadDesc(),
			Limits: downloader.DlLimits{
				BytesPerHour: softLimit,
			},
		},
		DlSingleObj: downloader.DlSingleObj{
			ObjName: "object",
			Link:    link,
		},
	})
	tassert.CheckFatal(t, err)
	defer abortDownload(t, id)

	time.Sleep(10 * time.Second) // wait for downloader to download `softLimit` bytes

	resp, err := api.DownloadStatus(baseParams, id)
	tassert.CheckFatal(t, err)

	tassert.Fatalf(t, len(resp.CurrentTasks) == 1, "expected one running task")
	tassert.Errorf(
		t, resp.CurrentTasks[0].Downloaded < hardLimit,
		"no more than %d should be downloaded, got: %d",
		hardLimit, resp.CurrentTasks[0].Downloaded,
	)
}
