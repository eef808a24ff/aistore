// Package ios is a collection of interfaces to the local storage subsystem;
// the package includes OS-dependent implementations for those interfaces.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package ios

type (
	diskBlockStat interface {
		ReadBytes() int64
		WriteBytes() int64

		IOMs() int64
		WriteMs() int64
		ReadMs() int64
	}

	diskBlockStats map[string]diskBlockStat
)
