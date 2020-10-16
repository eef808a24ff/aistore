// Package cmn provides common low-level types and utilities for all aistore projects
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package cmn

import (
	"fmt"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
)

const (
	// Number of sync maps
	MultiSyncMapCount = 0x40
)

type (
	// TimeoutGroup is similar to sync.WaitGroup with the difference on Wait
	// where we only allow timing out.
	//
	// WARNING: It should not be used in critical code as it may have worse
	// performance than sync.WaitGroup - use only if its needed.
	//
	// WARNING: It is not safe to wait on completion in multiple threads!
	//
	// WARNING: It is not recommended to reuse the TimeoutGroup - it was not
	// designed for that and bugs can be expected, especially when previous
	// group was not called with successful (without timeout) WaitTimeout.
	TimeoutGroup struct {
		jobsLeft  atomic.Int32 // counter for jobs left to be done
		postedFin atomic.Int32 // determines if we have already posted fin signal
		fin       chan struct{}
	}

	// StopCh is specialized channel for stopping things.
	StopCh struct {
		once sync.Once
		ch   chan struct{}
	}

	// DynSemaphore implements sempahore which can change its size during usage.
	DynSemaphore struct {
		size int
		cur  int
		c    *sync.Cond
		mu   sync.Mutex
	}

	// LimitedWaitGroup is helper struct which combines standard wait group and
	// semaphore to limit the number of goroutines created.
	LimitedWaitGroup struct {
		wg   *sync.WaitGroup
		sema *DynSemaphore
	}

	MultiSyncMap struct {
		M [MultiSyncMapCount]sync.Map
	}
)

func NewTimeoutGroup() *TimeoutGroup {
	return &TimeoutGroup{
		fin: make(chan struct{}, 1),
	}
}

func (tg *TimeoutGroup) Add(delta int) {
	tg.jobsLeft.Add(int32(delta))
}

// Wait waits until jobs are finished.
//
// NOTE: Wait can be only invoked after all Adds!
func (tg *TimeoutGroup) Wait() {
	tg.WaitTimeoutWithStop(24*time.Hour, nil)
}

// WaitTimeout waits until jobs are finished or timed out.
// In case of timeout it returns true.
//
// NOTE: WaitTimeout can be only invoked after all Adds!
func (tg *TimeoutGroup) WaitTimeout(timeout time.Duration) bool {
	timed, _ := tg.WaitTimeoutWithStop(timeout, nil)
	return timed
}

// WaitTimeoutWithStop waits until jobs are finished, timed out, or received
// signal on stop channel. When channel is nil it is equivalent to WaitTimeout.
//
// NOTE: WaitTimeoutWithStop can be only invoked after all Adds!
func (tg *TimeoutGroup) WaitTimeoutWithStop(timeout time.Duration, stop <-chan struct{}) (timed, stopped bool) {
	t := time.NewTimer(timeout)
	select {
	case <-tg.fin:
		tg.postedFin.Store(0)
		timed, stopped = false, false
	case <-t.C:
		timed, stopped = true, false
	case <-stop:
		timed, stopped = false, true
	}
	t.Stop()
	return
}

// Done decrements number of jobs left to do. Panics if the number jobs left is
// less than 0.
func (tg *TimeoutGroup) Done() {
	if left := tg.jobsLeft.Dec(); left == 0 {
		if posted := tg.postedFin.Swap(1); posted == 0 {
			tg.fin <- struct{}{}
		}
	} else if left < 0 {
		AssertMsg(false, fmt.Sprintf("jobs left is below zero: %d", left))
	}
}

func NewStopCh() *StopCh {
	return &StopCh{
		ch: make(chan struct{}, 1),
	}
}

func (sc *StopCh) Listen() <-chan struct{} {
	return sc.ch
}

func (sc *StopCh) Close() {
	sc.once.Do(func() {
		close(sc.ch)
	})
}

func NewDynSemaphore(n int) *DynSemaphore {
	sema := &DynSemaphore{
		size: n,
	}
	sema.c = sync.NewCond(&sema.mu)
	return sema
}

func (s *DynSemaphore) Size() int {
	s.mu.Lock()
	size := s.size
	s.mu.Unlock()
	return size
}

func (s *DynSemaphore) SetSize(n int) {
	Assert(n >= 1)
	s.mu.Lock()
	s.size = n
	s.mu.Unlock()
}

func (s *DynSemaphore) Acquire(cnts ...int) {
	cnt := 1
	if len(cnts) > 0 {
		cnt = cnts[0]
	}
	s.mu.Lock()
check:
	if s.cur+cnt <= s.size {
		s.cur += cnt
		s.mu.Unlock()
		return
	}

	// Wait for vacant place(s)
	s.c.Wait()
	goto check
}

func (s *DynSemaphore) Release(cnts ...int) {
	cnt := 1
	if len(cnts) > 0 {
		cnt = cnts[0]
	}

	s.mu.Lock()

	Assert(s.cur >= cnt)

	s.cur -= cnt
	s.c.Signal()
	s.mu.Unlock()
}

func NewLimitedWaitGroup(n int) *LimitedWaitGroup {
	return &LimitedWaitGroup{
		wg:   &sync.WaitGroup{},
		sema: NewDynSemaphore(n),
	}
}

func (wg *LimitedWaitGroup) Add(n int) {
	wg.wg.Add(n)
	wg.sema.Acquire(n)
}

func (wg *LimitedWaitGroup) Done() {
	wg.wg.Done()
	wg.sema.Release()
}

func (wg *LimitedWaitGroup) Wait() {
	wg.wg.Wait()
}

func (msm *MultiSyncMap) Get(idx int) *sync.Map {
	Assert(idx >= 0 && idx < MultiSyncMapCount)
	return &msm.M[idx]
}

func (msm *MultiSyncMap) GetByHash(hash uint32) *sync.Map {
	return &msm.M[hash%MultiSyncMapCount]
}
