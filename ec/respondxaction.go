package ec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/transport"
)

type (
	// Xaction responsible for responding to EC requests of other targets
	// Should not be stopped if number of known targets is small
	XactRespond struct {
		xactECBase
	}
)

func NewRespondXact(t cluster.Target, bmd cluster.Bowner, smap cluster.Sowner,
	si *cluster.Snode, bucket string, reqBundle, respBundle *transport.StreamBundle) *XactRespond {

	runner := &XactRespond{
		xactECBase: newXactECBase(t, bmd, smap, si, bucket, reqBundle, respBundle),
	}

	return runner
}

func (r *XactRespond) Run() (err error) {
	glog.Infof("Starting %s", r.Getname())

	conf := cmn.GCO.Get()
	tck := time.NewTicker(conf.Periodic.StatsTime)
	lastAction := time.Now()
	idleTimeout := conf.Timeout.SendFile * 3

	// as of now all requests are equal. Some may get throttling later
	for {
		select {
		case <-tck.C:
			if s := fmt.Sprintf("%v", r.stats.stats()); s != "" {
				glog.Info(s)
			}
		case <-r.ChanCheckTimeout():
			idleEnds := lastAction.Add(idleTimeout)
			if idleEnds.Before(time.Now()) && r.Timeout() {
				if glog.V(4) {
					glog.Infof("Idle time is over: %v. Last action at: %v",
						time.Now(), lastAction)
				}

				r.stop()

				return nil
			}

		case <-r.ChanAbort():
			r.stop()
			return fmt.Errorf("%s aborted, exiting", r)
		}
	}
}

// Utility function to cleanup both object/slice and its meta on the local node
// Used when processing object deletion request
func (r *XactRespond) removeObjAndMeta(bucket, objname string, bckIsLocal bool) error {
	if glog.V(4) {
		glog.Infof("Delete request for %s/%s", bucket, objname)
	}

	// to be consistent with PUT, object's files are deleted in a reversed
	// order: first Metafile is removed, then Replica/Slice
	// Why: the main object is gone already, so we do not want any target
	// responds that it has the object because it has metafile. We delete
	// metafile that makes remained slices/replicas outdated and can be cleaned
	// up later by LRU or other runner
	for _, tp := range []string{MetaType, fs.ObjectType, SliceType} {
		fqnMeta, errstr := cluster.FQN(tp, bucket, objname, bckIsLocal)
		if errstr != "" {
			return errors.New(errstr)
		}
		if err := os.RemoveAll(fqnMeta); err != nil {
			return fmt.Errorf("error removing %s %q: %v", tp, fqnMeta, err)
		}
	}

	return nil
}

// DispatchReq is responsible for handling request from other targets
// which require a response
func (r *XactRespond) DispatchReq(iReq IntraReq, bucket, objName string) {
	bckIsLocal := r.bmd.Get().IsLocal(bucket)
	daemonID := iReq.Sender

	switch iReq.Act {
	case reqDel:
		// object cleanup request: delete replicas, slices and metafiles
		if err := r.removeObjAndMeta(bucket, objName, bckIsLocal); err != nil {
			glog.Errorf("Failed to delete %s/%s: %v", bucket, objName, err)
		}
	case reqGet:
		//slice or replica request: send the object's data to the caller
		var fqn, errstr string
		if iReq.IsSlice {
			if glog.V(4) {
				glog.Infof("Received request for slice %d of %s", iReq.Meta.SliceID, objName)
			}
			fqn, errstr = cluster.FQN(SliceType, bucket, objName, bckIsLocal)
		} else {
			if glog.V(4) {
				glog.Infof("Received request for replica %s", objName)
			}
			fqn, errstr = cluster.FQN(fs.ObjectType, bucket, objName, bckIsLocal)
		}
		if errstr != "" {
			glog.Errorf(errstr)
			return
		}

		if err := r.dataResponse(RespPut, fqn, bucket, objName, daemonID); err != nil {
			glog.Errorf("Failed to send back [GET req] %q: %v", fqn, err)
		}
	case ReqMeta:
		// metadata request: send the metadata to the caller
		fqn, errstr := cluster.FQN(MetaType, bucket, objName, bckIsLocal)
		if errstr != "" {
			glog.Errorf(errstr)
			return
		}
		if err := r.dataResponse(iReq.Act, fqn, bucket, objName, daemonID); err != nil {
			glog.Errorf("Failed to send back [META req] %q: %v", fqn, err)
		}
	default:
		// invalid request detected
		glog.Errorf("Invalid request type %d", iReq.Act)
	}
}

func (r *XactRespond) DispatchResp(iReq IntraReq, bucket, objName string, objAttrs transport.ObjectAttrs, object io.Reader) {
	uname := unique(iReq.Sender, bucket, objName)

	switch iReq.Act {
	case ReqPut:
		// a remote target sent a replica/slice while it was
		// encoding or restoring an object. In this case it just saves
		// the sent replica or slice to a local file along with its metadata
		// look for metadata in request
		if glog.V(4) {
			glog.Infof("Response from %s, %s", iReq.Sender, uname)
		}

		// First check if the request is valid: it must contain metadata
		meta := iReq.Meta
		if meta == nil {
			glog.Errorf("No metadata in request for %s/%s", bucket, objName)
			return
		}

		bckIsLocal := r.bmd.Get().IsLocal(bucket)
		var (
			objFQN, errstr string
			err            error
		)
		if iReq.IsSlice {
			if glog.V(4) {
				glog.Infof("Got slice response from %s (#%d of %s/%s)",
					iReq.Sender, iReq.Meta.SliceID, bucket, objName)
			}
			objFQN, errstr = cluster.FQN(SliceType, bucket, objName, bckIsLocal)
			if errstr != "" {
				glog.Error(errstr)
				return
			}
		} else {
			if glog.V(4) {
				glog.Infof("Got replica response from %s (%s/%s)",
					iReq.Sender, bucket, objName)
			}
			objFQN, errstr = cluster.FQN(fs.ObjectType, bucket, objName, bckIsLocal)
			if errstr != "" {
				glog.Error(errstr)
				return
			}
		}

		// save slice/object
		tmpFQN := fs.CSM.GenContentFQN(objFQN, fs.WorkfileType, "ec")
		buf, slab := mem2.AllocFromSlab2(cmn.MiB)
		err = cmn.SaveReaderSafe(tmpFQN, objFQN, object, buf)
		if err == nil {
			lom := &cluster.LOM{FQN: objFQN, T: r.t}
			if errstr := lom.Fill("", 0); errstr != "" {
				glog.Errorf("Failed to read resolve FQN %s: %s", objFQN, errstr)
				slab.Free(buf)
				return
			}
			lom.Version = objAttrs.Version
			lom.Atime = time.Unix(0, objAttrs.Atime)

			// LOM checksum is filled with checksum of a slice. Source object's checksum is stored in metadata
			if objAttrs.CksumType != "" {
				lom.Cksum = cmn.NewCksum(objAttrs.CksumType, objAttrs.CksumValue)
			}

			if errstr := lom.Persist(false); errstr != "" {
				err = errors.New(errstr)
			}
		}

		if err != nil {
			glog.Errorf("Failed to save %s/%s data to %q: %v",
				bucket, objName, objFQN, err)
			slab.Free(buf)
			return
		}

		// save its metadata
		metaFQN := fs.CSM.GenContentFQN(objFQN, MetaType, "")
		metaBuf, err := meta.marshal()
		if err == nil {
			err = cmn.SaveReader(metaFQN, bytes.NewReader(metaBuf), buf)
		}

		slab.Free(buf)
		if err != nil {
			glog.Errorf("Failed to save metadata to %q: %v", metaFQN, err)
		}
	default:
		// should be unreachable
		glog.Errorf("Invalid request type: %d", iReq.Act)
	}
}

func (r *XactRespond) Stop(error) { r.Abort() }

func (r *XactRespond) stop() {
	if r.Finished() {
		glog.Warningf("%s - not running, nothing to do", r)
		return
	}

	r.XactDemandBase.Stop()
	r.EndTime(time.Now())
}