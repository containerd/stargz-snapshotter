#!/bin/bash

#   Copyright The containerd Authors.

#   Licensed under the Apache License, Version 2.0 (the "License");
#   you may not use this file except in compliance with the License.
#   You may obtain a copy of the License at

#       http://www.apache.org/licenses/LICENSE-2.0

#   Unless required by applicable law or agreed to in writing, software
#   distributed under the License is distributed on an "AS IS" BASIS,
#   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#   See the License for the specific language governing permissions and
#   limitations under the License.

set -euo pipefail

# Copied from moby project (Apache License 2.0)
# https://github.com/moby/moby/blob/a9fe88e395acaacd84067b5fc701d52dbcf4b625/hack/dind#L28-L38
# cgroup v2: enable nesting
if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
       # move the processes from the root group to the /init group,
       # otherwise writing subtree_control fails with EBUSY.
       # An error during moving non-existent process (i.e., "cat") is ignored.
       mkdir -p /sys/fs/cgroup/init
       xargs -rn1 < /sys/fs/cgroup/cgroup.procs > /sys/fs/cgroup/init/cgroup.procs || :
       # enable controllers
       sed -e 's/ / +/g' -e 's/^/+/' < /sys/fs/cgroup/cgroup.controllers \
               > /sys/fs/cgroup/cgroup.subtree_control
fi

exec $@
