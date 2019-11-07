#!/bin/bash

cd $GOPATH/src/github.com/ktock/remote-snapshotter
go test -test.root
go test ./...
