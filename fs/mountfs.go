// Package fs provides mountpath and FQN abstractions and methods to resolve/map stored content
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package fs

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/mono"
	"github.com/NVIDIA/aistore/ios"
	"github.com/OneOfOne/xxhash"
)

const (
	uQuantum = 10 // each GET adds a "quantum" of utilization to the mountpath
)

// mountpath lifecycle-change enum
const (
	AddMpath     = "add-mp"
	RemoveMpath  = "remove-mp"
	EnableMpath  = "enable-mp"
	DisableMpath = "disable-mp"
)

const (
	TrashDir = "$trash"
)

// global singleton
var (
	mfs *MountedFS
)

// Terminology:
// - a mountpath is equivalent to (configurable) fspath - both terms are used interchangeably;
// - each mountpath is, simply, a local directory that is serviced by a local filesystem;
// - there's a 1-to-1 relationship between a mountpath and a local filesystem
//   (different mountpaths map onto different filesystems, and vise versa);
// - mountpaths of the form <filesystem-mountpoint>/a/b/c are supported.

type (
	MountpathInfo struct {
		Path       string // Cleaned OrigPath
		OrigPath   string // As entered by the user, must be used for logging / returning errors
		Fsid       syscall.Fsid
		FileSystem string
		PathDigest uint64

		// LOM caches
		lomCaches cmn.MultiSyncMap

		// capacity
		cmu      sync.RWMutex
		capacity Capacity
	}
	MPI map[string]*MountpathInfo

	Capacity struct {
		Used    uint64 `json:"used,string"`  // bytes
		Avail   uint64 `json:"avail,string"` // ditto
		PctUsed int32  `json:"pct_used"`     // %% used (redundant ok)
	}
	MPCap map[string]Capacity // [mpath => Capacity]

	// MountedFS holds all mountpaths for the target.
	MountedFS struct {
		mu sync.Mutex
		// fsIDs is set in which we store fsids of mountpaths. This allows for
		// determining if there are any duplications of file system - we allow
		// only one mountpath per file system.
		fsIDs map[syscall.Fsid]string
		// checkFsID determines if we should actually check FSID when adding new
		// mountpath. By default it is set to true.
		checkFsID bool
		// Available mountpaths - mountpaths which are used to store the data.
		available atomic.Pointer
		// Disabled mountpaths - mountpaths which for some reason did not pass
		// the health check and cannot be used for a moment.
		disabled atomic.Pointer
		// Iostats for the available mountpaths
		ios ios.IOStater

		// capacity
		cmu     sync.RWMutex
		capTime struct {
			curr, next int64
		}
		capStatus CapStatus
	}
	CapStatus struct {
		TotalUsed  uint64 // bytes
		TotalAvail uint64 // bytes
		PctAvg     int32  // used average (%)
		PctMax     int32  // max used (%)
		Err        error
		OOS        bool
	}
)

///////////////////
// MountpathInfo //
///////////////////

func newMountpath(cleanPath, origPath string, fsid syscall.Fsid, fs string) *MountpathInfo {
	mi := &MountpathInfo{
		Path:       cleanPath,
		OrigPath:   origPath,
		Fsid:       fsid,
		FileSystem: fs,
		PathDigest: xxhash.ChecksumString64S(cleanPath, cmn.MLCG32),
	}
	return mi
}

func (mi *MountpathInfo) String() string {
	return fmt.Sprintf("mp[%s, fs=%s]", mi.Path, mi.FileSystem)
}

func (mi *MountpathInfo) LomCache(idx int) *sync.Map { return mi.lomCaches.Get(idx) }

func (mi *MountpathInfo) evictLomCache() {
	for idx := 0; idx < cmn.MultiSyncMapCount; idx++ {
		cache := mi.LomCache(idx)
		cache.Range(func(key interface{}, _ interface{}) bool {
			cache.Delete(key)
			return true
		})
	}
}

func (mi *MountpathInfo) MakePathTrash() string { return filepath.Join(mi.Path, TrashDir) }

// MoveToTrash removes directory in steps:
// 1. Synchronously gets temporary directory name
// 2. Synchronously renames old folder to temporary directory
func (mi *MountpathInfo) MoveToTrash(dir string) error {
	// Loose assumption: removing something which doesn't exist is fine.
	if err := Access(dir); err != nil && os.IsNotExist(err) {
		return nil
	}
Retry:
	var (
		trashDir = mi.MakePathTrash()
		tmpDir   = filepath.Join(trashDir, fmt.Sprintf("$dir-%d", mono.NanoTime()))
	)
	if err := cmn.CreateDir(trashDir); err != nil {
		return err
	}
	if err := os.Rename(dir, tmpDir); err != nil {
		if os.IsExist(err) {
			// Slow path: `tmpDir` already exists so let's retry. It should
			// never happen but who knows...
			glog.Warningf("directory %q already exist in trash", tmpDir)
			goto Retry
		}
		if os.IsNotExist(err) {
			// Someone removed `dir` before `os.Rename`, nothing more to do.
			return nil
		}
		if err != nil {
			return err
		}
	}
	// TODO: remove and make it work when the space is extremely constrained (J)
	if debug.Enabled {
		go func() {
			if err := os.RemoveAll(tmpDir); err != nil {
				glog.Errorf("RemoveAll for %q failed with %v", tmpDir, err)
			}
		}()
	}
	return nil
}

func (mi *MountpathInfo) IsIdle(config *cmn.Config, nowTs int64) bool {
	if config == nil {
		config = cmn.GCO.Get()
	}
	curr := mfs.ios.GetMpathUtil(mi.Path, nowTs)
	return curr >= 0 && curr < config.Disk.DiskUtilLowWM
}

func (mi *MountpathInfo) CreateMissingBckDirs(bck cmn.Bck) (err error) {
	for contentType := range CSM.RegisteredContentTypes {
		dir := mi.MakePathCT(bck, contentType)
		if err = Access(dir); err == nil {
			continue
		}
		if err = cmn.CreateDir(dir); err != nil {
			return
		}
	}
	return
}

// make-path methods

func (mi *MountpathInfo) makePathBuf(bck cmn.Bck, contentType string, extra int) (buf []byte) {
	var (
		nsLen, bckNameLen, ctLen int

		provLen = 1 + 1 + len(bck.Provider)
	)
	if !bck.Ns.IsGlobal() {
		nsLen = 1
		if bck.Ns.IsRemote() {
			nsLen += 1 + len(bck.Ns.UUID)
		}
		nsLen += 1 + len(bck.Ns.Name)
	}
	if bck.Name != "" {
		bckNameLen = 1 + len(bck.Name)
	}
	if contentType != "" {
		cmn.Assert(bckNameLen > 0)
		cmn.Assert(len(contentType) == contentTypeLen)
		ctLen = 1 + 1 + contentTypeLen
	}

	buf = make([]byte, 0, len(mi.Path)+provLen+nsLen+bckNameLen+ctLen+extra)
	buf = append(buf, mi.Path...)
	buf = append(buf, filepath.Separator, prefProvider)
	buf = append(buf, bck.Provider...)
	if nsLen > 0 {
		buf = append(buf, filepath.Separator)
		if bck.Ns.IsRemote() {
			buf = append(buf, prefNsUUID)
			buf = append(buf, bck.Ns.UUID...)
		}
		buf = append(buf, prefNsName)
		buf = append(buf, bck.Ns.Name...)
	}
	if bckNameLen > 0 {
		buf = append(buf, filepath.Separator)
		buf = append(buf, bck.Name...)
	}
	if ctLen > 0 {
		buf = append(buf, filepath.Separator, prefCT)
		buf = append(buf, contentType...)
	}
	return
}

func (mi *MountpathInfo) MakePathBck(bck cmn.Bck) string {
	buf := mi.makePathBuf(bck, "", 0)
	return *(*string)(unsafe.Pointer(&buf))
}

func (mi *MountpathInfo) MakePathCT(bck cmn.Bck, contentType string) string {
	debug.AssertFunc(bck.Valid, bck)
	cmn.Assert(contentType != "")
	buf := mi.makePathBuf(bck, contentType, 0)
	return *(*string)(unsafe.Pointer(&buf))
}

func (mi *MountpathInfo) MakePathFQN(bck cmn.Bck, contentType, objName string) string {
	debug.AssertFunc(bck.Valid, bck)
	cmn.Assert(contentType != "" && objName != "")
	buf := mi.makePathBuf(bck, contentType, 1+len(objName))
	buf = append(buf, filepath.Separator)
	buf = append(buf, objName...)
	return *(*string)(unsafe.Pointer(&buf))
}

func (mi *MountpathInfo) getCapacity(config *cmn.Config, refresh bool) (c Capacity, err error) {
	if !refresh {
		mi.cmu.RLock()
		c = mi.capacity
		mi.cmu.RUnlock()
		return
	}

	mi.cmu.Lock()
	statfs := &syscall.Statfs_t{}
	if err = syscall.Statfs(mi.Path, statfs); err != nil {
		mi.cmu.Unlock()
		return
	}
	bused := statfs.Blocks - statfs.Bavail
	pct := bused * 100 / statfs.Blocks
	if pct >= uint64(config.LRU.HighWM)-1 {
		fpct := math.Ceil(float64(bused) * 100 / float64(statfs.Blocks))
		pct = uint64(fpct)
	}
	mi.capacity.Used = bused * uint64(statfs.Bsize)
	mi.capacity.Avail = statfs.Bavail * uint64(statfs.Bsize)
	mi.capacity.PctUsed = int32(pct)
	c = mi.capacity
	mi.cmu.Unlock()
	return
}

// Creates all CT directories for a given (mountpath, bck)
// NOTE: notice handling of empty dirs
func (mi *MountpathInfo) createBckDirs(bck cmn.Bck) (num int, err error) {
	for contentType := range CSM.RegisteredContentTypes {
		dir := mi.MakePathCT(bck, contentType)
		if err := Access(dir); err == nil {
			names, empty, errEmpty := IsDirEmpty(dir)
			if errEmpty != nil {
				return num, errEmpty
			}
			if !empty {
				err = fmt.Errorf("bucket %s: directory %s already exists and is not empty (%v...)",
					bck, dir, names)
				if contentType != WorkfileType {
					return num, err
				}
				glog.Warning(err)
			}
		} else if err := cmn.CreateDir(dir); err != nil {
			return num, fmt.Errorf("bucket %s: failed to create directory %s: %w", bck, dir, err)
		}
		num++
	}
	return num, nil
}

///////////////
// MountedFS //
///////////////

// create a new singleton
func Init(iostater ...ios.IOStater) {
	mfs = &MountedFS{fsIDs: make(map[syscall.Fsid]string, 10), checkFsID: true}
	if len(iostater) > 0 {
		mfs.ios = iostater[0]
	} else {
		mfs.ios = ios.NewIostatContext()
	}
}

// SetMountpaths prepares, validates, and adds configured mountpaths.
func SetMountpaths(fsPaths []string) error {
	if len(fsPaths) == 0 {
		// (usability) not to clutter the log with backtraces when starting up and validating config
		return fmt.Errorf("FATAL: no fspaths - see README => Configuration and/or fspaths section in the config.sh")
	}

	for _, path := range fsPaths {
		if err := Add(path); err != nil {
			return err
		}
	}

	return nil
}

func LoadBalanceGET(objFQN, objMpath string, copies MPI) (fqn string) {
	var (
		nowTs                = mono.NanoTime()
		mpathUtils, mpathRRs = mfs.ios.GetAllMpathUtils(nowTs)
		objUtil, ok          = mpathUtils[objMpath]
		rr, _                = mpathRRs[objMpath] // GET round-robin counter (zeros out every iostats refresh i-val)
		util                 = objUtil
		r                    = rr
	)
	fqn = objFQN
	if !ok {
		// Only assert when `mpathUtils` is non-empty. If it's empty it means
		// that `fs2disks` returned empty response so there is no way to get utils.
		debug.AssertMsg(len(mpathUtils) == 0, objMpath)
		return
	}
	for copyFQN, copyMPI := range copies {
		var (
			u        int64
			c, rrcnt int32
		)
		if u, ok = mpathUtils[copyMPI.Path]; !ok {
			continue
		}
		if r, ok = mpathRRs[copyMPI.Path]; !ok {
			if u < util {
				fqn, util, rr = copyFQN, u, r
			}
			continue
		}
		c = r.Load()
		if rr != nil {
			rrcnt = rr.Load()
		}
		if u < util && c <= rrcnt { // the obvious choice
			fqn, util, rr = copyFQN, u, r
			continue
		}
		if u+int64(c)*uQuantum < util+int64(rrcnt)*uQuantum { // heuristics - make uQuantum configurable?
			fqn, util, rr = copyFQN, u, r
		}
	}
	// NOTE: the counter could've been already inc-ed
	//       could keep track of the second best and use CAS to recerve-inc and compare
	//       can wait though
	if rr != nil {
		rr.Inc()
	}
	return
}

// ios delegators
func GetMpathUtil(mpath string, nowTs int64) int64 {
	return mfs.ios.GetMpathUtil(mpath, nowTs)
}

func GetAllMpathUtils(nowTs int64) (utils map[string]int64) {
	utils, _ = mfs.ios.GetAllMpathUtils(nowTs)
	return
}

func LogAppend(lines []string) []string {
	return mfs.ios.LogAppend(lines)
}

func GetSelectedDiskStats() (m map[string]*ios.SelectedDiskStats) {
	return mfs.ios.GetSelectedDiskStats()
}

// DisableFsIDCheck disables fsid checking when adding new mountpath
func DisableFsIDCheck() { mfs.checkFsID = false }

// Returns number of available mountpaths
func NumAvail() int {
	availablePaths := (*MPI)(mfs.available.Load())
	return len(*availablePaths)
}

func updatePaths(available, disabled MPI) {
	mfs.available.Store(unsafe.Pointer(&available))
	mfs.disabled.Store(unsafe.Pointer(&disabled))
}

// Add adds new mountpath to the target's mountpaths.
// FIXME: unify error messages for original and clean mountpath
func Add(mpath string) error {
	cleanMpath, err := cmn.ValidateMpath(mpath)
	if err != nil {
		return err
	}
	if err := Access(cleanMpath); err != nil {
		return fmt.Errorf("fspath %q %s, err: %v", mpath, cmn.DoesNotExist, err)
	}
	statfs := syscall.Statfs_t{}
	if err := syscall.Statfs(cleanMpath, &statfs); err != nil {
		return fmt.Errorf("cannot statfs fspath %q, err: %w", mpath, err)
	}

	fs, err := fqn2fsAtStartup(cleanMpath)
	if err != nil {
		return fmt.Errorf("cannot get filesystem: %v", err)
	}

	mp := newMountpath(cleanMpath, mpath, statfs.Fsid, fs)

	mfs.mu.Lock()
	defer mfs.mu.Unlock()

	availablePaths, disabledPaths := mountpathsCopy()
	if _, exists := availablePaths[mp.Path]; exists {
		return fmt.Errorf("tried to add already registered mountpath: %v", mp.Path)
	}

	if existingPath, exists := mfs.fsIDs[mp.Fsid]; exists && mfs.checkFsID {
		return fmt.Errorf("tried to add path %v but same fsid (%v) was already registered by %v",
			mpath, mp.Fsid, existingPath)
	}

	mfs.ios.AddMpath(mp.Path, mp.FileSystem)

	availablePaths[mp.Path] = mp
	mfs.fsIDs[mp.Fsid] = cleanMpath
	updatePaths(availablePaths, disabledPaths)
	return nil
}

// mountpathsCopy returns a shallow copy of current mountpaths
func mountpathsCopy() (MPI, MPI) {
	availablePaths, disabledPaths := Get()
	availableCopy := make(MPI, len(availablePaths))
	disabledCopy := make(MPI, len(availablePaths))

	for mpath, mpathInfo := range availablePaths {
		availableCopy[mpath] = mpathInfo
	}
	for mpath, mpathInfo := range disabledPaths {
		disabledCopy[mpath] = mpathInfo
	}
	return availableCopy, disabledCopy
}

// Remove removes mountpaths from the target's mountpaths. It searches
// for the mountpath in `available` and, if not found, in `disabled`.
func Remove(mpath string) error {
	var (
		mp     *MountpathInfo
		exists bool
	)

	mfs.mu.Lock()
	defer mfs.mu.Unlock()

	cleanMpath, err := cmn.ValidateMpath(mpath)
	if err != nil {
		return err
	}

	availablePaths, disabledPaths := mountpathsCopy()
	if mp, exists = availablePaths[cleanMpath]; !exists {
		if mp, exists = disabledPaths[cleanMpath]; !exists {
			return fmt.Errorf("tried to remove non-existing mountpath: %v", mpath)
		}

		delete(disabledPaths, cleanMpath)
		delete(mfs.fsIDs, mp.Fsid)
		updatePaths(availablePaths, disabledPaths)
		return nil
	}

	delete(availablePaths, cleanMpath)
	mfs.ios.RemoveMpath(cleanMpath)
	delete(mfs.fsIDs, mp.Fsid)

	go mp.evictLomCache()

	if l := len(availablePaths); l == 0 {
		glog.Errorf("removed the last available mountpath %s", mp)
	} else {
		glog.Infof("removed mountpath %s (%d remain(s) active)", mp, l)
	}

	updatePaths(availablePaths, disabledPaths)
	return nil
}

// Enable enables previously disabled mountpath. enabled is set to
// true if mountpath has been moved from disabled to available and exists is
// set to true if such mountpath even exists.
func Enable(mpath string) (enabled bool, err error) {
	mfs.mu.Lock()
	defer mfs.mu.Unlock()

	cleanMpath, err := cmn.ValidateMpath(mpath)
	if err != nil {
		return false, err
	}
	availablePaths, disabledPaths := mountpathsCopy()
	if _, ok := availablePaths[cleanMpath]; ok {
		return false, nil
	}
	if mp, ok := disabledPaths[cleanMpath]; ok {
		availablePaths[cleanMpath] = mp
		mfs.ios.AddMpath(cleanMpath, mp.FileSystem)
		delete(disabledPaths, cleanMpath)
		updatePaths(availablePaths, disabledPaths)
		return true, nil
	}

	return false, cmn.NewNoMountpathError(mpath)
}

// Disable disables an available mountpath. disabled is set to true if
// mountpath has been moved from available to disabled and exists is set to
// true if such mountpath even exists.
func Disable(mpath string) (disabled bool, err error) {
	mfs.mu.Lock()
	defer mfs.mu.Unlock()

	cleanMpath, err := cmn.ValidateMpath(mpath)
	if err != nil {
		return false, err
	}

	availablePaths, disabledPaths := mountpathsCopy()
	if mpathInfo, ok := availablePaths[cleanMpath]; ok {
		disabledPaths[cleanMpath] = mpathInfo
		mfs.ios.RemoveMpath(cleanMpath)
		delete(availablePaths, cleanMpath)
		updatePaths(availablePaths, disabledPaths)
		if l := len(availablePaths); l == 0 {
			glog.Errorf("disabled the last available mountpath %s", mpathInfo)
		} else {
			glog.Infof("disabled mountpath %s (%d remain(s) active)", mpathInfo, l)
		}
		go mpathInfo.evictLomCache()
		return true, nil
	}
	if _, ok := disabledPaths[cleanMpath]; ok {
		return false, nil
	}
	return false, cmn.NewNoMountpathError(mpath)
}

// Mountpaths returns both available and disabled mountpaths.
func Get() (MPI, MPI) {
	var (
		availablePaths = (*MPI)(mfs.available.Load())
		disabledPaths  = (*MPI)(mfs.disabled.Load())
	)
	if availablePaths == nil {
		tmp := make(MPI, 10)
		availablePaths = &tmp
	}
	if disabledPaths == nil {
		tmp := make(MPI, 10)
		disabledPaths = &tmp
	}
	return *availablePaths, *disabledPaths
}

func CreateBuckets(op string, bcks ...cmn.Bck) (errs []error) {
	var (
		availablePaths, _ = Get()
		totalDirs         = len(availablePaths) * len(bcks) * len(CSM.RegisteredContentTypes)
		totalCreatedDirs  int
	)
	for _, mi := range availablePaths {
		for _, bck := range bcks {
			num, err := mi.createBckDirs(bck)
			if err != nil {
				errs = append(errs, err)
			} else {
				totalCreatedDirs += num
			}
		}
	}
	if errs == nil && totalCreatedDirs != totalDirs {
		errs = append(errs, fmt.Errorf("failed to create %d out of %d buckets' directories: %v",
			totalDirs-totalCreatedDirs, totalDirs, bcks))
	}
	if errs == nil && glog.FastV(4, glog.SmoduleFS) {
		glog.Infof("%s(create bucket dirs): %v, num=%d", op, bcks, totalDirs)
	}
	return
}

func DestroyBuckets(op string, bcks ...cmn.Bck) error {
	const destroyStr = "destroy-ais-bucket-dir"
	var (
		availablePaths, _  = Get()
		totalDirs          = len(availablePaths) * len(bcks)
		totalDestroyedDirs = 0
	)
	for _, mpathInfo := range availablePaths {
		for _, bck := range bcks {
			dir := mpathInfo.MakePathBck(bck)
			if err := mpathInfo.MoveToTrash(dir); err != nil {
				glog.Errorf("%s: failed to %s (dir: %q, err: %v)", op, destroyStr, dir, err)
			} else {
				totalDestroyedDirs++
			}
		}
	}
	if totalDestroyedDirs != totalDirs {
		return fmt.Errorf("failed to destroy %d out of %d buckets' directories: %v",
			totalDirs-totalDestroyedDirs, totalDirs, bcks)
	}
	if glog.FastV(4, glog.SmoduleFS) {
		glog.Infof("%s: %s (buckets %v, num dirs %d)", op, destroyStr, bcks, totalDirs)
	}
	return nil
}

func RenameBucketDirs(bckFrom, bckTo cmn.Bck) (err error) {
	availablePaths, _ := Get()
	renamed := make([]*MountpathInfo, 0, len(availablePaths))
	for _, mpathInfo := range availablePaths {
		fromPath := mpathInfo.MakePathBck(bckFrom)
		toPath := mpathInfo.MakePathBck(bckTo)

		// os.Rename fails when renaming to a directory which already exists.
		// We should remove destination bucket directory before rename. It's reasonable to do so
		// as all targets agreed to rename and rename was committed in BMD.
		os.RemoveAll(toPath)
		if err = os.Rename(fromPath, toPath); err != nil {
			break
		}
		renamed = append(renamed, mpathInfo)
	}

	if err == nil {
		return
	}
	for _, mpathInfo := range renamed {
		fromPath := mpathInfo.MakePathBck(bckTo)
		toPath := mpathInfo.MakePathBck(bckFrom)
		if erd := os.Rename(fromPath, toPath); erd != nil {
			glog.Error(erd)
		}
	}
	return
}

// capacity management

func GetCapStatus() (cs CapStatus) {
	mfs.cmu.RLock()
	cs = mfs.capStatus
	mfs.cmu.RUnlock()
	return
}

func RefreshCapStatus(config *cmn.Config, mpcap MPCap) (cs CapStatus, err error) {
	var (
		availablePaths, _ = Get()
		c                 Capacity
	)
	if len(availablePaths) == 0 {
		err = errors.New(cmn.NoMountpaths)
		return
	}
	if config == nil {
		config = cmn.GCO.Get()
	}
	high, oos := config.LRU.HighWM, config.LRU.OOS
	for path, mi := range availablePaths {
		if c, err = mi.getCapacity(config, true); err != nil {
			glog.Error(err) // TODO: handle
			return
		}
		cs.TotalUsed += c.Used
		cs.TotalAvail += c.Avail
		cs.PctMax = cmn.MaxI32(cs.PctMax, c.PctUsed)
		cs.PctAvg += c.PctUsed
		if mpcap != nil {
			mpcap[path] = c
		}
	}
	cs.PctAvg /= int32(len(availablePaths))
	cs.OOS = int64(cs.PctMax) > oos
	if cs.OOS || int64(cs.PctMax) > high {
		cs.Err = cmn.NewErrorCapacityExceeded(high, cs.PctMax, cs.OOS)
	}
	// cached cap state
	mfs.cmu.Lock()
	mfs.capStatus = cs
	mfs.capTime.curr = mono.NanoTime()
	mfs.capTime.next = mfs.capTime.curr + int64(nextRefresh(config))
	mfs.cmu.Unlock()
	return
}

// recompute next time to refresh cached capacity stats (mfs.capStatus)
func nextRefresh(config *cmn.Config) time.Duration {
	var (
		util = int64(mfs.capStatus.PctAvg) // NOTE: average not max
		umin = cmn.MaxI64(config.LRU.HighWM-10, config.LRU.LowWM)
		umax = config.LRU.OOS
		tmax = config.LRU.CapacityUpdTime
		tmin = config.Periodic.StatsTime
	)
	if util <= umin {
		return config.LRU.CapacityUpdTime
	}
	if util >= umax {
		return config.Periodic.StatsTime
	}
	debug.Assert(umin < umax)
	debug.Assert(tmin < tmax)
	ratio := (util - umin) * 100 / (umax - umin)
	return time.Duration(ratio)*(tmax-tmin)/100 + tmin
}

// NOTE: is called only and exclusively by `stats.Trunner` providing
//       `config.Periodic.StatsTime` tick
func CapPeriodic(mpcap MPCap) (cs CapStatus, err error, updated bool) {
	config := cmn.GCO.Get()
	mfs.capTime.curr += int64(config.Periodic.StatsTime)
	if mfs.capTime.curr < mfs.capTime.next {
		return
	}
	cs, err = RefreshCapStatus(config, mpcap)
	updated = true
	return
}

// a slightly different view of the same
func CapStatusAux() (fsInfo cmn.CapacityInfo) {
	cs := GetCapStatus()
	fsInfo.Used = cs.TotalUsed
	fsInfo.Total = cs.TotalUsed + cs.TotalAvail
	fsInfo.PctUsed = float64(cs.PctAvg)
	return
}
