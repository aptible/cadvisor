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

package influxdb2

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"sync"
	"time"

	"golang.org/x/net/context"

	info "github.com/google/cadvisor/info/v1"
	"github.com/google/cadvisor/storage"
	"github.com/google/cadvisor/version"

	influxdb "github.com/influxdata/influxdb-client-go"
	"github.com/influxdata/influxdb-client-go/api/write"
)

func init() {
	storage.RegisterStorageDriver("influxdb", new)
}

type influxdbStorage struct {
	client          *influxdb.Client
	machineName     string
	bucket          string
	org             string
	retentionPolicy string
	bufferDuration  time.Duration
	lastWrite       time.Time
	points          []*write.Point
	lock            sync.Mutex
	readyToFlush    func() bool
}

var (
	argInfluxOrg = flag.String("storage_driver_influx2_org", "OrgName", "Influxdb2 organization name")
)

// Series names
const (
	// Cumulative CPU usage
	serCpuUsageTotal  string = "cpu_usage_total"
	serCpuUsageSystem string = "cpu_usage_system"
	serCpuUsageUser   string = "cpu_usage_user"
	serCpuThrottled   string = "cpu_throttled"
	// Smoothed average of number of runnable threads x 1000.
	serLoadAverage string = "load_average"
	// Memory Usage
	serMemoryUsage string = "memory_usage"
	// RSS size
	serMemoryRSS string = "memory_rss"
	// Working set size
	serMemoryWorkingSet string = "memory_working_set"
	// Cumulative count of bytes received.
	serRxBytes string = "rx_bytes"
	// Cumulative count of receive errors encountered.
	serRxErrors string = "rx_errors"
	// Cumulative count of bytes transmitted.
	serTxBytes string = "tx_bytes"
	// Cumulative count of transmit errors encountered.
	serTxErrors string = "tx_errors"
	// Filesystem device.
	serFsDevice string = "fs_device"
	// Filesystem limit.
	serFsLimit string = "fs_limit"
	// Filesystem usage.
	serFsUsage string = "fs_usage"
	// Serviced IO bytes
	serIoBytes string = "io_bytes"
	// Serviced IO operations
	serIoOps string = "io_ops"
)

func new() (storage.StorageDriver, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	return newStorage(
		hostname,
		*storage.ArgDbName,
		*argInfluxOrg,
		*storage.ArgDbPassword,
		*storage.ArgDbHost,
		*storage.ArgDbIsSecure,
		*storage.ArgDbBufferDuration,
	)
}

// Field names
const (
	fieldValue string = "value"
)

// Tag names
const (
	tagContainerId string = "container_id"
	tagDevice      string = "device"
)

func (self *influxdbStorage) containerFilesystemStatsToPoints(
	ref info.ContainerReference,
	stats *info.ContainerStats) (points []*write.Point) {
	if len(stats.Filesystem) == 0 {
		return points
	}
	for _, fsStat := range stats.Filesystem {
		tagsFsUsage := map[string]string{
			tagDevice: fsStat.Device,
		}
		fieldsFsUsage := map[string]interface{}{
			fieldValue: int64(fsStat.Usage),
		}
		pointFsUsage := influxdb.NewPoint(serFsUsage, tagsFsUsage, fieldsFsUsage, stats.Timestamp)

		tagsFsLimit := map[string]string{
			tagDevice: fsStat.Device,
		}
		fieldsFsLimit := map[string]interface{}{
			fieldValue: int64(fsStat.Limit),
		}
		pointFsLimit := influxdb.NewPoint(serFsLimit, tagsFsLimit, fieldsFsLimit, stats.Timestamp)

		points = append(points, pointFsUsage, pointFsLimit)
	}

	self.tagPoints(ref, stats, points)

	return points
}

// Set tags and timestamp for all points of the batch.
// Points should inherit the tags that are set for BatchPoints, but that does not seem to work.
func (self *influxdbStorage) tagPoints(ref info.ContainerReference, stats *info.ContainerStats, points []*write.Point) {
	commonTags := map[string]string{
		tagContainerId: ref.Name,
	}

	for i := 0; i < len(points); i++ {
		// merge with existing tags if any
		addTagsToPoint(points[i], commonTags)
		points[i].SetTime(stats.Timestamp)
	}
}

func (self *influxdbStorage) containerStatsToPoints(
	ref info.ContainerReference,
	stats *info.ContainerStats,
) (points []*write.Point) {
	// CPU usage: Total usage in nanoseconds
	points = append(points, makePoint(serCpuUsageTotal, stats.Cpu.Usage.Total))

	// CPU usage: Time spend in system space (in nanoseconds)
	points = append(points, makePoint(serCpuUsageSystem, stats.Cpu.Usage.System))

	// CPU usage: Time spent in user space (in nanoseconds)
	points = append(points, makePoint(serCpuUsageUser, stats.Cpu.Usage.User))

	// CPU usage: Time throttled (in nanoseconds)
	points = append(points, makePoint(serCpuThrottled, stats.Cpu.Usage.Throttled))

	// Load Average
	points = append(points, makePoint(serLoadAverage, stats.Cpu.LoadAverage))

	// Memory Usage
	points = append(points, makePoint(serMemoryUsage, stats.Memory.Usage))

	// RSS
	points = append(points, makePoint(serMemoryRSS, stats.Memory.RSS))

	// IO stats
	var readBytes, writeBytes, readOps, writeOps uint64 = 0, 0, 0, 0

	for _, diskStats := range stats.DiskIo.IoServiceBytes {
		readBytes += diskStats.Stats["Read"]
		writeBytes += diskStats.Stats["Write"]
	}

	for _, diskStats := range stats.DiskIo.IoServiced {
		readOps += diskStats.Stats["Read"]
		writeOps += diskStats.Stats["Write"]
	}

	points = append(points, makePoint(serIoBytes, readBytes+writeBytes))
	points = append(points, makePoint(serIoOps, readOps+writeOps))

	// Network Stats
	points = append(points, makePoint(serRxBytes, stats.Network.RxBytes))
	points = append(points, makePoint(serRxErrors, stats.Network.RxErrors))
	points = append(points, makePoint(serTxBytes, stats.Network.TxBytes))
	points = append(points, makePoint(serTxErrors, stats.Network.TxErrors))

	self.tagPoints(ref, stats, points)

	return points
}

func (self *influxdbStorage) OverrideReadyToFlush(readyToFlush func() bool) {
	self.readyToFlush = readyToFlush
}

func (self *influxdbStorage) defaultReadyToFlush() bool {
	return time.Since(self.lastWrite) >= self.bufferDuration
}

func (self *influxdbStorage) AddStats(ref info.ContainerReference, stats *info.ContainerStats) error {
	if stats == nil {
		return nil
	}
	var pointsToFlush []*write.Point
	func() {
		// AddStats will be invoked simultaneously from multiple threads and only one of them will perform a write.
		self.lock.Lock()
		defer self.lock.Unlock()

		self.points = append(self.points, self.containerStatsToPoints(ref, stats)...)
		self.points = append(self.points, self.containerFilesystemStatsToPoints(ref, stats)...)
		if self.readyToFlush() {
			pointsToFlush = self.points
			self.points = make([]*write.Point, 0)
			self.lastWrite = time.Now()
		}
	}()
	if len(pointsToFlush) > 0 {
		points := make([]*write.Point, len(pointsToFlush))
		for i, p := range pointsToFlush {
			points[i] = p
		}

		err := (*self.client).WriteAPIBlocking(self.org, self.bucket).WritePoint(context.Background(), points...)
		if err != nil {
			return fmt.Errorf("failed to write stats to influxDb - %s", err)
		}
	}
	return nil
}

func (self *influxdbStorage) Close() error {
	self.client = nil
	return nil
}

// machineName: A unique identifier to identify the host that current cAdvisor
// instance is running on.
// influxdbHost: The host which runs influxdb (host:port)
func newStorage(
	machineName,
	bucket,
	org,
	password,
	influxdbHost string,
	isSecure bool,
	bufferDuration time.Duration,
) (*influxdbStorage, error) {
	url := &url.URL{
		Scheme: "http",
		Host:   influxdbHost,
	}
	if isSecure {
		url.Scheme = "https"
	}

	config := influxdb.DefaultOptions()
	config.SetApplicationName(fmt.Sprintf("%v/%v", "cAdvisor", version.Info["version"]))

	client := influxdb.NewClientWithOptions(url.String(), password, config)

	ret := &influxdbStorage{
		client:         &client,
		machineName:    machineName,
		bucket:         bucket,
		bufferDuration: bufferDuration,
		lastWrite:      time.Now(),
		points:         make([]*write.Point, 0),
		org:            org,
	}
	ret.readyToFlush = ret.defaultReadyToFlush
	return ret, nil
}

// Creates a measurement point with a single value field
func makePoint(name string, value interface{}) *write.Point {
	return influxdb.NewPointWithMeasurement(name).AddField(fieldValue, toSignedIfUnsigned(value))
}

// Adds additional tags to the existing tags of a point
func addTagsToPoint(point *write.Point, tags map[string]string) {
	for k, v := range tags {
		point.AddTag(k, v)
	}
}

// Some stats have type unsigned integer, but the InfluxDB client accepts only signed integers.
func toSignedIfUnsigned(value interface{}) interface{} {
	switch v := value.(type) {
	case uint64:
		return int64(v)
	case uint32:
		return int32(v)
	case uint16:
		return int16(v)
	case uint8:
		return int8(v)
	case uint:
		return int(v)
	}
	return value
}
