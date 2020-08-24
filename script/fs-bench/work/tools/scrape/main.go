/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	proc "github.com/prometheus/procfs"
)

var (
	pid      = flag.Int("pid", 1, "process to watch")
	interval = flag.Duration("interval", time.Second, "how often scrape process metrics once")
	output   = flag.String("output", "-", "output file, specify - for /dev/stdout")
	netinf   = flag.String("netinf", "eth0", "network interface name")

	outputFile = os.Stdout
)

func main() {
	flag.Parse()

	if *output != "-" && *output != "/dev/stdout" {
		var err error
		outputFile, err = os.OpenFile(*output, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			log.Fatalf("open %q: %s", *output, err)
		}
	}

	wg := &sync.WaitGroup{}
	wg.Add(1)
	go worker(wg, SetupSignalHandler())
	wg.Wait()
}

func worker(wg *sync.WaitGroup, stop <-chan struct{}) {
	defer wg.Done()

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	scrape(time.Now())

	for {
		select {
		case <-stop:
			return
		case now := <-ticker.C:
			scrape(now)
		}
	}
}

// See https://github.com/prometheus/procfs/blob/master/proc_stat.go for details on userHZ.
const userHz = 100.0

var pagesize = uint64(os.Getpagesize())

func formatUint(u uint64) string {
	return strconv.FormatUint(u, 10)
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func scrape(now time.Time) {
	fs, err := proc.NewDefaultFS()
	if err != nil {
		log.Printf("read /proc: %s", err)
		return
	}

	nds, err := fs.NetDev()
	if err != nil {
		log.Printf("read /proc/net/dev: %s", err)
		return
	}

	net, ok := nds[*netinf]
	if !ok {
		log.Printf("not found interface %s", *netinf)
		return
	}

	pp, err := fs.NewProc(*pid)
	if err != nil {
		log.Printf("read /proc/%d: %s", *pid, err)
		return
	}

	nfd, err := pp.FileDescriptorsLen()
	if err != nil {
		log.Printf("count number of FD: %s", err)
		return
	}

	pio, err := pp.IO()
	if err != nil {
		log.Printf("read proc i/o stats: %s", err)
		return
	}

	pstat, err := pp.Stat()
	if err != nil {
		log.Printf("read proc stat: %s", err)
		return
	}

	ts := " " + strconv.FormatInt(now.Unix()*1000+int64(now.Nanosecond())/1e6, 10) + " "
	sb := &strings.Builder{}

	// iops
	sb.WriteString("syscr" + ts + formatUint(pio.SyscR) + "\n")
	sb.WriteString("syscw" + ts + formatUint(pio.SyscW) + "\n")

	// bw
	sb.WriteString("readbytes" + ts + formatUint(pio.ReadBytes) + "\n")
	sb.WriteString("writebytes" + ts + formatUint(pio.WriteBytes) + "\n")

	// cpu time in seconds
	sb.WriteString("utime" + ts + formatFloat(float64(pstat.UTime)/userHz) + "\n")
	sb.WriteString("ctime" + ts + formatFloat(float64(pstat.STime)/userHz) + "\n")
	sb.WriteString("time" + ts + formatFloat(pstat.CPUTime()) + "\n")

	// mem in bytes
	sb.WriteString("resident" + ts + strconv.Itoa(pstat.ResidentMemory()) + "\n")
	sb.WriteString("virtual" + ts + formatUint(uint64(pstat.VirtualMemory())) + "\n")

	// page faults
	sb.WriteString("majflt" + ts + formatUint(uint64(pstat.MinFlt)) + "\n")
	sb.WriteString("minflt" + ts + formatUint(uint64(pstat.MajFlt)) + "\n")

	// fd
	sb.WriteString("fd" + ts + strconv.Itoa(nfd) + "\n")

	// net
	sb.WriteString("rxbytes" + ts + formatUint(net.RxBytes) + "\n")
	sb.WriteString("txbytes" + ts + formatUint(net.TxBytes) + "\n")
	sb.WriteString("rxpkts" + ts + formatUint(net.RxPackets) + "\n")
	sb.WriteString("txpkts" + ts + formatUint(net.TxPackets) + "\n")

	if n, err := fmt.Fprint(outputFile, sb.String()); err != nil {
		log.Printf("%s: write %s, written %d bytes, err %s", now, *output, n, err)
	}
}

var onlyOneSignalHandler = make(chan struct{})
var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// SetupSignalHandler registered for SIGTERM and SIGINT. A stop channel is returned
// which is closed on one of these signals. If a second signal is caught, the program
// is terminated with exit code 1.
func SetupSignalHandler() <-chan struct{} {
	close(onlyOneSignalHandler) // panics when called twice

	stop := make(chan struct{})
	c := make(chan os.Signal, 2)
	signal.Notify(c, shutdownSignals...)
	go func() {
		<-c
		close(stop)
		<-c
		os.Exit(1) // second signal. Exit directly.
	}()

	return stop
}
