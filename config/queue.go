/*
Copyright 2021 The TestGrid Authors.

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

package config

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"bitbucket.org/creachadair/stringset"
	configpb "github.com/GoogleCloudPlatform/testgrid/pb/config"
	"github.com/sirupsen/logrus"
)

// TestGroupQueue can send test groups to receivers at a specific frequency.
//
// Also contains the ability to modify the next time to send groups.
// First call must be to Init().
// Exported methods are safe to call concurrently.
type TestGroupQueue struct {
	queue  priorityQueue
	items  map[string]*item
	lock   sync.RWMutex
	signal chan struct{}
}

// Init (or reinit) the queue with the specified groups, which should be updated at frequency.
func (q *TestGroupQueue) Init(testGroups []*configpb.TestGroup, when time.Time) {
	n := len(testGroups)
	found := stringset.NewSize(n)

	q.lock.Lock()
	defer q.lock.Unlock()
	defer q.rouse()

	if q.signal == nil {
		q.signal = make(chan struct{})
	}

	if q.items == nil {
		q.items = make(map[string]*item, n)
	}
	if q.queue == nil {
		q.queue = make(priorityQueue, 0, n)
	}
	items := q.items

	for _, tg := range testGroups {
		name := tg.Name
		found.Add(name)
		it, ok := items[name]
		if !ok {
			it = &item{
				tg:    tg,
				when:  when,
				index: len(q.queue),
			}
			heap.Push(&q.queue, it)
			items[name] = it
			logrus.WithFields(logrus.Fields{
				"when":  when,
				"group": name,
			}).Info("Adding group to queue")
		} else {
			it.tg = tg
		}
	}

	for name, it := range items {
		if found.Contains(name) {
			continue
		}
		logrus.WithField("group", name).Info("Removing group from queue")
		heap.Remove(&q.queue, it.index)
		delete(q.items, name)
	}
}

// FixAll will fix multiple groups inside a single critical section.
func (q *TestGroupQueue) FixAll(whens map[string]time.Time) error {
	q.lock.Lock()
	defer q.lock.Unlock()
	var missing []string
	defer q.rouse()

	for name, when := range whens {
		it, ok := q.items[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		if !when.Equal(it.when) {
			logrus.WithFields(logrus.Fields{
				"group": name,
				"when":  when,
			}).Info("Fixing groups")
			it.when = when
		}
	}
	heap.Init(&q.queue)
	if len(missing) > 0 {
		return fmt.Errorf("not found: %v", missing)
	}
	return nil
}

// Fix the next time to send the group to receivers.
func (q *TestGroupQueue) Fix(name string, when time.Time) error {
	q.lock.Lock()
	defer q.lock.Unlock()
	defer q.rouse()

	it, ok := q.items[name]
	if !ok {
		return errors.New("not found")
	}
	if !when.Equal(it.when) {
		logrus.WithFields(logrus.Fields{
			"group": name,
			"when":  when,
		}).Info("Fixed group")
		it.when = when
		heap.Fix(&q.queue, it.index)
	}
	return nil
}

// Status of the queue: depth, next item and when the next item is ready.
func (q *TestGroupQueue) Status() (int, *configpb.TestGroup, time.Time) {
	q.lock.RLock()
	defer q.lock.RUnlock()
	var tg *configpb.TestGroup
	var when time.Time
	if it := q.queue.peek(); it != nil {
		tg = it.tg
		when = it.when
	}
	return len(q.queue), tg, when
}

func (q *TestGroupQueue) rouse() {
	select {
	case q.signal <- struct{}{}: // wake up early
	default: // not sleeping
	}
}

func (q *TestGroupQueue) sleep(d time.Duration) {
	log := logrus.WithFields(logrus.Fields{
		"seconds": d.Round(100 * time.Millisecond).Seconds(),
	})
	if d > 5*time.Second {
		log.Info("Sleeping...")
	} else {
		log.Debug("Sleeping...")
	}
	sleep := time.NewTimer(d)
	select {
	case <-q.signal:
		if !sleep.Stop() {
			<-sleep.C
		}
		log.Info("Roused")
	case <-sleep.C:
	}
}

// Send test groups to receivers until the context expires.
//
// Pops items off the queue when frequency is zero.
// Otherwise reschedules the item after the specified frequency has elapsed.
func (q *TestGroupQueue) Send(ctx context.Context, receivers chan<- *configpb.TestGroup, frequency time.Duration) error {
	var next func() (*configpb.TestGroup, time.Time)
	if frequency == 0 {
		next = func() (*configpb.TestGroup, time.Time) {
			if len(q.queue) == 0 {
				return nil, time.Time{}
			}
			it := heap.Pop(&q.queue).(*item)
			return it.tg, it.when
		}
	} else {
		next = func() (*configpb.TestGroup, time.Time) {
			it := q.queue.peek()
			if it == nil {
				return nil, time.Time{}
			}
			when := it.when
			it.when = time.Now().Add(frequency)
			heap.Fix(&q.queue, it.index)
			return it.tg, when
		}
	}

	for {
		q.lock.Lock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		tg, when := next()
		q.lock.Unlock()

		if tg == nil {
			if frequency == 0 {
				return nil
			}
			q.sleep(time.Second)
			continue
		}

		if dur := when.Sub(time.Now()); dur > 0 {
			q.sleep(dur)
		}
		select {
		case receivers <- tg:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

type priorityQueue []*item

func (pq priorityQueue) Len() int { return len(pq) }
func (pq priorityQueue) Less(i, j int) bool {
	return pq[i].when.Before(pq[j].when)
}
func (pq priorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

func (pq *priorityQueue) Push(something interface{}) {
	it := something.(*item)
	it.index = len(*pq)
	*pq = append(*pq, it)
}

func (pq *priorityQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	it := old[n-1]
	it.index = -1
	old[n-1] = nil
	*pq = old[0 : n-1]
	return it
}

func (pq priorityQueue) peek() *item {
	n := len(pq)
	if n == 0 {
		return nil
	}
	return pq[0]
}

type item struct {
	tg    *configpb.TestGroup
	when  time.Time
	index int
}
