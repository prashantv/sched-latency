package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"runtime"
	"runtime/metrics"
	"sort"
	"time"
)

var (
	percentiles    = []float64{0, 0.5, 0.99, 1.0}
	percentilesFmt = func(ps []time.Duration) string {
		for i := range ps {
			ps[i] = truncate(ps[i])
		}
		return fmt.Sprintf("min %-10v p50 %-10v p99 %-10v max %-10v", ps[0], ps[1], ps[2], ps[3])
	}
)

type Config struct {
	ReportInterval time.Duration
	SleepInterval  time.Duration
	Percentiles    []float64
}

func main() {
	cfg := Config{
		Percentiles: percentiles,
	}
	flag.DurationVar(&cfg.ReportInterval, "report-interval", time.Second, "How often to report delay measurements")
	flag.DurationVar(&cfg.SleepInterval, "sleep-interval", 15*time.Millisecond, "How long to sleep to measure delay")
	workers := flag.Int("workers", runtime.GOMAXPROCS(0), "Number of CPU-bound workers (defaults to GOMAXPROCS")

	flag.Parse()

	fmt.Printf("Config: %+v\n", cfg)

	go measureSleepDelay(cfg)
	go measureTimerDelay(cfg)
	go measureGoSchedDelay(cfg)

	for i := 0; i < *workers; i++ {
		go cpuLoop()
	}

	select {}
}

func measureSleepDelay(cfg Config) {
	reportAfter := time.Now().Add(cfg.ReportInterval)
	var measured []time.Duration

	for {
		start := time.Now()
		time.Sleep(cfg.SleepInterval)
		stop := time.Now()

		measured = append(measured, stop.Sub(start)-cfg.SleepInterval)
		if stop.After(reportAfter) {
			percentiles := cfg.SamplePercentiles(measured)
			cfg.Report("time.Sleep delay", percentiles)

			measured = measured[:0]
			reportAfter = time.Now().Add(cfg.ReportInterval)
		}
	}
}

func cpuLoop() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	for {
		json.Marshal(m)
	}
}

func measureTimerDelay(cfg Config) {
	// Create a timer to reuse.
	t := time.NewTimer(time.Second)
	if !t.Stop() {
		<-t.C
	}

	reportAfter := time.Now().Add(cfg.ReportInterval)
	var measured []time.Duration

	for {
		start := time.Now()
		t.Reset(cfg.SleepInterval)
		stop := <-t.C

		measured = append(measured, stop.Sub(start)-cfg.SleepInterval)
		if stop.After(reportAfter) {
			percentiles := cfg.SamplePercentiles(measured)
			cfg.Report("timer delay", percentiles)

			measured = measured[:0]
			reportAfter = time.Now().Add(cfg.ReportInterval)
		}
	}
}

func measureGoSchedDelay(cfg Config) {
	t := time.NewTicker(cfg.ReportInterval)

	cur := []metrics.Sample{{Name: "/sched/latencies:seconds"}}
	last := []metrics.Sample{{Name: "/sched/latencies:seconds"}}
	metrics.Read(last)

	for {
		<-t.C
		metrics.Read(cur)

		percentiles := cfg.HistogramPercentiles(cur[0].Value.Float64Histogram(), last[0].Value.Float64Histogram())
		cfg.Report("/sched/latencies", percentiles)

		last, cur = cur, last
	}
}

func floatSecondsToDuration(v float64) time.Duration {
	return time.Duration(v * float64(time.Second))
}

func truncate(d time.Duration) time.Duration {
	if d > time.Second {
		return d.Truncate(10 * time.Millisecond)
	}
	if d > time.Millisecond {
		return d.Truncate(10 * time.Microsecond)
	}
	if d > time.Microsecond {
		return d.Truncate(10 * time.Nanosecond)
	}
	return d
}

func (c Config) Report(name string, percentileSamples []time.Duration) {
	fmt.Printf("%20s: %s\n", name, percentilesFmt(percentileSamples))
}

func (c Config) SamplePercentiles(samples []time.Duration) []time.Duration {
	sort.Slice(samples, func(i, j int) bool {
		return samples[i] < samples[j]
	})

	var percentileDurations []time.Duration
	for _, p := range c.Percentiles {
		if len(samples) == 0 {
			percentileDurations = append(percentileDurations, 0)
			continue
		}

		idx := int(p * float64(len(samples)-1))
		percentileDurations = append(percentileDurations, samples[idx])
	}
	return percentileDurations
}

func (c Config) HistogramPercentiles(cur, last *metrics.Float64Histogram) []time.Duration {
	var total uint64
	cumulativeDiffs := make([]uint64, len(cur.Counts))
	for i := range cur.Counts {
		d := cur.Counts[i] - last.Counts[i]
		cumulativeDiffs[i] = d + total
		total += d
	}

	var pDurations []time.Duration
	for _, p := range percentiles {
		percentileVal := uint64(p * float64(total))

		percentileIdx := sort.Search(len(cumulativeDiffs), func(i int) bool {
			if p == 1.0 {
				// When looking for the max, we need "=".
				return cumulativeDiffs[i] >= percentileVal
			}
			return cumulativeDiffs[i] > percentileVal
		})

		// Use the upper-bound.
		percentileIdx++

		if percentileIdx >= len(cumulativeDiffs) {
			pDurations = append(pDurations, floatSecondsToDuration(cur.Buckets[percentileIdx-1]))
		} else {
			pDurations = append(pDurations, floatSecondsToDuration(cur.Buckets[percentileIdx]))
		}
	}
	return pDurations
}
