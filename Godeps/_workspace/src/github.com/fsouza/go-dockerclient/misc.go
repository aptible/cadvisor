// Copyright 2015 go-dockerclient authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package docker

import (
	"encoding/json"
	"strings"
	"errors"
	"fmt"
)

type ConvertibleBool bool

func (bit ConvertibleBool) UnmarshalJSON(data []byte) error {
    asString := string(data)
    if asString == "1" || asString == "true" {
        bit = true
    } else if asString == "0" || asString == "false" {
        bit = false
    } else {
        return errors.New(fmt.Sprintf("Boolean unmarshal error: invalid input %s", asString))
    }
    return nil
}

// Version returns version information about the docker server.
//
// See https://goo.gl/ND9R8L for more details.
func (c *Client) Version() (*Env, error) {
	resp, err := c.do("GET", "/version", doOptions{})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var env Env
	if err := env.Decode(resp.Body); err != nil {
		return nil, err
	}
	return &env, nil
}

// DockerInfo contains information about the Docker server
//
// See https://goo.gl/bHUoz9 for more details.
type DockerInfo struct {
	ID                 string
	Containers         int
	ContainersRunning  int
	ContainersPaused   int
	ContainersStopped  int
	Images             int
	Driver             string
	DriverStatus       [][2]string
	SystemStatus       [][2]string
	Plugins            PluginsInfo
	MemoryLimit        ConvertibleBool
	SwapLimit          ConvertibleBool
	KernelMemory       ConvertibleBool
	CPUCfsPeriod       ConvertibleBool `json:"CpuCfsPeriod"`
	CPUCfsQuota        ConvertibleBool `json:"CpuCfsQuota"`
	CPUShares          ConvertibleBool
	CPUSet             ConvertibleBool
	IPv4Forwarding     ConvertibleBool
	BridgeNfIptables   ConvertibleBool
	BridgeNfIP6tables  ConvertibleBool `json:"BridgeNfIp6tables"`
	Debug              ConvertibleBool
	NFd                int
	OomKillDisable     ConvertibleBool
	NGoroutines        int
	SystemTime         string
	ExecutionDriver    string
	LoggingDriver      string
	CgroupDriver       string
	NEventsListener    int
	KernelVersion      string
	OperatingSystem    string
	OSType             string
	Architecture       string
	IndexServerAddress string
	NCPU               int
	MemTotal           int64
	DockerRootDir      string
	HTTPProxy          string `json:"HttpProxy"`
	HTTPSProxy         string `json:"HttpsProxy"`
	NoProxy            string
	Name               string
	Labels             []string
	ExperimentalBuild  ConvertibleBool
	ServerVersion      string
	ClusterStore       string
	ClusterAdvertise   string
}

// PluginsInfo is a struct with the plugins registered with the docker daemon
//
// for more information, see: https://goo.gl/bHUoz9
type PluginsInfo struct {
	// List of Volume plugins registered
	Volume []string
	// List of Network plugins registered
	Network []string
	// List of Authorization plugins registered
	Authorization []string
}

// Info returns system-wide information about the Docker server.
//
// See https://goo.gl/ElTHi2 for more details.
func (c *Client) Info() (*DockerInfo, error) {
	resp, err := c.do("GET", "/info", doOptions{})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var info DockerInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	return &info, nil
}

// ParseRepositoryTag gets the name of the repository and returns it splitted
// in two parts: the repository and the tag.
//
// Some examples:
//
//     localhost.localdomain:5000/samalba/hipache:latest -> localhost.localdomain:5000/samalba/hipache, latest
//     localhost.localdomain:5000/samalba/hipache -> localhost.localdomain:5000/samalba/hipache, ""
func ParseRepositoryTag(repoTag string) (repository string, tag string) {
	n := strings.LastIndex(repoTag, ":")
	if n < 0 {
		return repoTag, ""
	}
	if tag := repoTag[n+1:]; !strings.Contains(tag, "/") {
		return repoTag[:n], tag
	}
	return repoTag, ""
}
