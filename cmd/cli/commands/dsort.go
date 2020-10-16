// Package commands provides the set of CLI commands used to communicate with the AIS cluster.
// This file handles commands that interact with objects in the cluster
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package commands

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/hex"
	jsonStd "encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cmd/cli/templates"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/dsort"
	"github.com/NVIDIA/aistore/memsys"
	"github.com/urfave/cli"
	"github.com/vbauerster/mpb/v4"
	"github.com/vbauerster/mpb/v4/decor"
	"golang.org/x/sync/errgroup"
)

var (
	phasesOrdered = []string{
		dsort.ExtractionPhase,
		dsort.SortingPhase,
		dsort.CreationPhase,
	}

	dSortCmdsFlags = map[string][]cli.Flag{
		commandGenShards: {
			extFlag,
			dsortBucketFlag,
			dsortTemplateFlag,
			fileSizeFlag,
			fileCountFlag,
			cleanupFlag,
			concurrencyFlag,
		},
	}

	dSortCmds = []cli.Command{
		{
			Name:      commandGenShards,
			Usage:     fmt.Sprintf("put randomly generated shards that can be used for %s testing", cmn.DSortName),
			ArgsUsage: noArguments,
			Flags:     dSortCmdsFlags[commandGenShards],
			Action:    genShardsHandler,
		},
	}
)

func phaseToBarText(phase string) string {
	return strings.Title(phase) + " phase: "
}

func createTar(w io.Writer, ext string, start, end, fileCnt int, fileSize int64) error {
	var (
		gzw       *gzip.Writer
		tw        *tar.Writer
		random    = cmn.NowRand()
		buf       = make([]byte, fileSize)
		randBytes = make([]byte, 10)
	)

	if ext == ".tgz" {
		gzw = gzip.NewWriter(w)
		tw = tar.NewWriter(gzw)
		defer gzw.Close()
	} else {
		tw = tar.NewWriter(w)
	}
	defer tw.Close()

	for fileNum := start; fileNum < end; fileNum++ {
		// Generate random name
		random.Read(randBytes)
		name := fmt.Sprintf("%s-%0*d.test", hex.EncodeToString(randBytes), len(strconv.Itoa(fileCnt)), fileNum)

		h := &tar.Header{
			Typeflag: tar.TypeReg,
			Size:     fileSize,
			Name:     name,
			Uid:      os.Getuid(),
			Gid:      os.Getgid(),
			Mode:     0o664,
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}

		if _, err := io.CopyBuffer(tw, io.LimitReader(random, fileSize), buf); err != nil {
			return err
		}
	}

	return nil
}

// Creates bucket if not exists. If exists uses it or deletes and creates new
// one if cleanup flag was set.
func setupBucket(c *cli.Context, bck cmn.Bck) error {
	cleanup := flagIsSet(c, cleanupFlag)

	exists, err := api.DoesBucketExist(defaultAPIParams, cmn.QueryBcks(bck))
	if err != nil {
		return err
	}

	if exists && cleanup {
		err := api.DestroyBucket(defaultAPIParams, bck)
		if err != nil {
			return err
		}
	}

	if !exists || cleanup {
		if err := api.CreateBucket(defaultAPIParams, bck); err != nil {
			return err
		}
	}

	return nil
}

func genShardsHandler(c *cli.Context) error {
	var (
		ext = parseStrFlag(c, extFlag)
		bck = cmn.Bck{
			Name: parseStrFlag(c, dsortBucketFlag),
		}
		template  = parseStrFlag(c, dsortTemplateFlag)
		fileCnt   = parseIntFlag(c, fileCountFlag)
		concLimit = parseIntFlag(c, concurrencyFlag)

		fileSize int64
	)
	fileSize, err := parseByteFlagToInt(c, fileSizeFlag)
	if err != nil {
		return err
	}

	supportedExts := []string{".tar", ".tgz"}
	if !cmn.StringInSlice(ext, supportedExts) {
		return fmt.Errorf("extension %q is invalid, should be one of %q", ext, strings.Join(supportedExts, ", "))
	}

	mem := &memsys.MMSA{Name: "dsort-cli"}
	if err := mem.Init(false); err != nil {
		return err
	}

	pt, err := cmn.ParseBashTemplate(template)
	if err != nil {
		return err
	}

	if err := setupBucket(c, bck); err != nil {
		return err
	}

	// Progress bar
	text := "Shards created: "
	progress := mpb.New(mpb.WithWidth(progressBarWidth))
	bar := progress.AddBar(
		pt.Count(),
		mpb.PrependDecorators(
			decor.Name(text, decor.WC{W: len(text) + 2, C: decor.DSyncWidthR}),
			decor.CountersNoUnit("%d/%d", decor.WCSyncWidth),
		),
		mpb.AppendDecorators(decor.Percentage(decor.WCSyncWidth)),
	)

	concSemaphore := make(chan struct{}, concLimit)
	group, ctx := errgroup.WithContext(context.Background())
	shardIt := pt.Iter()
	shardNum := 0
CreateShards:
	for shardName, hasNext := shardIt(); hasNext; shardName, hasNext = shardIt() {
		select {
		case concSemaphore <- struct{}{}:
		case <-ctx.Done():
			break CreateShards
		}

		group.Go(func(i int, name string) func() error {
			return func() error {
				defer func() {
					bar.Increment()
					<-concSemaphore
				}()

				name := fmt.Sprintf("%s%s", name, ext)
				sgl := mem.NewSGL(fileSize * int64(fileCnt))
				defer sgl.Free()

				if err := createTar(sgl, ext, i*fileCnt, (i+1)*fileCnt, fileCnt, fileSize); err != nil {
					return err
				}

				putArgs := api.PutObjectArgs{
					BaseParams: defaultAPIParams,
					Bck:        bck,
					Object:     name,
					Reader:     sgl,
				}
				if err := api.PutObject(putArgs); err != nil {
					return err
				}

				return nil
			}
		}(shardNum, shardName))
		shardNum++
	}

	if err := group.Wait(); err != nil {
		bar.Abort(true)
		return err
	}

	progress.Wait()
	return nil
}

type dsortResult struct {
	dur      time.Duration
	created  int64
	errors   []string
	warnings []string
	aborted  bool
}

func (d dsortResult) String() string {
	if d.aborted {
		return fmt.Sprintf("%s job was aborted", cmn.DSortName)
	}

	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("Created %d new shards. Job duration: %s", d.created, d.dur))
	if len(d.errors) > 0 {
		sb.WriteString("\nFollowing errors occurred during execution: ")
		sb.WriteString(strings.Join(d.errors, ","))
	}
	if len(d.warnings) > 0 {
		sb.WriteString("\nFollowing warnings occurred during execution: ")
		sb.WriteString(strings.Join(d.warnings, ","))
	}

	return sb.String()
}

type dsortPhaseState struct {
	total    int64
	progress int64
	bar      *mpb.Bar
}

type dsortPB struct {
	id          string
	params      api.BaseParams
	refreshTime time.Duration

	p *mpb.Progress

	dur     time.Duration
	phases  map[string]*dsortPhaseState
	aborted bool

	warnings []string
	errors   []string
}

func newPhases() map[string]*dsortPhaseState {
	phases := make(map[string]*dsortPhaseState, 3)
	phases[dsort.ExtractionPhase] = &dsortPhaseState{}
	phases[dsort.SortingPhase] = &dsortPhaseState{}
	phases[dsort.CreationPhase] = &dsortPhaseState{}
	return phases
}

func newDSortPB(baseParams api.BaseParams, id string, refreshTime time.Duration) *dsortPB {
	return &dsortPB{
		id:          id,
		params:      baseParams,
		refreshTime: refreshTime,
		p:           mpb.New(mpb.WithWidth(progressBarWidth)),
		phases:      newPhases(),
		errors:      make([]string, 0),
		warnings:    make([]string, 0),
	}
}

func (b *dsortPB) run() (dsortResult, error) {
	finished, err := b.start()
	if err != nil {
		return dsortResult{}, err
	}
	if finished {
		return b.result(), nil
	}

	for !finished {
		time.Sleep(b.refreshTime)

		metrics, err := api.MetricsDSort(b.params, b.id)
		if err != nil {
			b.cleanBars()
			return dsortResult{}, err
		}

		var targetMetrics *dsort.Metrics
		for _, targetMetrics = range metrics {
			break
		}

		if targetMetrics.Aborted.Load() {
			b.aborted = true
			break
		}

		b.warnings, b.errors = make([]string, 0), make([]string, 0)
		for _, targetMetrics := range metrics {
			b.warnings = append(b.warnings, targetMetrics.Warnings...)
			b.errors = append(b.errors, targetMetrics.Errors...)
		}
		finished = b.updateBars(metrics)
	}

	b.cleanBars()
	return b.result(), nil
}

func (b *dsortPB) start() (bool, error) {
	metrics, err := api.MetricsDSort(b.params, b.id)
	if err != nil {
		return false, err
	}

	finished := b.updateBars(metrics)
	return finished, nil
}

func (b *dsortPB) updateBars(metrics map[string]*dsort.Metrics) bool {
	var (
		targetMetrics *dsort.Metrics
		finished      = true
		phases        = newPhases()
	)

	for _, targetMetrics = range metrics {
		phases[dsort.ExtractionPhase].progress += targetMetrics.Extraction.ExtractedCnt

		phases[dsort.SortingPhase].progress = cmn.MaxI64(phases[dsort.SortingPhase].progress, targetMetrics.Sorting.RecvStats.Count)

		phases[dsort.CreationPhase].progress += targetMetrics.Creation.CreatedCnt
		phases[dsort.CreationPhase].total += targetMetrics.Creation.ToCreate

		finished = finished && targetMetrics.Creation.Finished
	}

	phases[dsort.ExtractionPhase].total = targetMetrics.Extraction.TotalCnt
	phases[dsort.SortingPhase].total = int64(math.Log2(float64(len(metrics))))

	// Create progress bars if necessary and/or update them.
	for _, phaseName := range phasesOrdered {
		phase := phases[phaseName]
		barPhase := b.phases[phaseName]

		diff := phase.progress - barPhase.progress

		// If not finished also update the progress bars.
		if !finished {
			if phases[phaseName].progress > 0 && barPhase.bar == nil {
				text := phaseToBarText(phaseName)
				barPhase.bar = b.p.AddBar(
					phase.total,
					mpb.PrependDecorators(
						decor.Name(text, decor.WC{W: len(text) + 2, C: decor.DSyncWidthR}),
						decor.CountersNoUnit("%d/%d", decor.WCSyncWidth),
					),
					mpb.AppendDecorators(decor.Percentage(decor.WCSyncWidth)),
				)
			}

			if barPhase.bar != nil && diff > 0 {
				if phase.total > barPhase.total {
					barPhase.bar.SetTotal(phase.total, false)
				}
				barPhase.bar.IncrBy(int(diff))
			}
		}

		barPhase.progress = phase.progress
		barPhase.total = phase.total
	}

	b.dur = targetMetrics.Creation.End.Sub(targetMetrics.Extraction.Start)
	return finished
}

func (b *dsortPB) cleanBars() {
	for _, phase := range b.phases {
		if phase.bar != nil {
			phase.bar.SetTotal(phase.total, true) // complete the bar
		}
	}
	b.p.Wait()
}

func (b *dsortPB) result() dsortResult {
	var created int64
	if phase, ok := b.phases[dsort.CreationPhase]; ok {
		created = phase.total
	}

	return dsortResult{
		dur:      b.dur,
		created:  created,
		warnings: b.warnings,
		errors:   b.errors,
		aborted:  b.aborted,
	}
}

func printMetrics(w io.Writer, jobID string, daemonIds []string) (aborted, finished bool, err error) {
	resp, err := api.MetricsDSort(defaultAPIParams, jobID)
	if err != nil {
		return false, false, err
	}

	aborted = false
	finished = true
	if len(daemonIds) > 0 {
		// Check if targets exist in the metric output.
		for _, daemonID := range daemonIds {
			if _, ok := resp[daemonID]; !ok {
				return false, false, fmt.Errorf("invalid daemon id %q ", daemonID)
			}
		}
		// Filter metrics only for requested targets.
		filterMap := map[string]*dsort.Metrics{}
		for _, daemonID := range daemonIds {
			filterMap[daemonID] = resp[daemonID]
		}
		resp = filterMap
	}
	for _, targetMetrics := range resp {
		aborted = aborted || targetMetrics.Aborted.Load()
		finished = finished && targetMetrics.Creation.Finished
	}
	// Here we use encoding/json inplace of jsoniter because of indentation issues,
	// https://github.com/json-iterator/go/issues/331
	b, err := jsonStd.MarshalIndent(resp, "", "  ")
	if err != nil {
		return false, false, err
	}

	_, err = fmt.Fprintf(w, "%s\n", string(b))
	return
}

func printCondensedStats(w io.Writer, id string) error {
	resp, err := api.MetricsDSort(defaultAPIParams, id)
	if err != nil {
		return err
	}

	var (
		aborted  bool
		finished = true

		elapsedTime    time.Duration
		extractionTime time.Duration
		sortingTime    time.Duration
		creationTime   time.Duration
	)
	for _, tm := range resp {
		aborted = aborted || tm.Aborted.Load()
		finished = finished && tm.Creation.Finished

		elapsedTime = cmn.MaxDuration(elapsedTime, tm.ElapsedTime())
		if tm.Extraction.Finished {
			extractionTime = cmn.MaxDuration(extractionTime, tm.Extraction.End.Sub(tm.Extraction.Start))
		} else {
			extractionTime = cmn.MaxDuration(extractionTime, time.Since(tm.Extraction.Start))
		}

		if tm.Sorting.Finished {
			sortingTime = cmn.MaxDuration(sortingTime, tm.Sorting.End.Sub(tm.Sorting.Start))
		} else if tm.Sorting.Running {
			sortingTime = cmn.MaxDuration(sortingTime, time.Since(tm.Sorting.Start))
		}

		if tm.Creation.Finished {
			creationTime = cmn.MaxDuration(creationTime, tm.Creation.End.Sub(tm.Creation.Start))
		} else if tm.Creation.Running {
			creationTime = cmn.MaxDuration(creationTime, time.Since(tm.Creation.Start))
		}
	}

	if aborted {
		_, _ = fmt.Fprintf(w, "DSort job was aborted. Check detailed metrics for encountered errors.\n")
		return nil
	}

	if finished {
		_, _ = fmt.Fprintf(
			w,
			"DSort job has successfully finished in %v:\n  Longest extraction:\t%v\n  Longest sorting:\t%v\n  Longest creation:\t%v\n",
			elapsedTime, extractionTime, sortingTime, creationTime,
		)
		return nil
	}

	_, _ = fmt.Fprintf(w, "DSort job currently running:\n  Extraction:\t%v", extractionTime)
	if sortingTime.Seconds() > 0 {
		_, _ = fmt.Fprintf(w, "\n  Sorting:\t%v", sortingTime)
	}
	if creationTime.Seconds() > 0 {
		_, _ = fmt.Fprintf(w, "\n  Creation:\t%v", creationTime)
	}
	_, _ = fmt.Fprint(w, "\n")
	return nil
}

func dsortJobsList(c *cli.Context, regex string) error {
	list, err := api.ListDSort(defaultAPIParams, regex)
	if err != nil {
		return err
	}

	sort.Slice(list, func(i int, j int) bool {
		if list[i].IsRunning() && !list[j].IsRunning() {
			return true
		}
		if !list[i].IsRunning() && list[j].IsRunning() {
			return false
		}
		if !list[i].Aborted && list[j].Aborted {
			return true
		}
		if list[i].Aborted && !list[j].Aborted {
			return false
		}

		return list[i].StartedTime.Before(list[j].StartedTime)
	})

	return templates.DisplayOutput(list, c.App.Writer, templates.DSortListTmpl)
}

func dsortJobStatus(c *cli.Context, id string) error {
	var (
		verbose = flagIsSet(c, verboseFlag)
		refresh = flagIsSet(c, refreshFlag)
		logging = flagIsSet(c, logFlag)
		json    = flagIsSet(c, jsonFlag)
	)

	// Show progress bar.
	if !verbose && refresh && !logging {
		refreshRate := calcRefreshRate(c)
		dsortResult, err := newDSortPB(defaultAPIParams, id, refreshRate).run()
		if err != nil {
			return err
		}

		fmt.Fprintln(c.App.Writer, dsortResult)
		return nil
	}

	// Show metrics just once.
	if !refresh && !logging {
		if json || verbose {
			var daemonIds []string
			if c.NArg() == 2 {
				daemonIds = append(daemonIds, c.Args().Get(1))
			}
			if _, _, err := printMetrics(c.App.Writer, id, daemonIds); err != nil {
				return err
			}

			fmt.Fprintf(c.App.Writer, "\n")
		}
		if !json {
			return printCondensedStats(c.App.Writer, id)
		}
		return nil
	}

	// Show metrics once in a while.

	w := c.App.Writer

	rate := calcRefreshRate(c)
	if logging {
		file, err := cmn.CreateFile(c.String(logFlag.Name))
		if err != nil {
			return err
		}
		w = file
		defer file.Close()
	}

	var (
		aborted, finished bool
		err               error
	)
	for {
		aborted, finished, err = printMetrics(w, id, nil)
		if err != nil {
			return err
		}
		if aborted || finished {
			break
		}

		time.Sleep(rate)
	}

	fmt.Fprintf(c.App.Writer, "\n")
	return printCondensedStats(c.App.Writer, id)
}
