#!/bin/bash

crfs &

if [ $# -gt 0 ] ; then
    exec $@
fi

exec /bin/bash
