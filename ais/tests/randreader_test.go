// Package integration contains AIS integration tests.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package integration

import (
	"sync"
	"testing"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/tutils"
)

func TestRandomReaderPutStress(t *testing.T) {
	var (
		numworkers = 1000
		numobjects = 10 // NOTE: increase this number if need be ...
		bck        = cmn.Bck{
			Name:     "RRTestBucket",
			Provider: cmn.ProviderAIS,
		}
		proxyURL   = tutils.GetPrimaryURL()
		baseParams = tutils.BaseAPIParams(proxyURL)
		wg         = &sync.WaitGroup{}
		dir        = t.Name()
		cksumType  = cmn.DefaultAISBckProps().Cksum.Type
	)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	for i := 0; i < numworkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tutils.PutRR(t, baseParams, fileSize, cksumType, bck, dir, numobjects, fnlen)
		}()
	}
	wg.Wait()
}
