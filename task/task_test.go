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
	"sync"
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

	wait := func(t *testing.T, name string, done func() bool) {
		ch := make(chan struct{})
		go func() {
			for !done() {
				time.Sleep(10 * time.Millisecond)
			}
			ch <- struct{}{}
		}()
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout for %q", name)
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
				wait(t, "task1 started", task1.checkStarted())
				doGo(func() { pm.InvokeBackgroundTask(task2.do, 24*time.Hour) })
				wait(t, "task2 started", task2.checkStarted())
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
				wait(t, "task1 started", task1.checkStarted())
				doGo(func() { pm.InvokeBackgroundTask(task2.do, 24*time.Hour) })
				wait(t, "task2 started", task2.checkStarted())
				pm.DoPrioritizedTask()
				wait(t, "task1 canceled", task1.checkCanceled())
				wait(t, "task2 canceled", task2.checkCanceled())
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
				wait(t, "task1 started", task1.checkStarted())
				doGo(func() { pm.InvokeBackgroundTask(task2.do, 24*time.Hour) })
				wait(t, "task2 started", task2.checkStarted())
				pm.DoPrioritizedTask()
				wait(t, "task1 canceled", task1.checkCanceled())
				wait(t, "task2 canceled", task2.checkCanceled())
				task1.reset()
				task2.reset()
				pm.DonePrioritizedTask()
				wait(t, "task1 resumed", task1.checkStarted())
				wait(t, "task2 resumed", task2.checkStarted())
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
				wait(t, "task1 started", task1.checkStarted())
				doGo(func() { pm.InvokeBackgroundTask(task2.do, 24*time.Hour) })
				task1.finish()
				wait(t, "task1 done", task1.checkDone())
				wait(t, "task2 started", task2.checkStarted())
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
				wait(t, "task1 started", task1.checkStarted())
				doGo(func() { pm.InvokeBackgroundTask(task2.do, 24*time.Hour) })
				task1.finish()
				wait(t, "task1 done", task1.checkDone())
				wait(t, "task2 started", task2.checkStarted())
				task2.finish()
				wait(t, "task2 done", task2.checkDone())
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
				t.Errorf("assertion failed:status=(task1:%q,task2:%q,task3:%q,task4:%q)",
					task1.dumpStatus(), task2.dumpStatus(),
					task3.dumpStatus(), task4.dumpStatus())
			}
		})
	}
}

type sampleTask struct {
	started  bool
	done     bool
	canceled bool
	mu       sync.Mutex

	finishCh chan struct{}
}

func newSampleTask() *sampleTask {
	return &sampleTask{
		finishCh: make(chan struct{}),
	}
}

func (st *sampleTask) checkStarted() func() bool {
	return func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.started
	}
}

func (st *sampleTask) checkDone() func() bool {
	return func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.done
	}
}

func (st *sampleTask) checkCanceled() func() bool {
	return func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.canceled
	}
}

func (st *sampleTask) do(ctx context.Context) {
	st.mu.Lock()
	st.started = true
	st.mu.Unlock()
	select {
	case <-st.finishCh:
		st.mu.Lock()
		st.done = true
		st.mu.Unlock()
	case <-ctx.Done():
		st.mu.Lock()
		st.canceled = true
		st.mu.Unlock()
	}
}

func (st *sampleTask) finish() {
	st.finishCh <- struct{}{}
}

func (st *sampleTask) reset() {
	st.mu.Lock()
	st.started, st.done, st.canceled = false, false, false
	st.mu.Unlock()
	st.finishCh = make(chan struct{})
}

func (st *sampleTask) dumpStatus() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return fmt.Sprintf("started=%v,done=%v,canceled=%v", st.started, st.done, st.canceled)
}

func (st *sampleTask) assert(started, done, canceled bool) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return (st.started == started) && (st.done == done) && (st.canceled == canceled)
}
