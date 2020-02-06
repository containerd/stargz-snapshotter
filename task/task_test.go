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

package task

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestBackgroundTasks tests background task manager.
func TestBackgroundTasks(t *testing.T) {
	doGo := func(f func()) {
		invoked := make(chan struct{})
		go func() {
			invoked <- struct{}{}
			f()
		}()
		<-invoked
		time.Sleep(10 * time.Millisecond)
	}

	wait := func(t *testing.T, name string, c *bool) {
		ch := make(chan struct{})
		go func() {
			for !*c {
				time.Sleep(10 * time.Millisecond)
			}
			ch <- struct{}{}
		}()
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout for %s", name)
		}
	}

	tests := []struct {
		name          string
		concurrency   int64
		checkInterval time.Duration
		context       func(t *testing.T, pm *BackgroundTaskManager, task1, task2, task3, task4 *sampleTask)
		assert        func(task1, task2, task3, task4 *sampleTask) bool
	}{
		{
			name:          "privilege_running",
			concurrency:   1,
			checkInterval: 100 * time.Millisecond,
			context: func(t *testing.T, pm *BackgroundTaskManager, task1, task2, task3, task4 *sampleTask) {
				pm.DoPrioritizedTask()
				doGo(func() { pm.InvokeBackgroundTask(task1.do, 24*time.Hour) })
				time.Sleep(300 * time.Millisecond) // wait for long time...
			},
			assert: func(task1, task2, task3, task4 *sampleTask) bool {
				return (task1.assert(false, false, false))
			},
		},
		{
			name:          "concurrency",
			concurrency:   2,
			checkInterval: time.Duration(0), // We don't care prioritized tasks now
			context: func(t *testing.T, pm *BackgroundTaskManager, task1, task2, task3, task4 *sampleTask) {
				doGo(func() { pm.InvokeBackgroundTask(task1.do, 24*time.Hour) })
				wait(t, "task1 started", &task1.started)
				doGo(func() { pm.InvokeBackgroundTask(task2.do, 24*time.Hour) })
				wait(t, "task2 started", &task2.started)
				doGo(func() { pm.InvokeBackgroundTask(task3.do, 24*time.Hour) })
				doGo(func() { pm.InvokeBackgroundTask(task4.do, 24*time.Hour) })
				time.Sleep(300 * time.Millisecond) // wait for long time...
			},
			assert: func(task1, task2, task3, task4 *sampleTask) bool {
				return (task1.assert(true, false, false) &&
					task2.assert(true, false, false) &&
					task3.assert(false, false, false) &&
					task4.assert(false, false, false))
			},
		},
		{
			name:          "cancel",
			concurrency:   2,
			checkInterval: 100 * time.Millisecond,
			context: func(t *testing.T, pm *BackgroundTaskManager, task1, task2, task3, task4 *sampleTask) {
				doGo(func() { pm.InvokeBackgroundTask(task1.do, 24*time.Hour) })
				wait(t, "task1 started", &task1.started)
				doGo(func() { pm.InvokeBackgroundTask(task2.do, 24*time.Hour) })
				wait(t, "task2 started", &task2.started)
				pm.DoPrioritizedTask()
				wait(t, "task1 canceled", &task1.canceled)
				wait(t, "task2 canceled", &task2.canceled)
			},
			assert: func(task1, task2, task3, task4 *sampleTask) bool {
				return (task1.assert(true, false, true) &&
					task2.assert(true, false, true))
			},
		},
		{
			name:          "resume",
			concurrency:   2,
			checkInterval: 100 * time.Millisecond,
			context: func(t *testing.T, pm *BackgroundTaskManager, task1, task2, task3, task4 *sampleTask) {
				doGo(func() { pm.InvokeBackgroundTask(task1.do, 24*time.Hour) })
				wait(t, "task1 started", &task1.started)
				doGo(func() { pm.InvokeBackgroundTask(task2.do, 24*time.Hour) })
				wait(t, "task2 started", &task2.started)
				pm.DoPrioritizedTask()
				wait(t, "task1 canceled", &task1.canceled)
				wait(t, "task2 canceled", &task2.canceled)
				task1.reset()
				task2.reset()
				pm.DonePrioritizedTask()
				wait(t, "task1 resumed", &task1.started)
				wait(t, "task2 resumed", &task2.started)
			},
			assert: func(task1, task2, task3, task4 *sampleTask) bool {
				return (task1.assert(true, false, false) &&
					task2.assert(true, false, false))
			},
		},
		{
			name:          "finish_partial",
			concurrency:   1,
			checkInterval: time.Duration(0), // We don't care prioritized tasks now
			context: func(t *testing.T, pm *BackgroundTaskManager, task1, task2, task3, task4 *sampleTask) {
				doGo(func() { pm.InvokeBackgroundTask(task1.do, 24*time.Hour) })
				wait(t, "task1 started", &task1.started)
				doGo(func() { pm.InvokeBackgroundTask(task2.do, 24*time.Hour) })
				task1.finish()
				wait(t, "task1 done", &task1.done)
				wait(t, "task2 started", &task2.started)
			},
			assert: func(task1, task2, task3, task4 *sampleTask) bool {
				return (task1.assert(true, true, false) &&
					task2.assert(true, false, false))
			},
		},
		{
			name:          "finish_all",
			concurrency:   1,
			checkInterval: time.Duration(0), // We don't care prioritized tasks now
			context: func(t *testing.T, pm *BackgroundTaskManager, task1, task2, task3, task4 *sampleTask) {
				doGo(func() { pm.InvokeBackgroundTask(task1.do, 24*time.Hour) })
				wait(t, "task1 started", &task1.started)
				doGo(func() { pm.InvokeBackgroundTask(task2.do, 24*time.Hour) })
				task1.finish()
				wait(t, "task1 done", &task1.done)
				wait(t, "task2 started", &task2.started)
				task2.finish()
				wait(t, "task2 done", &task2.done)
			},
			assert: func(task1, task2, task3, task4 *sampleTask) bool {
				return (task1.assert(true, true, false) &&
					task2.assert(true, true, false))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				pm    = NewBackgroundTaskManager(tt.concurrency, tt.checkInterval)
				task1 = newSampleTask()
				task2 = newSampleTask()
				task3 = newSampleTask()
				task4 = newSampleTask()
			)
			tt.context(t, pm, task1, task2, task3, task4)
			if !tt.assert(task1, task2, task3, task4) {
				t.Errorf("assertion failed: status=(task1:%s,task2:%s,task3:%s,task4:%s)",
					task1.dumpStatus(), task2.dumpStatus(), task3.dumpStatus(), task4.dumpStatus())
			}
		})
	}
}

type sampleTask struct {
	started  bool
	done     bool
	canceled bool
	finishCh chan struct{}
}

func newSampleTask() *sampleTask {
	return &sampleTask{
		finishCh: make(chan struct{}),
	}
}

func (st *sampleTask) do(ctx context.Context) {
	st.started = true
	select {
	case <-st.finishCh:
		st.done = true
	case <-ctx.Done():
		st.canceled = true
	}
}

func (st *sampleTask) finish() {
	st.finishCh <- struct{}{}
}

func (st *sampleTask) reset() {
	st.started, st.done, st.canceled = false, false, false
	st.finishCh = make(chan struct{})
}

func (st *sampleTask) dumpStatus() string {
	return fmt.Sprintf("started=%v,done=%v,canceled=%v", st.started, st.done, st.canceled)
}

func (st *sampleTask) assert(started, done, canceled bool) bool {
	return (st.started == started) && (st.done == done) && (st.canceled == canceled)
}
