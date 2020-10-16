// Package fs provides mountpath and FQN abstractions and methods to resolve/map stored content
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package fs_test

import (
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/ios"
	"github.com/NVIDIA/aistore/tutils"
	"github.com/NVIDIA/aistore/tutils/tassert"
)

func TestWalkBck(t *testing.T) {
	var (
		bck   = cmn.Bck{Name: "name", Provider: cmn.ProviderAIS}
		tests = []struct {
			name     string
			mpathCnt int
			sorted   bool
		}{
			{name: "simple_sorted", mpathCnt: 1, sorted: true},
			{name: "10mpaths_sorted", mpathCnt: 10, sorted: true},
		}
	)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fs.Init(ios.NewIOStaterMock())
			fs.DisableFsIDCheck()
			_ = fs.CSM.RegisterContentType(fs.ObjectType, &fs.ObjectContentResolver{})

			mpaths := make([]string, 0, test.mpathCnt)
			defer func() {
				for _, mpath := range mpaths {
					os.RemoveAll(mpath)
				}
			}()

			for i := 0; i < test.mpathCnt; i++ {
				mpath, err := ioutil.TempDir("", "testwalk")
				tassert.CheckFatal(t, err)

				err = cmn.CreateDir(mpath)
				tassert.CheckFatal(t, err)

				err = fs.Add(mpath)
				tassert.CheckFatal(t, err)

				mpaths = append(mpaths, mpath)
			}

			avail, _ := fs.Get()
			var fileNames []string
			for _, mpath := range avail {
				dir := mpath.MakePathCT(bck, fs.ObjectType)
				err := cmn.CreateDir(dir)
				tassert.CheckFatal(t, err)

				_, names := tutils.PrepareDirTree(t, tutils.DirTreeDesc{
					InitDir: dir,
					Dirs:    rand.Int()%100 + 1,
					Files:   rand.Int()%100 + 1,
					Depth:   rand.Int()%4 + 1,
					Empty:   false,
				})
				fileNames = append(fileNames, names...)
			}

			var (
				objs = make([]string, 0, 100)
				fqns = make([]string, 0, 100)
			)
			err := fs.WalkBck(&fs.WalkBckOptions{
				Options: fs.Options{
					Bck:         bck,
					CTs:         []string{fs.ObjectType},
					ErrCallback: nil,
					Callback: func(fqn string, de fs.DirEntry) error {
						parsedFQN, err := fs.ParseFQN(fqn)
						tassert.CheckError(t, err)
						objs = append(objs, parsedFQN.ObjName)
						fqns = append(fqns, fqn)
						return nil
					},
					Sorted: test.sorted,
				},
			})
			tassert.CheckFatal(t, err)

			sorted := sort.IsSorted(sort.StringSlice(objs))
			tassert.Fatalf(t, sorted == test.sorted, "expected the output to be sorted=%t", test.sorted)

			sort.Strings(fqns)
			sort.Strings(fileNames)
			tassert.Fatalf(t, reflect.DeepEqual(fqns, fileNames), "found objects don't match expected objects")
		})
	}
}

func TestWalkBckSkipDir(t *testing.T) {
	rand.Seed(time.Now().UnixNano())
	type (
		mpathMeta struct {
			total int
			done  bool
		}
	)

	var (
		bck           = cmn.Bck{Name: "name", Provider: cmn.ProviderAIS}
		mpathCnt      = 5 + rand.Int()%5
		minObjectsCnt = 10
		mpaths        = make(map[string]*mpathMeta)
	)

	fs.Init(ios.NewIOStaterMock())
	fs.DisableFsIDCheck()
	_ = fs.CSM.RegisterContentType(fs.ObjectType, &fs.ObjectContentResolver{})

	defer func() {
		for mpath := range mpaths {
			os.RemoveAll(mpath)
		}
	}()

	for i := 0; i < mpathCnt; i++ {
		mpath, err := ioutil.TempDir("", "testwalk")
		tassert.CheckFatal(t, err)

		err = cmn.CreateDir(mpath)
		tassert.CheckFatal(t, err)

		err = fs.Add(mpath)
		tassert.CheckFatal(t, err)

		mpaths[mpath] = &mpathMeta{total: 0, done: false}
	}

	avail, _ := fs.Get()
	for _, mpath := range avail {
		dir := mpath.MakePathCT(bck, fs.ObjectType)
		err := cmn.CreateDir(dir)
		tassert.CheckFatal(t, err)

		totalFilesCnt := rand.Int()%100 + minObjectsCnt
		for i := 0; i < totalFilesCnt; i++ {
			f, err := ioutil.TempFile(dir, "")
			tassert.CheckFatal(t, err)
			f.Close()
		}
	}

	fqns := make([]string, 0, 100)
	err := fs.WalkBck(&fs.WalkBckOptions{
		Options: fs.Options{
			Bck:         bck,
			CTs:         []string{fs.ObjectType},
			ErrCallback: nil,
			Callback: func(fqn string, de fs.DirEntry) error {
				fqns = append(fqns, fqn)
				return nil
			},
			Sorted: true,
		},
		ValidateCallback: func(fqn string, de fs.DirEntry) error {
			if de.IsDir() {
				return nil
			}
			parsedFQN, err := fs.ParseFQN(fqn)
			tassert.CheckError(t, err)
			cmn.Assert(!mpaths[parsedFQN.MpathInfo.Path].done)
			if rand.Int()%10 == 0 {
				mpaths[parsedFQN.MpathInfo.Path].done = true
				return filepath.SkipDir
			}
			mpaths[parsedFQN.MpathInfo.Path].total++
			return nil
		},
	})
	tassert.CheckFatal(t, err)

	expectedTotal := 0
	for _, meta := range mpaths {
		expectedTotal += meta.total
	}
	tassert.Fatalf(t, expectedTotal == len(fqns), "expected %d objects, got %d", expectedTotal, len(fqns))
}
