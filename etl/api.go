// Package etl provides utilities to initialize and use transformation pods.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package etl

import (
	"errors"
	"fmt"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/etl/runtime"
)

type (
	InitMsg struct {
		ID          string           `json:"id"`
		Spec        []byte           `json:"spec"`
		CommType    string           `json:"communication_type"`
		WaitTimeout cmn.DurationJSON `json:"wait_timeout"`
	}

	BuildMsg struct {
		ID          string           `json:"id"`
		Code        []byte           `json:"code"`
		Deps        []byte           `json:"dependencies"`
		Runtime     string           `json:"runtime"`
		WaitTimeout cmn.DurationJSON `json:"wait_timeout"`
	}

	Info struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}

	PodsLogsMsg []PodLogsMsg
	PodLogsMsg  struct {
		TargetID string `json:"target_id"`
		Logs     []byte `json:"logs"`
	}

	OfflineMsg struct {
		ID     string `json:"id"`      // ETL ID
		Prefix string `json:"prefix"`  // Prefix added to each resulting object.
		DryRun bool   `json:"dry_run"` // Don't perform any PUT

		// New objects names will have this extension. Warning: if in a source
		// bucket exist two objects with the same base name, but different
		// extension, specifying this field might cause object overriding.
		// This is because of resulting name conflict.
		Ext string `json:"ext"`
	}
)

var ErrMissingUUID = errors.New("ETL UUID can't be empty")

func (m BuildMsg) Validate() error {
	if len(m.Code) == 0 {
		return fmt.Errorf("source code is empty")
	}
	if m.Runtime == "" {
		return fmt.Errorf("runtime is not specified")
	}
	if _, ok := runtime.Runtimes[m.Runtime]; !ok {
		return fmt.Errorf("unsupported runtime provided: %s", m.Runtime)
	}
	return nil
}

func (p PodsLogsMsg) Len() int           { return len(p) }
func (p PodsLogsMsg) Less(i, j int) bool { return p[i].TargetID < p[j].TargetID }
func (p PodsLogsMsg) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
