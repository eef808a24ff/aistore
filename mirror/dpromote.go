// Package mirror provides local mirroring and replica management
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package mirror

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/xaction/registry"
)

// XactDirPromote copies a bucket locally within the same cluster

type (
	dirPromoteProvider struct {
		registry.BaseBckEntry
		xact *XactDirPromote

		t      cluster.Target
		dir    string
		params *cmn.ActValPromote
	}
	XactDirPromote struct {
		xactBckBase
		dir    string
		params *cmn.ActValPromote
	}
)

func (*dirPromoteProvider) New(args registry.XactArgs) registry.BucketEntry {
	c := args.Custom.(*registry.DirPromoteArgs)
	return &dirPromoteProvider{t: args.T, dir: c.Dir, params: c.Params}
}

func (p *dirPromoteProvider) Start(bck cmn.Bck) error {
	xact := NewXactDirPromote(p.dir, bck, p.t, p.params)
	go xact.Run()
	p.xact = xact
	return nil
}
func (*dirPromoteProvider) Kind() string        { return cmn.ActPromote }
func (p *dirPromoteProvider) Get() cluster.Xact { return p.xact }

//
// public methods
//

func NewXactDirPromote(dir string, bck cmn.Bck, t cluster.Target, params *cmn.ActValPromote) *XactDirPromote {
	return &XactDirPromote{
		xactBckBase: *newXactBckBase("", cmn.ActPromote, bck, t),
		dir:         dir,
		params:      params,
	}
}

func (r *XactDirPromote) Run() (err error) {
	glog.Infoln(r.String(), r.dir, "=>", r.Bck())
	opts := &fs.Options{
		Dir:      r.dir,
		Callback: r.walk,
		Sorted:   false,
	}
	if err := fs.Walk(opts); err != nil {
		glog.Errorln(err)
	}
	return
}

func (r *XactDirPromote) walk(fqn string, de fs.DirEntry) error {
	if de.IsDir() {
		return nil
	}
	if !r.params.Recurs {
		fname, err := filepath.Rel(r.dir, fqn)
		cmn.AssertNoErr(err)
		if strings.ContainsRune(fname, filepath.Separator) {
			return nil
		}
	}
	// NOTE: destination objName is:
	// r.params.ObjName + filepath.Base(fqn) if promoting single file
	// r.params.ObjName + strings.TrimPrefix(fileFqn, dirFqn) if promoting the whole directory
	cmn.Assert(filepath.IsAbs(fqn))

	bck := cluster.NewBckEmbed(r.Bck())
	if err := bck.Init(r.t.Bowner(), r.t.Snode()); err != nil {
		return err
	}
	objName := r.params.ObjName
	if objName != "" && objName[len(objName)-1] != os.PathSeparator {
		objName += string(os.PathSeparator)
	}
	objName += strings.TrimPrefix(strings.TrimPrefix(fqn, r.dir), string(filepath.Separator))
	objName = strings.Trim(objName, string(filepath.Separator))

	params := cluster.PromoteFileParams{
		SrcFQN:    fqn,
		Bck:       bck,
		ObjName:   objName,
		Overwrite: r.params.Overwrite,
		KeepOrig:  r.params.KeepOrig,
		Verbose:   r.params.Verbose,
	}
	lom, err := r.Target().PromoteFile(params)
	if err != nil {
		if finfo, ers := os.Stat(fqn); ers == nil {
			if !finfo.Mode().IsRegular() {
				glog.Warningf("%v (mode=%#x)", err, finfo.Mode()) // symbolic link, etc.
			}
		} else {
			glog.Error(err)
		}
	} else if lom != nil { // nil when (placement = different target)
		r.ObjectsInc()
		r.BytesAdd(lom.Size())
	}
	return nil
}
