// Package dbdriver provides a local database server for the AIStore object storage.
/*
 * Copyright (c) 2020, NVIDIA CORPORATION. All rights reserved.
 */
package dbdriver

import (
	"strings"

	"github.com/NVIDIA/aistore/cmn"
	jsoniter "github.com/json-iterator/go"
	"github.com/tidwall/buntdb"
)

// BuntDB:
// At this moment, BuntDB runs with the following settings:
// - calls filesystem sync every second (if we want to do it less frequently
//   may add `Flush` to `Driver` interface and call in Get/Set/Delete/List
//   when a certain number of seconds pass. There is not way to do it in easier
//   way with built-in stuff: either Never or every second)
// - after database size exceeds `autoShrinkSize`(1MB), BuntDB starts compacting
//   the database periodically(when it grows over a given limit)
// - compacting is executed when the database size is greater than the size
//   after previous compacting by `AutoShrinkPercentage`(50%)

const (
	autoShrinkSize = cmn.MiB
	collectionSepa = "##"
)

type (
	BuntDriver struct {
		driver *buntdb.DB
	}
)

var _ Driver = &BuntDriver{}

func NewBuntDB(path string) (*BuntDriver, error) {
	driver, err := buntdb.Open(path)
	if err != nil {
		return nil, err
	}
	driver.SetConfig(buntdb.Config{
		SyncPolicy:           buntdb.EverySecond, // periodical FS sync
		AutoShrinkMinSize:    autoShrinkSize,     // start autoShrink only of file exceeds the size
		AutoShrinkPercentage: 50,                 // run compacting when DB grows by half
	})
	return &BuntDriver{driver: driver}, nil
}

// Convert original DB error to `dbdriver` package ones for clients
func buntToCommonErr(err error, collection, key string) error {
	if err == buntdb.ErrNotFound {
		return NewErrNotFound(collection, key)
	}
	return err
}

// Create "unique" key from collection and key, so there was no trouble when
// there is an overlap. E.g, if key and collection uses the same separator
// for subkeys, two pairs ("abc", "def/ghi") and ("abc/def", "ghi") generate
// the same full path. The function should make them different.
func makePath(collection, key string) string {
	if strings.HasSuffix(collection, "##") {
		return collection + key
	}
	return collection + collectionSepa + key
}

func (bd *BuntDriver) Close() error {
	return bd.driver.Close()
}

func (bd *BuntDriver) Set(collection, key string, object interface{}) error {
	b := cmn.MustMarshal(object)
	return bd.SetString(collection, key, string(b))
}

func (bd *BuntDriver) Get(collection, key string, object interface{}) error {
	s, err := bd.GetString(collection, key)
	if err != nil {
		return err
	}
	return jsoniter.Unmarshal([]byte(s), object)
}

func (bd *BuntDriver) SetString(collection, key, data string) error {
	name := makePath(collection, key)
	return bd.driver.Update(func(tx *buntdb.Tx) error {
		_, _, err := tx.Set(name, data, nil)
		return err
	})
}

func (bd *BuntDriver) GetString(collection, key string) (string, error) {
	var value string
	name := makePath(collection, key)
	err := bd.driver.View(func(tx *buntdb.Tx) error {
		var err error
		value, err = tx.Get(name)
		return err
	})
	return value, buntToCommonErr(err, collection, key)
}

func (bd *BuntDriver) Delete(collection, key string) error {
	name := makePath(collection, key)
	err := bd.driver.Update(func(tx *buntdb.Tx) error {
		_, err := tx.Delete(name)
		return err
	})
	return buntToCommonErr(err, collection, key)
}

func (bd *BuntDriver) List(collection, pattern string) ([]string, error) {
	var (
		keys   = make([]string, 0)
		filter string
	)
	if !strings.Contains(pattern, "*") && !strings.Contains(pattern, "?") {
		pattern += "*"
	}
	filter = makePath(collection, pattern)
	err := bd.driver.View(func(tx *buntdb.Tx) error {
		tx.AscendKeys(filter, func(key, _ string) bool {
			keys = append(keys, key)
			return true
		})
		return nil
	})
	return keys, buntToCommonErr(err, collection, "")
}

func (bd *BuntDriver) DeleteCollection(collection string) error {
	keys, err := bd.List(collection, "")
	if err != nil || len(keys) == 0 {
		return err
	}
	return bd.driver.Update(func(tx *buntdb.Tx) error {
		for _, k := range keys {
			_, err := tx.Delete(k)
			if err != nil && err != buntdb.ErrNotFound {
				return err
			}
		}
		return nil
	})
}