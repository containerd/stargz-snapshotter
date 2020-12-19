#!/bin/bash

set -euo pipefail

echo "/tmp/storage/$(echo -n "${1}" | base64 -w 0)/${2}/diff"
