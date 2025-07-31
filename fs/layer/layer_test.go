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

/*
   Copyright 2019 The Go Authors. All rights reserved.
   Use of this source code is governed by a BSD-style
   license that can be found in the NOTICE.md file.
*/

package layer

import (
	"testing"
	"time"

	memorymetadata "github.com/containerd/stargz-snapshotter/metadata/memory"
)

func TestLayer(t *testing.T) {
	testRunner := &TestRunner{
		TestingT: t,
		Runner: func(testingT TestingT, name string, run func(t TestingT)) {
			tt, ok := testingT.(*testing.T)
			if !ok {
				testingT.Fatal("TestingT is not a *testing.T")
				return
			}

			tt.Run(name, func(t *testing.T) {
				run(t)
			})
		},
	}

	TestSuiteLayer(testRunner, memorymetadata.NewReader)
}

func TestWaiter(t *testing.T) {
	var (
		w         = newWaiter()
		waitTime  = time.Second
		startTime = time.Now()
		doneTime  time.Time
		done      = make(chan struct{})
	)

	go func() {
		defer close(done)
		if err := w.wait(10 * time.Second); err != nil {
			t.Errorf("failed to wait: %v", err)
			return
		}
		doneTime = time.Now()
	}()

	time.Sleep(waitTime)
	w.done()
	<-done

	if doneTime.Sub(startTime) < waitTime {
		t.Errorf("wait time is too short: %v; want %v", doneTime.Sub(startTime), waitTime)
	}
}
