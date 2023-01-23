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

REPO_PATH=/go/src/github.com/containerd/stargz-snapshotter

ipfs init
ipfs daemon --offline &
curl --retry 10 --retry-all-errors --retry-delay 3 -X POST localhost:5001/api/v0/version >/dev/null 2>&1 # wait for up

$@
