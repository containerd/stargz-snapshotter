#!/bin/bash

CACHE_TYPE="${1}"
CONTAINERD_ROOT=/var/lib/containerd
CONFIG_FILE=/etc/containerd/config.toml

if [ "${CACHE_TYPE}" != "" ] ; then 
    sed -i 's/http_cache_type = .*/http_cache_type = "'"${CACHE_TYPE}"'"/g' "${CONFIG_FILE}"
    sed -i 's/filesystem_cache_type = .*/filesystem_cache_type = "'"${CACHE_TYPE}"'"/g' "${CONFIG_FILE}"
fi
