// Package ios is a collection of interfaces to the local storage subsystem;
// the package includes OS-dependent implementations for those interfaces.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package ios

import (
	"github.com/NVIDIA/aistore/3rdparty/atomic"
)

// interface guard
var _ IOStater = &IOStaterMock{}

type (
	IOStaterMock struct {
		Utils map[string]int64
	}
)

func NewIOStaterMock() *IOStaterMock {
	return &IOStaterMock{
		Utils: make(map[string]int64, 10),
	}
}

func (m *IOStaterMock) GetMpathUtil(mpath string, nowTs int64) int64 { return m.Utils[mpath] }
func (m *IOStaterMock) GetAllMpathUtils(nowTs int64) (map[string]int64, map[string]*atomic.Int32) {
	return m.Utils, nil
}
func (m *IOStaterMock) AddMpath(mpath, fs string)                           {}
func (m *IOStaterMock) RemoveMpath(mpath string)                            {}
func (m *IOStaterMock) LogAppend(l []string) []string                       { return l }
func (m *IOStaterMock) GetSelectedDiskStats() map[string]*SelectedDiskStats { return nil }
