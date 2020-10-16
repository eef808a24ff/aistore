// Package commands provides the set of CLI commands used to communicate with the AIS cluster.
// This file handles commands related to specific (not supported for other entities) object actions.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package commands

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/urfave/cli"
)

var (
	objectSpecificCmdsFlags = map[string][]cli.Flag{
		commandEvict: append(
			baseLstRngFlags,
			dryRunFlag,
		),
		commandGet: {
			offsetFlag,
			lengthFlag,
			checksumFlag,
			isCachedFlag,
			forceFlag,
		},
		commandPut: append(
			checksumFlags,
			chunkSizeFlag,
			concurrencyFlag,
			dryRunFlag,
			progressBarFlag,
			recursiveFlag,
			refreshFlag,
			verboseFlag,
			yesFlag,
			computeCksumFlag,
		),
		commandPromote: {
			recursiveFlag,
			overwriteFlag,
			keepOrigFlag,
			targetFlag,
			verboseFlag,
		},
		commandConcat: {
			recursiveFlag,
			progressBarFlag,
		},
		commandCat: {
			offsetFlag,
			lengthFlag,
			checksumFlag,
			forceFlag,
		},
	}

	objectSpecificCmds = []cli.Command{
		{
			Name:         commandEvict,
			Usage:        "evict objects from the cache",
			ArgsUsage:    optionalObjectsArgument,
			Flags:        objectSpecificCmdsFlags[commandEvict],
			Action:       evictHandler,
			BashComplete: bucketCompletions(bckCompletionsOpts{multiple: true}),
		},
		{
			Name:         commandGet,
			Usage:        "get the object from the specified bucket",
			ArgsUsage:    getObjectArgument,
			Flags:        objectSpecificCmdsFlags[commandGet],
			Action:       getHandler,
			BashComplete: bucketCompletions(bckCompletionsOpts{separator: true}),
		},
		{
			Name:         commandPut,
			Usage:        "put the objects to the specified bucket",
			ArgsUsage:    putPromoteObjectArgument,
			Flags:        objectSpecificCmdsFlags[commandPut],
			Action:       putHandler,
			BashComplete: putPromoteObjectCompletions,
		},
		{
			Name:         commandPromote,
			Usage:        "promote AIStore-local files and directories to objects",
			ArgsUsage:    putPromoteObjectArgument,
			Flags:        objectSpecificCmdsFlags[commandPromote],
			Action:       promoteHandler,
			BashComplete: putPromoteObjectCompletions,
		},
		{
			Name:      commandConcat,
			Usage:     "concatenate multiple files one by one into new, single object to the specified bucket",
			ArgsUsage: concatObjectArgument,
			Flags:     objectSpecificCmdsFlags[commandConcat],
			Action:    concatHandler,
		},
		{
			Name:         commandCat,
			Usage:        "gets object from the specified bucket and prints it to STDOUT; alias for ais get BUCKET_NAME/OBJECT_NAME -",
			ArgsUsage:    objectArgument,
			Flags:        objectSpecificCmdsFlags[commandCat],
			Action:       catHandler,
			BashComplete: bucketCompletions(bckCompletionsOpts{separator: true}),
		},
	}
)

func prefetchHandler(c *cli.Context) (err error) {
	var (
		bck     cmn.Bck
		objName string
		objPath string
	)

	printDryRunHeader(c)

	if c.NArg() == 0 {
		return incorrectUsageMsg(c, "missing bucket name")
	}
	if c.NArg() > 1 {
		return incorrectUsageMsg(c, "too many arguments")
	}
	objPath = c.Args().First()
	if isWebURL(objPath) {
		bck = parseURLtoBck(objPath)
	} else if bck, objName, err = parseBckObjectURI(c, objPath); err != nil {
		return
	}
	if bck.IsAIS() {
		return fmt.Errorf("cannot prefetch from ais buckets (the operation applies to Cloud buckets only)")
	}
	if bck, _, err = validateBucket(c, bck, "", false); err != nil {
		return
	}
	// FIXME: it can be easily handled
	if objName != "" {
		return incorrectUsageMsg(c, "object name not supported, use list flag or range flag")
	}

	if flagIsSet(c, listFlag) || flagIsSet(c, templateFlag) {
		return listOrRangeOp(c, commandPrefetch, bck)
	}

	return missingArgumentsError(c, "object list or range")
}

func evictHandler(c *cli.Context) (err error) {
	var (
		bck     cmn.Bck
		objName string
	)
	printDryRunHeader(c)

	if c.NArg() == 0 {
		return incorrectUsageMsg(c, "missing bucket name")
	}

	// default bucket or bucket argument given by the user
	if c.NArg() == 1 {
		objPath := c.Args().First()
		if isWebURL(objPath) {
			bck = parseURLtoBck(objPath)
		} else if bck, objName, err = parseBckObjectURI(c, objPath); err != nil {
			return
		}

		if bck.IsAIS() {
			return fmt.Errorf("cannot evict ais buckets (the operation applies to Cloud buckets only)")
		}

		if bck, _, err = validateBucket(c, bck, "", false); err != nil {
			return err
		}

		if flagIsSet(c, listFlag) || flagIsSet(c, templateFlag) {
			if objName != "" {
				return incorrectUsageMsg(c, "object name (%q) not supported when list or template flag provided", objName)
			}
			// list or range operation on a given bucket
			return listOrRangeOp(c, commandEvict, bck)
		}

		if objName == "" {
			// operation on a given bucket
			return evictBucket(c, bck)
		}

		// evict single object from cloud bucket - multiObjOp will handle
	}

	// list and range flags are invalid with object argument(s)
	if flagIsSet(c, listFlag) || flagIsSet(c, templateFlag) {
		return incorrectUsageMsg(c, "flags %q are invalid when object names provided", strings.Join([]string{listFlag.Name, templateFlag.Name}, ", "))
	}

	// object argument(s) given by the user; operation on given object(s)
	return multiObjOp(c, commandEvict)
}

func getHandler(c *cli.Context) (err error) {
	outFile := c.Args().Get(1) // empty string if arg not given
	return getObject(c, outFile, false /*silent*/)
}

func putHandler(c *cli.Context) (err error) {
	var (
		bck         cmn.Bck
		p           *cmn.BucketProps
		objName     string
		fileName    = c.Args().Get(0)
		fullObjName = c.Args().Get(1)
	)
	if c.NArg() < 1 {
		return missingArgumentsError(c, "file to upload", "object name in the form bucket/[object]")
	}
	if c.NArg() < 2 {
		return missingArgumentsError(c, "object name in the form bucket/[object]")
	}
	bck, objName, err = parseBckObjectURI(c, fullObjName)
	if err != nil {
		return
	}

	if bck, p, err = validateBucket(c, bck, fullObjName, false); err != nil {
		return
	}

	return putObject(c, bck, objName, fileName, p.Cksum.Type)
}

func concatHandler(c *cli.Context) (err error) {
	var (
		bck     cmn.Bck
		objName string
	)
	if c.NArg() < 1 {
		return missingArgumentsError(c, "at least one file to upload", "object name in the form bucket/[object]")
	}
	if c.NArg() < 2 {
		return missingArgumentsError(c, "object name in the form bucket/object")
	}

	fullObjName := c.Args().Get(len(c.Args()) - 1)
	fileNames := make([]string, len(c.Args())-1)
	for i := 0; i < len(c.Args())-1; i++ {
		fileNames[i] = c.Args().Get(i)
	}

	bck, objName, err = parseBckObjectURI(c, fullObjName)
	if err != nil {
		return
	}
	if objName == "" {
		return fmt.Errorf("object name is required")
	}
	if bck, _, err = validateBucket(c, bck, fullObjName, false); err != nil {
		return
	}

	return concatObject(c, bck, objName, fileNames)
}

func promoteHandler(c *cli.Context) (err error) {
	var (
		bck         cmn.Bck
		objName     string
		fqn         = c.Args().Get(0)
		fullObjName = c.Args().Get(1)
	)
	if c.NArg() < 1 {
		return missingArgumentsError(c, "file|directory to promote")
	}
	if c.NArg() < 2 {
		return missingArgumentsError(c, "object name in the form bucket/[object]")
	}
	if !filepath.IsAbs(fqn) {
		return incorrectUsageMsg(c, "promoted source (file or directory) must have an absolute path")
	}

	bck, objName, err = parseBckObjectURI(c, fullObjName)
	if err != nil {
		return
	}
	if bck, _, err = validateBucket(c, bck, fullObjName, false); err != nil {
		return
	}
	return promoteFileOrDir(c, bck, objName, fqn)
}

func catHandler(c *cli.Context) (err error) {
	return getObject(c, fileStdIO, true /*silent*/)
}
