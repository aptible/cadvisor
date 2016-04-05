// Copyright 2014 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package manager

import (
	"flag"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"os/exec"
	"path"
	"regexp"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/cadvisor/cache/memory"
	"github.com/google/cadvisor/collector"
	"github.com/google/cadvisor/container"
	info "github.com/google/cadvisor/info/v1"
	"github.com/google/cadvisor/info/v2"
	"github.com/google/cadvisor/summary"
	"github.com/google/cadvisor/utils"
	"github.com/google/cadvisor/utils/cpuload"

	units "github.com/docker/go-units"
	"github.com/golang/glog"
)

// Housekeeping interval.
var enableLoadReader = flag.Bool("enable_load_reader", false, "Whether to enable cpu load reader")
var HousekeepingInterval = flag.Duration("housekeeping_interval", 1*time.Second, "Interval between container housekeepings")
var LoadreaderInterval = flag.Duration("load_reader_interval", 1*time.Second, "Interval between load reader probes")
var MaxLoadReaderInterval = flag.Duration("max_load_reader_interval", 60*time.Second, "Interval between load reader probes")

var cgroupPathRegExp = regexp.MustCompile(`devices[^:]*:(.*?)[,;$]`)

type containerInfo struct {
	info.ContainerReference
	Subcontainers []info.ContainerReference
	Spec          info.ContainerSpec
}

type containerData struct {
	handler                  container.ContainerHandler
	info                     containerInfo
	memoryCache              *memory.InMemoryCache
	lock                     sync.Mutex
	loadReader               cpuload.CpuLoadReader
	summaryReader            *summary.StatsSummary
	housekeepingInterval     time.Duration
	maxHousekeepingInterval  time.Duration
	allowDynamicHousekeeping bool
	lastUpdatedTime          time.Time
	lastErrorTime            time.Time

	// smoothed load average seen so far.
	loadAvg              float64
	loadAvgLastProbeTime time.Time
	// How often load average should be checked (samples are unreliable by nature)
	loadreaderInterval time.Duration
	// Decay value used for load average smoothing. Interval length of 10 seconds is used.
	loadStop chan bool
	loadLock sync.Mutex

	// last taskstats
	taskStats info.LoadStats

	// Whether to log the usage of this container when it is updated.
	logUsage bool

	// Tells the container to stop.
	stop chan bool

	// Runs custom metric collectors.
	collectorManager collector.CollectorManager
}

func DurationMin(d1 time.Duration, d2 time.Duration) time.Duration {
	if d1 < d2 {
		return d1
	}

	return d2
}

func (c *containerData) Start() error {
	go c.doHousekeepingLoop()
	go c.doLoadReaderLoop()
	return nil
}

func (c *containerData) Stop() error {
	err := c.memoryCache.RemoveContainer(c.info.Name)
	if err != nil {
		return err
	}
	c.stop <- true
	c.loadStop <- true
	return nil
}

func (c *containerData) allowErrorLogging() bool {
	if time.Since(c.lastErrorTime) > time.Minute {
		c.lastErrorTime = time.Now()
		return true
	}
	return false
}

func (c *containerData) GetInfo() (*containerInfo, error) {
	// Get spec and subcontainers.
	if time.Since(c.lastUpdatedTime) > 5*time.Second {
		err := c.updateSpec()
		if err != nil {
			return nil, err
		}
		err = c.updateSubcontainers()
		if err != nil {
			return nil, err
		}
		c.lastUpdatedTime = time.Now()
	}
	// Make a copy of the info for the user.
	c.lock.Lock()
	defer c.lock.Unlock()
	return &c.info, nil
}

func (c *containerData) DerivedStats() (v2.DerivedStats, error) {
	if c.summaryReader == nil {
		return v2.DerivedStats{}, fmt.Errorf("derived stats not enabled for container %q", c.info.Name)
	}
	return c.summaryReader.DerivedStats()
}

func (c *containerData) getCgroupPath(cgroups string) (string, error) {
	if cgroups == "-" {
		return "/", nil
	}
	matches := cgroupPathRegExp.FindSubmatch([]byte(cgroups))
	if len(matches) != 2 {
		glog.V(3).Infof("failed to get devices cgroup path from %q", cgroups)
		// return root in case of failures - devices hierarchy might not be enabled.
		return "/", nil
	}
	return string(matches[1]), nil
}

// Returns contents of a file inside the container root.
// Takes in a path relative to container root.
func (c *containerData) ReadFile(filepath string, inHostNamespace bool) ([]byte, error) {
	pids, err := c.getContainerPids(inHostNamespace)
	if err != nil {
		return nil, err
	}
	// TODO(rjnagal): Optimize by just reading container's cgroup.proc file when in host namespace.
	rootfs := "/"
	if !inHostNamespace {
		rootfs = "/rootfs"
	}
	for _, pid := range pids {
		filePath := path.Join(rootfs, "/proc", pid, "/root", filepath)
		glog.V(3).Infof("Trying path %q", filePath)
		data, err := ioutil.ReadFile(filePath)
		if err == nil {
			return data, err
		}
	}
	// No process paths could be found. Declare config non-existent.
	return nil, fmt.Errorf("file %q does not exist.", filepath)
}

// Return output for ps command in host /proc with specified format
func (c *containerData) getPsOutput(inHostNamespace bool, format string) ([]byte, error) {
	args := []string{}
	command := "ps"
	if !inHostNamespace {
		command = "/usr/sbin/chroot"
		args = append(args, "/rootfs", "ps")
	}
	args = append(args, "-e", "-o", format)
	out, err := exec.Command(command, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("failed to execute %q command: %v", command, err)
	}
	return out, err
}

// Get pids of processes in this container.
// A slightly lighterweight call than GetProcessList if other details are not required.
func (c *containerData) getContainerPids(inHostNamespace bool) ([]string, error) {
	format := "pid,cgroup"
	out, err := c.getPsOutput(inHostNamespace, format)
	if err != nil {
		return nil, err
	}
	expectedFields := 2
	lines := strings.Split(string(out), "\n")
	pids := []string{}
	for _, line := range lines[1:] {
		if len(line) == 0 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < expectedFields {
			return nil, fmt.Errorf("expected at least %d fields, found %d: output: %q", expectedFields, len(fields), line)
		}
		pid := fields[0]
		cgroup, err := c.getCgroupPath(fields[1])
		if err != nil {
			return nil, fmt.Errorf("could not parse cgroup path from %q: %v", fields[1], err)
		}
		if c.info.Name == cgroup {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func (c *containerData) GetProcessList(cadvisorContainer string, inHostNamespace bool) ([]v2.ProcessInfo, error) {
	// report all processes for root.
	isRoot := c.info.Name == "/"
	format := "user,pid,ppid,stime,pcpu,pmem,rss,vsz,stat,time,comm,cgroup"
	out, err := c.getPsOutput(inHostNamespace, format)
	if err != nil {
		return nil, err
	}
	expectedFields := 12
	processes := []v2.ProcessInfo{}
	lines := strings.Split(string(out), "\n")
	for _, line := range lines[1:] {
		if len(line) == 0 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < expectedFields {
			return nil, fmt.Errorf("expected at least %d fields, found %d: output: %q", expectedFields, len(fields), line)
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, fmt.Errorf("invalid pid %q: %v", fields[1], err)
		}
		ppid, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, fmt.Errorf("invalid ppid %q: %v", fields[2], err)
		}
		percentCpu, err := strconv.ParseFloat(fields[4], 32)
		if err != nil {
			return nil, fmt.Errorf("invalid cpu percent %q: %v", fields[4], err)
		}
		percentMem, err := strconv.ParseFloat(fields[5], 32)
		if err != nil {
			return nil, fmt.Errorf("invalid memory percent %q: %v", fields[5], err)
		}
		rss, err := strconv.ParseUint(fields[6], 0, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid rss %q: %v", fields[6], err)
		}
		// convert to bytes
		rss *= 1024
		vs, err := strconv.ParseUint(fields[7], 0, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid virtual size %q: %v", fields[7], err)
		}
		// convert to bytes
		vs *= 1024
		cgroup, err := c.getCgroupPath(fields[11])
		if err != nil {
			return nil, fmt.Errorf("could not parse cgroup path from %q: %v", fields[11], err)
		}
		// Remove the ps command we just ran from cadvisor container.
		// Not necessary, but makes the cadvisor page look cleaner.
		if !inHostNamespace && cadvisorContainer == cgroup && fields[10] == "ps" {
			continue
		}
		var cgroupPath string
		if isRoot {
			cgroupPath = cgroup
		}

		if isRoot || c.info.Name == cgroup {
			processes = append(processes, v2.ProcessInfo{
				User:          fields[0],
				Pid:           pid,
				Ppid:          ppid,
				StartTime:     fields[3],
				PercentCpu:    float32(percentCpu),
				PercentMemory: float32(percentMem),
				RSS:           rss,
				VirtualSize:   vs,
				Status:        fields[8],
				RunningTime:   fields[9],
				Cmd:           fields[10],
				CgroupPath:    cgroupPath,
			})
		}
	}
	return processes, nil
}

func newContainerData(containerName string, memoryCache *memory.InMemoryCache, handler container.ContainerHandler, logUsage bool, collectorManager collector.CollectorManager, maxHousekeepingInterval time.Duration, allowDynamicHousekeeping bool) (*containerData, error) {
	if memoryCache == nil {
		return nil, fmt.Errorf("nil memory storage")
	}
	if handler == nil {
		return nil, fmt.Errorf("nil container handler")
	}
	ref, err := handler.ContainerReference()
	if err != nil {
		return nil, err
	}

	cont := &containerData{
		handler:                  handler,
		memoryCache:              memoryCache,
		housekeepingInterval:     *HousekeepingInterval,
		maxHousekeepingInterval:  maxHousekeepingInterval,
		allowDynamicHousekeeping: allowDynamicHousekeeping,
		logUsage:                 logUsage,
		loadAvg:                  -1.0, // negative value indicates uninitialized
		loadreaderInterval:       *LoadreaderInterval,
		loadStop:                 make(chan bool, 1),
		stop:                     make(chan bool, 1),
		collectorManager:         collectorManager,
	}
	cont.info.ContainerReference = ref

	if *enableLoadReader {
		// Create cpu load reader.
		loadReader, err := cpuload.New()
		if err != nil {
			// TODO(rjnagal): Promote to warning once we support cpu load inside namespaces.
			glog.Infof("Could not initialize cpu load reader for %q: %s", ref.Name, err)
		} else {
			cont.loadReader = loadReader
		}
	}

	err = cont.updateSpec()
	if err != nil {
		return nil, err
	}
	cont.summaryReader, err = summary.New(cont.info.Spec)
	if err != nil {
		cont.summaryReader = nil
		glog.Warningf("Failed to create summary reader for %q: %v", ref.Name, err)
	}

	return cont, nil
}

// Determine when the next housekeeping should occur.
func (c *containerData) adjustHousekeepingInterval() error {
	if !c.allowDynamicHousekeeping {
		return nil
	}

	var empty time.Time
	stats, err := c.memoryCache.RecentStats(c.info.Name, empty, empty, 2)
	if err != nil {
		return err
	}

	if len(stats) < 2 {
		// Not enough stats yet to adjust housekeeping interval
		return nil
	}

	if stats[0].StatsEq(stats[1]) {
		c.housekeepingInterval = DurationMin(c.housekeepingInterval*2, c.maxHousekeepingInterval)
	} else {
		// Lower interval back to the baseline.
		c.housekeepingInterval = *HousekeepingInterval
	}

	return nil
}

// TODO(vmarmol): Implement stats collecting as a custom collector.
func (c *containerData) doHousekeepingLoop() {
	// Start any background goroutines - must be cleaned up in c.handler.Cleanup().
	c.handler.Start()
	defer c.handler.Cleanup()

	// Initialize cpuload reader - must be cleaned up in c.loadReader.Stop()
	if c.loadReader != nil {
		err := c.loadReader.Start()
		if err != nil {
			glog.Warningf("Could not start cpu load stat collector for %q: %s", c.info.Name, err)
		}
		defer c.loadReader.Stop()
	}

	// Long housekeeping is either 100ms or half of the housekeeping interval.
	longHousekeeping := 100 * time.Millisecond
	if *HousekeepingInterval/2 < longHousekeeping {
		longHousekeeping = *HousekeepingInterval / 2
	}

	glog.V(3).Infof("Start housekeeping for container %q\n", c.info.Name)
	lastHousekeeping := time.Now()
	for {
		select {
		case <-c.stop:
			// Stop housekeeping when signaled.
			return
		default:
			// Perform housekeeping.
			start := time.Now()
			c.doWithTimeout((*containerData).updateStats, (*HousekeepingInterval)*2)

			// Log if housekeeping took too long.
			duration := time.Since(start)
			if duration >= longHousekeeping {
				glog.V(3).Infof("[%s] Housekeeping took %s", c.info.Name, duration)
			}
		}

		// Log usage if asked to do so.
		if c.logUsage {
			const numSamples = 60
			var empty time.Time
			stats, err := c.memoryCache.RecentStats(c.info.Name, empty, empty, numSamples)
			if err != nil {
				if c.allowErrorLogging() {
					glog.Infof("[%s] Failed to get recent stats for logging usage: %v", c.info.Name, err)
				}
			} else if len(stats) < numSamples {
				// Ignore, not enough stats yet.
			} else {
				usageCpuNs := uint64(0)
				for i := range stats {
					if i > 0 {
						usageCpuNs += (stats[i].Cpu.Usage.Total - stats[i-1].Cpu.Usage.Total)
					}
				}
				usageMemory := stats[numSamples-1].Memory.Usage

				instantUsageInCores := float64(stats[numSamples-1].Cpu.Usage.Total-stats[numSamples-2].Cpu.Usage.Total) / float64(stats[numSamples-1].Timestamp.Sub(stats[numSamples-2].Timestamp).Nanoseconds())
				usageInCores := float64(usageCpuNs) / float64(stats[numSamples-1].Timestamp.Sub(stats[0].Timestamp).Nanoseconds())
				usageInHuman := units.HumanSize(float64(usageMemory))
				glog.Infof("[%s] %.3f cores (average: %.3f cores), %s of memory", c.info.Name, instantUsageInCores, usageInCores, usageInHuman)
			}
		}

		err := c.adjustHousekeepingInterval()
		if err != nil && c.allowErrorLogging() {
			glog.Warningf("Failed to get RecentStats(%q) while determining the next housekeeping: %v", c.info.Name, err)
		}
		next := lastHousekeeping.Add(utils.Jitter(c.housekeepingInterval, 1.0))

		// Schedule the next housekeeping. Sleep until that time.
		if time.Now().Before(next) {
			time.Sleep(next.Sub(time.Now()))
		} else {
			next = time.Now()
		}
		lastHousekeeping = next
	}
}

type ContainerDataMethod func(c *containerData) error

func (c *containerData) doWithTimeout(meth ContainerDataMethod, timeout time.Duration) {
	done := make(chan bool, 1)

	go func() {
		err := meth(c)
		if err != nil {
			if c.allowErrorLogging() {
				glog.Infof("Failed to update stats for container \"%s\": %s", c.info.Name, err)
			}
		}
		done <- true
	}()

	select {
	case <-done:
		// Nothing to do
	case <-time.After(timeout):
		// We timed out. Dump all goroutine stacks to facilitate troubleshooting, and panic.
		glog.Errorf("Timed out for: %s", c.info.Name)
		pprof.Lookup("goroutine").WriteTo(os.Stderr, 1)
		panic("Aborting!")
	}
}

func (c *containerData) updateSpec() error {
	spec, err := c.handler.GetSpec()
	if err != nil {
		// Ignore errors if the container is dead.
		if !c.handler.Exists() {
			return nil
		}
		return err
	}

	customMetrics, err := c.collectorManager.GetSpec()
	if err != nil {
		return err
	}
	if len(customMetrics) > 0 {
		spec.HasCustomMetrics = true
		spec.CustomMetrics = customMetrics
	}
	c.lock.Lock()
	defer c.lock.Unlock()
	c.info.Spec = spec
	return nil
}

// Calculate new smoothed load average using the new sample of runnable threads.
// The decay used ensures that the load will stabilize on a new constant value within
// 10 seconds.
func (c *containerData) updateLoadAvg(probeTime time.Time, newTaskStats info.LoadStats) {
	c.loadLock.Lock()
	defer c.loadLock.Unlock()

	newLoad := newTaskStats.NrRunning + newTaskStats.NrUninterruptible + newTaskStats.NrIoWait
	if c.loadAvg < 0 {
		// We never saw a load average, just record this as the new authoritative value
		c.loadAvg = float64(newLoad)
	} else {
		loadDecay := math.Exp(float64(-(probeTime.Sub(c.loadAvgLastProbeTime).Seconds() / 10)))
		c.loadAvg = float64(newLoad)*(1.0-loadDecay) + c.loadAvg*loadDecay
	}
	c.loadAvgLastProbeTime = probeTime
}

func (c *containerData) LoadAvg() float64 {
	c.loadLock.Lock()
	defer c.loadLock.Unlock()

	return c.loadAvg
}

func (c *containerData) updateTaskStats(taskStats info.LoadStats) {
	c.loadLock.Lock()
	defer c.loadLock.Unlock()

	c.taskStats = taskStats
}

func (c *containerData) TaskStats() info.LoadStats {
	c.loadLock.Lock()
	defer c.loadLock.Unlock()

	return c.taskStats
}

func (c *containerData) doLoadReaderIteration() error {
	path, err := c.handler.GetCgroupPath("cpu")
	if err == nil {
		newTaskStats, err := c.loadReader.GetCpuLoad(c.info.Name, path)
		probeTime := time.Now()
		if err != nil {
			return fmt.Errorf("failed to get load stat for %q - path %q, error %s", c.info.Name, path, err)
		}
		// Check whether we should backoff before updating task stats
		if c.allowDynamicHousekeeping && c.TaskStats() == newTaskStats {
			c.loadreaderInterval = DurationMin(c.loadreaderInterval*2, *MaxLoadReaderInterval)
		} else {
			c.loadreaderInterval = *LoadreaderInterval
		}

		c.updateTaskStats(newTaskStats)
		c.updateLoadAvg(probeTime, newTaskStats)
	}

	return nil
}

func (c *containerData) doLoadReaderLoop() {
	if c.loadReader == nil {
		// Load reader is disabled. Don't even start the loop.
		return
	}

	lastIteration := time.Now()
	for {
		select {
		case <-c.loadStop:
			return
		default:
			c.doWithTimeout((*containerData).doLoadReaderIteration, (*LoadreaderInterval)*2)
		}

		// Schedule the next housekeeping. Sleep until that time.
		next := lastIteration.Add(utils.Jitter(c.loadreaderInterval, 1.0))

		if time.Now().Before(next) {
			time.Sleep(next.Sub(time.Now()))
		} else {
			next = time.Now()
		}

		lastIteration = next
	}
}

func (c *containerData) updateStats() error {
	stats, statsErr := c.handler.GetStats()
	if statsErr != nil {
		// Ignore errors if the container is dead.
		if !c.handler.Exists() {
			return nil
		}

		// Stats may be partially populated, push those before we return an error.
		statsErr = fmt.Errorf("%v, continuing to push stats", statsErr)
	}
	if stats == nil {
		return statsErr
	}
	load := c.LoadAvg()
	if load >= 0 {
		// convert to 'milliLoad' to avoid floats and preserve precision.
		stats.Cpu.LoadAverage = int32(load * 1000)
	}
	taskStats := c.TaskStats()
	if &taskStats != nil {
		stats.TaskStats = taskStats
	}
	if c.summaryReader != nil {
		err := c.summaryReader.AddSample(*stats)
		if err != nil {
			// Ignore summary errors for now.
			glog.V(2).Infof("Failed to add summary stats for %q: %v", c.info.Name, err)
		}
	}
	var customStatsErr error
	cm := c.collectorManager.(*collector.GenericCollectorManager)
	if len(cm.Collectors) > 0 {
		if cm.NextCollectionTime.Before(time.Now()) {
			customStats, err := c.updateCustomStats()
			if customStats != nil {
				stats.CustomMetrics = customStats
			}
			if err != nil {
				customStatsErr = err
			}
		}
	}

	ref, err := c.handler.ContainerReference()
	if err != nil {
		// Ignore errors if the container is dead.
		if !c.handler.Exists() {
			return nil
		}
		return err
	}
	err = c.memoryCache.AddStats(ref, stats)
	if err != nil {
		return err
	}
	if statsErr != nil {
		return statsErr
	}
	return customStatsErr
}

func (c *containerData) updateCustomStats() (map[string][]info.MetricVal, error) {
	_, customStats, customStatsErr := c.collectorManager.Collect()
	if customStatsErr != nil {
		if !c.handler.Exists() {
			return customStats, nil
		}
		customStatsErr = fmt.Errorf("%v, continuing to push custom stats", customStatsErr)
	}
	return customStats, customStatsErr
}

func (c *containerData) updateSubcontainers() error {
	var subcontainers info.ContainerReferenceSlice
	subcontainers, err := c.handler.ListContainers(container.ListSelf)
	if err != nil {
		// Ignore errors if the container is dead.
		if !c.handler.Exists() {
			return nil
		}
		return err
	}
	sort.Sort(subcontainers)
	c.lock.Lock()
	defer c.lock.Unlock()
	c.info.Subcontainers = subcontainers
	return nil
}
