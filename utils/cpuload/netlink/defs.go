// Copyright 2015 Google Inc. All Rights Reserved.
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

package netlink

/*
#include <linux/genetlink.h>
#include <linux/taskstats.h>
#include <linux/cgroupstats.h>
*/
import "C"

const (
	GENL_ID_CTRL                  = C.GENL_ID_CTRL
	CTRL_ATTR_FAMILY_ID           = C.CTRL_ATTR_FAMILY_ID
	CTRL_ATTR_FAMILY_NAME         = C.CTRL_ATTR_FAMILY_NAME
	CTRL_CMD_GETFAMILY            = C.CTRL_CMD_GETFAMILY
	TASKSTATS_GENL_NAME           = C.TASKSTATS_GENL_NAME
	TASKSTATS_GENL_VERSION        = C.TASKSTATS_GENL_VERSION
	CGROUPSTATS_CMD_GET           = C.CGROUPSTATS_CMD_GET
	CGROUPSTATS_CMD_ATTR_FD       = C.CGROUPSTATS_CMD_ATTR_FD
	CGROUPSTATS_TYPE_CGROUP_STATS = C.CGROUPSTATS_TYPE_CGROUP_STATS
)
