module github.com/containerd/stargz-snapshotter/script/fs-bench/tools

go 1.13

// DO NOT use `go mod tidy` here, it will probably introduce a problem like:
// https://github.com/prometheus/prometheus/issues/5590

require (
	github.com/prometheus/procfs v0.0.12-0.20200513160535-c6ff04bafc38
	github.com/prometheus/prometheus v1.8.2-0.20191017095924-6f92ce560538 // indirect
)
