// Package aisloader
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */

package aisloader

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/NVIDIA/aistore/bench/aisloader/stats"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/stats/statsd"
	jsoniter "github.com/json-iterator/go"
)

var examples = `
1. Cleanup (i.e., empty) existing bucket:

	$ aisloader -bucket=nvais -duration 0s -totalputsize=0 # by default cleanup=true
	$ aisloader -bucket=nvais -provider=cloud -cleanup=true -duration 0s -totalputsize=0

2. Time-based 100% PUT into ais bucket. Upon exit the bucket is emptied (by default):

	$ aisloader -bucket=nvais -duration 10s -numworkers=3 -minsize=1K -maxsize=1K -pctput=100 -provider=local

3. Timed (for 1h) 100% GET from a Cloud bucket, no cleanup:

	$ aisloader -bucket=nvaws -duration 1h -numworkers=30 -pctput=0 -provider=cloud -cleanup=false

4. Mixed 30%/70% PUT and GET of variable-size objects to/from a Cloud bucket.
   PUT will generate random object names and is limited by the 10GB total size.
   Cleanup is not disabled, which means that upon completion all generated objects will be deleted:

	$ aisloader -bucket=nvaws -duration 0s -numworkers=3 -minsize=1024 -maxsize=1MB -pctput=30 -provider=cloud -totalputsize=10G

5. PUT 1GB total into an ais bucket with cleanup disabled, object size = 1MB, duration unlimited:

	$ aisloader -bucket=nvais -cleanup=false -totalputsize=1G -duration=0 -minsize=1MB -maxsize=1MB -numworkers=8 -pctput=100 -provider=ais

6. 100% GET from an ais bucket:

	$ aisloader -bucket=nvais -duration 5s -numworkers=3 -pctput=0 -provider=ais

7. PUT 2000 objects named as 'aisloader/hex({0..2000}{loaderid})':

	$ aisloader -bucket=nvais -duration 10s -numworkers=3 -loaderid=11 -loadernum=20 -maxputs=2000 -objNamePrefix="aisloader"

8. Use random object names and loaderID to report statistics:

	$ aisloader -loaderid=10

9. PUT objects with random name generation being based on the specified loaderID and the total number of concurrent aisloaders:

	$ aisloader -loaderid=10 -loadernum=20

10. Same as above except that loaderID is computed by the aisloader as hash(loaderstring) & 0xff:

	$ aisloader -loaderid=loaderstring -loaderidhashlen=8

11. Print loaderID and exit (all 3 examples below) with the resulting loaderID shown on the right:",

	$ aisloader -getloaderid (0x0)",
	$ aisloader -loaderid=10 -getloaderid (0xa)
	$ aisloader -loaderid=loaderstring -loaderidhashlen=8 -getloaderid (0xdb)

`

func printUsage(f *flag.FlagSet) {
	fmt.Println("\nAbout")
	fmt.Println("=====")
	fmt.Printf("AIS loader (aisloader v%s, build %s) is a tool to measure storage performance.\n", version, build)
	fmt.Println("It's a load generator that has been developed (and is currently used) to benchmark and")
	fmt.Println("stress-test AIStore(tm) but can be easily extended for any S3-compatible backend.")
	fmt.Println("For usage, run: `aisloader` or `aisloader usage` or `aisloader --help`.")
	fmt.Println("Further details at https://github.com/NVIDIA/aistore/blob/master/docs/howto_benchmark.md")

	fmt.Println("\nCommand-line options")
	fmt.Println("====================")
	f.PrintDefaults()
	fmt.Println()

	fmt.Println("\nExamples")
	fmt.Println("========")
	fmt.Print(examples)
}

// prettyNumber converts a number to format like 1,234,567
func prettyNumber(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%s,%03d", prettyNumber(n/1000), n%1000)
}

// prettyBytes converts number of bytes to something like 4.7G, 2.8K, etc
func prettyBytes(n int64) string {
	if n <= 0 { // process special case that B2S do not cover
		return "-"
	}
	return cmn.B2S(n, 1)
}

func prettySpeed(n int64) string {
	if n <= 0 {
		return "-"
	}
	return cmn.B2S(n, 2) + "/s"
}

// prettyDuration converts an integer representing a time in nano second to a string
func prettyDuration(t int64) string {
	d := time.Duration(t).String()
	i := strings.Index(d, ".")
	if i < 0 {
		return d
	}
	out := make([]byte, i+1, 32)
	copy(out, d[0:i+1])
	for j := i + 1; j < len(d); j++ {
		if d[j] > '9' || d[j] < '0' {
			out = append(out, d[j])
		} else if j < i+4 {
			out = append(out, d[j])
		}
	}
	return string(out)
}

// prettyLatency combines three latency min, avg and max into a string
func prettyLatency(min, avg, max int64) string {
	return fmt.Sprintf("%-11s%-11s%-11s", prettyDuration(min), prettyDuration(avg), prettyDuration(max))
}

func prettyTimestamp() string {
	return time.Now().Format("15:04:05")
}

func preWriteStats(to io.Writer, jsonFormat bool) {
	fmt.Fprintln(to)
	if !jsonFormat {
		fmt.Fprintf(to, statsPrintHeader,
			"Time", "OP", "Count", "Size (Total)", "Latency (min, avg, max)", "Throughput (Avg)", "Errors (Total)")
	} else {
		fmt.Fprintln(to, "[")
	}
}

func postWriteStats(to io.Writer, jsonFormat bool) {
	if jsonFormat {
		fmt.Fprintln(to)
		fmt.Fprintln(to, "]")
	}
}

func finalizeStats(to io.Writer) {
	accumulatedStats.aggregate(intervalStats)
	writeStats(to, runParams.jsonFormat, true /* final */, intervalStats, accumulatedStats)
	postWriteStats(to, runParams.jsonFormat)

	// reset gauges, otherwise they would stay at last send value
	statsd.ResetMetricsGauges(&statsdC)
}

func writeFinalStats(to io.Writer, jsonFormat bool, s sts) {
	if !jsonFormat {
		writeHumanReadibleFinalStats(to, s)
	} else {
		writeStatsJSON(to, s, false)
	}
}

func writeIntervalStats(to io.Writer, jsonFormat bool, s, t sts) {
	if !jsonFormat {
		writeHumanReadibleIntervalStats(to, s, t)
	} else {
		writeStatsJSON(to, s)
	}
}

func jsonStatsFromReq(r stats.HTTPReq) *jsonStats {
	jStats := &jsonStats{
		Cnt:        r.Total(),
		Bytes:      r.TotalBytes(),
		Start:      r.Start(),
		Duration:   time.Since(r.Start()),
		Errs:       r.TotalErrs(),
		Latency:    r.AvgLatency(),
		MinLatency: r.MinLatency(),
		MaxLatency: r.MaxLatency(),
		Throughput: r.Throughput(r.Start(), time.Now()),
	}

	return jStats
}

func writeStatsJSON(to io.Writer, s sts, withcomma ...bool) {
	jStats := struct {
		Get *jsonStats `json:"get"`
		Put *jsonStats `json:"put"`
		Cfg *jsonStats `json:"cfg"`
	}{
		Get: jsonStatsFromReq(s.get),
		Put: jsonStatsFromReq(s.put),
		Cfg: jsonStatsFromReq(s.getConfig),
	}

	jsonOutput := cmn.MustMarshal(jStats)
	fmt.Fprintf(to, "\n%s", string(jsonOutput))
	// print comma by default
	if len(withcomma) == 0 || withcomma[0] {
		fmt.Fprint(to, ",")
	}
}

func writeHumanReadibleIntervalStats(to io.Writer, s, t sts) {
	p := fmt.Fprintf
	pn := prettyNumber
	pb := prettyBytes
	ps := prettySpeed
	pl := prettyLatency
	pt := prettyTimestamp

	workOrderResLen := int64(len(workOrderResults))
	// show interval stats; some fields are shown of both interval and total, for example, gets, puts, etc
	errs := "-"
	if t.put.TotalErrs() != 0 {
		errs = pn(s.put.TotalErrs()) + " (" + pn(t.put.TotalErrs()) + ")"
	}
	if s.put.Total() != 0 {
		p(to, statsPrintHeader, pt(), "PUT",
			pn(s.put.Total())+" ("+pn(t.put.Total())+" "+pn(putPending)+" "+pn(workOrderResLen)+")",
			pb(s.put.TotalBytes())+" ("+pb(t.put.TotalBytes())+")",
			pl(s.put.MinLatency(), s.put.AvgLatency(), s.put.MaxLatency()),
			ps(s.put.Throughput(s.put.Start(), time.Now()))+" ("+ps(t.put.Throughput(t.put.Start(), time.Now()))+")",
			errs)
	}
	errs = "-"
	if t.get.TotalErrs() != 0 {
		errs = pn(s.get.TotalErrs()) + " (" + pn(t.get.TotalErrs()) + ")"
	}
	if s.get.Total() != 0 {
		p(to, statsPrintHeader, pt(), "GET",
			pn(s.get.Total())+" ("+pn(t.get.Total())+" "+pn(getPending)+" "+pn(workOrderResLen)+")",
			pb(s.get.TotalBytes())+" ("+pb(t.get.TotalBytes())+")",
			pl(s.get.MinLatency(), s.get.AvgLatency(), s.get.MaxLatency()),
			ps(s.get.Throughput(s.get.Start(), time.Now()))+" ("+ps(t.get.Throughput(t.get.Start(), time.Now()))+")",
			errs)
	}
	if s.getConfig.Total() != 0 {
		p(to, statsPrintHeader, pt(), "CFG",
			pn(s.getConfig.Total())+" ("+pn(t.getConfig.Total())+")",
			pb(s.getConfig.TotalBytes())+" ("+pb(t.getConfig.TotalBytes())+")",
			pl(s.getConfig.MinLatency(), s.getConfig.AvgLatency(), s.getConfig.MaxLatency()),
			ps(s.getConfig.Throughput(s.getConfig.Start(), time.Now()))+" ("+ps(t.getConfig.Throughput(t.getConfig.Start(), time.Now()))+")",
			pn(s.getConfig.TotalErrs())+" ("+pn(t.getConfig.TotalErrs())+")")
	}
}

func writeHumanReadibleFinalStats(to io.Writer, t sts) {
	p := fmt.Fprintf
	pn := prettyNumber
	pb := prettyBytes
	ps := prettySpeed
	pl := prettyLatency
	pt := prettyTimestamp
	preWriteStats(to, false)
	p(to, statsPrintHeader, pt(), "PUT",
		pn(t.put.Total()),
		pb(t.put.TotalBytes()),
		pl(t.put.MinLatency(), t.put.AvgLatency(), t.put.MaxLatency()),
		ps(t.put.Throughput(t.put.Start(), time.Now())),
		pn(t.put.TotalErrs()))
	p(to, statsPrintHeader, pt(), "GET",
		pn(t.get.Total()),
		pb(t.get.TotalBytes()),
		pl(t.get.MinLatency(), t.get.AvgLatency(), t.get.MaxLatency()),
		ps(t.get.Throughput(t.get.Start(), time.Now())),
		pn(t.get.TotalErrs()))
	p(to, statsPrintHeader, pt(), "CFG",
		pn(t.getConfig.Total()),
		pb(t.getConfig.TotalBytes()),
		pl(t.getConfig.MinLatency(), t.getConfig.AvgLatency(), t.getConfig.MaxLatency()),
		pb(t.getConfig.Throughput(t.getConfig.Start(), time.Now())),
		pn(t.getConfig.TotalErrs()))
}

// writeStatus writes stats to the writter.
// if final = true, writes the total; otherwise writes the interval stats
func writeStats(to io.Writer, jsonFormat, final bool, s, t sts) {
	if final {
		writeFinalStats(to, jsonFormat, t)
	} else {
		// show interval stats; some fields are shown of both interval and total, for example, gets, puts, etc
		writeIntervalStats(to, jsonFormat, s, t)
	}
}

// printRunParams show run parameters in json format
func printRunParams(p params) {
	b, _ := jsoniter.MarshalIndent(struct {
		Seed          int64  `json:"seed,string"`
		URL           string `json:"proxy"`
		Bucket        string `json:"bucket"`
		Provider      string `json:"provider"`
		Namespace     string `json:"namespace"`
		Duration      string `json:"duration"`
		MaxPutBytes   int64  `json:"PUT upper bound,string"`
		PutPct        int    `json:"% PUT"`
		MinSize       int64  `json:"minimum object size (bytes)"`
		MaxSize       int64  `json:"maximum object size (bytes)"`
		NumWorkers    int    `json:"# workers"`
		StatsInterval string `json:"stats interval"`
		Backing       string `json:"backed by"`
		Cleanup       bool   `json:"cleanup"`
	}{
		Seed:          p.seed,
		URL:           p.proxyURL,
		Bucket:        p.bck.Name,
		Provider:      p.bck.Provider,
		Namespace:     p.bck.Ns.String(),
		Duration:      p.duration.String(),
		MaxPutBytes:   p.putSizeUpperBound,
		PutPct:        p.putPct,
		MinSize:       p.minSize,
		MaxSize:       p.maxSize,
		NumWorkers:    p.numWorkers,
		StatsInterval: (time.Duration(runParams.statsShowInterval) * time.Second).String(),
		Backing:       p.readerType,
		Cleanup:       p.cleanUp.Val,
	}, "", "   ")

	fmt.Printf("Runtime configuration:\n%s\n\n", string(b))
}
