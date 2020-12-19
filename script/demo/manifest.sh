#!/bin/bash

set -euo pipefail

REF=$(echo -n "${1}" | base64 -w 0)
echo "/tmp/storage/${REF}/manifest"
