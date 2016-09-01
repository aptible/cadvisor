// Copyright 2016 Google Inc. All Rights Reserved.
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

// +build linux

package fs

import (
	dockerMountInfo "github.com/docker/docker/pkg/mount"
	"path/filepath"
)

type mountInfoClient interface {
	GetMounts() ([]*dockerMountInfo.Info, error)
}

type defaultMountInfoClient struct{}

func (*defaultMountInfoClient) GetMounts() ([]*dockerMountInfo.Info, error) {
	mounts, err := dockerMountInfo.GetMounts()
	if err != nil {
		return nil, err
	}

	for _, mount := range mounts {
		// Dereference the device name to account for symlinks (e.g. /dev/disk/by-uuid/...)
		// Not all mounts can be de-referenced though, so we skip silently mounts that cannot.
		deviceName, err := filepath.EvalSymlinks(mount.Source)
		if err != nil {
			continue
		}
		mount.Source = deviceName
	}

	return mounts, nil
}

var _ mountInfoClient = &defaultMountInfoClient{}
