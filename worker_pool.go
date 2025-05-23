package taskman

import (
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/xid"
)

const (
	// If the worker pool utilization is above this threshold, we will not scale down
	utilizationThreshold = 0.4
	// Minimum interval between downscaling events
	downScaleMinInterval = time.Second * 30
)

// workerPool manages a pool of workers that execute tasks.
type workerPool struct {
	workers           sync.Map     // Map worker ID (xid.ID) to worker (workerInfo)
	workersActive     atomic.Int32 // Number of active workers
	workersRunning    atomic.Int32 // Number of running workers
	workerCountTarget atomic.Int32 // Target number of workers

	errorChan       chan<- error       // Send-only channel for errors
	execTimeChan    chan time.Duration // Channel to send execution times
	taskChan        <-chan Task        // Receive-only channel for tasks
	workerCountChan chan int32         // Channel to receive worker count changes
	stopPoolChan    chan struct{}      // Channel to signal stopping the worker pool
	workerPoolDone  chan struct{}      // Channel to signal worker pool is done

	workerScalingEvents atomic.Int64 // Number of worker scaling events since start
	lastDownScale       time.Time    // Last time a downscaling event occurred

	mu sync.Mutex
	wg sync.WaitGroup
}

// worker represents a worker that executes tasks.
type workerInfo struct {
	id   xid.ID      // The worker ID
	busy atomic.Bool // True if worker is busy

	stopChan chan struct{} // Channel to signal stopping the worker
	stopOnce sync.Once     // Once to ensure stop signal is sent only once
}

// activeWorkers returns the number of active workers.
func (wp *workerPool) activeWorkers() int32 {
	return wp.workersActive.Load()
}

// runningWorkers returns the number of running workers.
func (wp *workerPool) runningWorkers() int32 {
	return wp.workersRunning.Load()
}

// targetWorkerCount returns the pool's target worker count.
func (wp *workerPool) targetWorkerCount() int32 {
	return wp.workerCountTarget.Load()
}

func (wp *workerPool) availableWorkers() int32 {
	return wp.runningWorkers() - wp.activeWorkers()
}

// addWorkers adds to the worker pool by starting new workers.
func (wp *workerPool) addWorkers(nWorkers int) {
	logger.Debug().Msgf("Adding %d new workers to the pool", nWorkers)
	wp.wg.Add(nWorkers)
	for range nWorkers {
		workerID := xid.New()
		go wp.startWorker(workerID)
	}
}

// adjustWorkerCount adjusts the number of workers in the pool to match the target worker count.
func (pool *workerPool) adjustWorkerCount(newTargetCount int32) {
	pool.workerScalingEvents.Add(1)
	currentTarget := pool.targetWorkerCount()

	// Update desired target count
	pool.workerCountTarget.Store(newTargetCount)

	switch {
	case newTargetCount > currentTarget:
		// Scale up
		logger.Debug().Msgf("Scaling worker count UP from %d to %d", currentTarget, newTargetCount)
		pool.addWorkers(int(newTargetCount - currentTarget))

	case newTargetCount < currentTarget:
		// Scale down based on utilization and debounce
		if pool.utilization() < utilizationThreshold && time.Since(pool.lastDownScale) >= downScaleMinInterval {
			logger.Debug().Msgf("Scaling worker count DOWN from %d to %d", currentTarget, newTargetCount)
			if err := pool.stopWorkers(int(currentTarget - newTargetCount)); err != nil {
				logger.Warn().Err(err).Msg("stopWorkers failed")
			} else {
				pool.lastDownScale = time.Now()
			}
		} else {
			logger.Debug().
				Msgf("Skipping down-scale: util=%.2f, sinceLast=%s",
					pool.utilization(), time.Since(pool.lastDownScale))
		}

	default:
		logger.Debug().Msgf("Pool already at target worker count %d", newTargetCount)
	}
}

// busyStateWorkers returns two slices of worker IDs:
// 1. Busy workers
// 2. Idle workers
func (wp *workerPool) busyAndIdleWorkers() ([]xid.ID, []xid.ID) {
	var busyWorkers []xid.ID
	var idleWorkers []xid.ID
	wp.workers.Range(func(key, value any) bool {
		workerID := key.(xid.ID)
		workerInfo := value.(*workerInfo)
		if workerInfo.busy.Load() {
			busyWorkers = append(busyWorkers, workerID)
		} else {
			idleWorkers = append(idleWorkers, workerID)
		}
		return true
	})
	return busyWorkers, idleWorkers
}

// busyWorkers returns a slice of currently busy workers.
func (wp *workerPool) busyWorkers() []xid.ID {
	busyWorkers, _ := wp.busyAndIdleWorkers()
	return busyWorkers
}

// enqueueWorkerScaling enqueues a worker count scaling request.
func (p *workerPool) enqueueWorkerScaling(target int32) {
	select {
	case <-p.stopPoolChan:
		// Worker pool is shutting down, exit
		return
	default:
	}

	// Drain any stale target so the buffer never blocks
	select {
	case <-p.workerCountChan:
	default:
	}

	// Attempt to send, but abort if stopPoolChan closes
	select {
	case p.workerCountChan <- target:
	case <-p.stopPoolChan:
	}
}

// idleWorkers returns a slice of currently idle workers.
func (wp *workerPool) idleWorkers() []xid.ID {
	_, idleWorkers := wp.busyAndIdleWorkers()
	return idleWorkers
}

// processWorkerCountScaling listens for worker count requests and adjusts the worker count accordingly.
func (wp *workerPool) processWorkerCountScaling() {
	for {
		select {
		case <-wp.stopPoolChan:
			// Worker count scaling received stop signal, exiting
			return
		case newTargetCount := <-wp.workerCountChan:
			wp.mu.Lock()
			wp.adjustWorkerCount(newTargetCount)
			wp.mu.Unlock()
		}
	}
}

// startWorker executes tasks from the task channel.
func (wp *workerPool) startWorker(id xid.ID) {
	logger.Debug().Msgf("Starting worker %s", id)

	wp.workersRunning.Add(1)
	worker := &workerInfo{
		id:       id,
		busy:     atomic.Bool{},
		stopChan: make(chan struct{}),
	}
	wp.workers.Store(id, worker)

	defer func() {
		wp.workersRunning.Add(-1)
		wp.workers.Delete(id)
		wp.wg.Done()
	}()

	for {
		select {
		case task, ok := <-wp.taskChan:
			if !ok {
				logger.Debug().Msgf("Worker %s: task channel closed, exiting", id)
				return
			}
			logger.Trace().Msgf("Worker %s executing task", id)

			func() {
				// Update worker state: busy
				worker.busy.Store(true)
				wp.workersActive.Add(1)

				defer func() {
					if r := recover(); r != nil {
						logger.Error().Msgf("Worker %s: panic: %v\n%s", id, r, string(debug.Stack()))
						err := fmt.Errorf("worker %s: panic: %v", id, r)
						select {
						case wp.errorChan <- err:
							// Error sent
						default:
							// Error channel not ready to receive, do nothing
						}
					}

					// Update worker state: dormant
					worker.busy.Store(false)
					wp.workersActive.Add(-1)
					logger.Trace().Msgf("Worker %s: finished task", id)
				}()

				// Execute the task
				start := time.Now()
				err := task.Execute()
				if err != nil {
					// No retry policy is implemented, we just log and send the error for now
					select {
					case wp.errorChan <- err:
						// Error sent
					default:
						// Error channel not ready to receive, do nothing
					}
				}
				execTime := time.Since(start)
				select {
				case wp.execTimeChan <- execTime:
					// Execution time sent
				default:
					// Execution time channel not ready to receive, do nothing
				}
			}()

		case <-worker.stopChan:
			logger.Debug().Msgf("Worker %s: received targeted stop signal, exiting", id)
			return

		case <-wp.stopPoolChan:
			logger.Debug().Msgf("Worker %s: received global stop signal, exiting", id)
			return
		}
	}
}

// stop signals the worker pool to stop processing tasks and exit.
func (wp *workerPool) stop() {
	// Signal workers to stop
	close(wp.stopPoolChan)

	// Wait for all workers to finish
	wp.wg.Wait()

	// Signal worker pool is done
	close(wp.workerPoolDone)
}

// stopWorker signals a specific worker to stop processing tasks and exit. This will also remove
// the worker from the worker pool.
func (wp *workerPool) stopWorker(id xid.ID) error {
	value, ok := wp.workers.Load(id)
	if !ok {
		return fmt.Errorf("worker %s not found", id)
	}

	workerInfo, ok := value.(*workerInfo)
	if !ok {
		return fmt.Errorf("worker %s has invalid type", id)
	}

	workerInfo.stopOnce.Do(func() {
		close(workerInfo.stopChan)
	})
	return nil
}

// stopWorkers stops workers, which removes them from the pool.
// Note 1: if the number of workers to stop exceeds the number of idle workers the function will
// send stop signals to busy workers, which will stop after they finish their current task.
// Note 2: due to the timing of the stop signal, there is a chance that a worker marked as idle
// will pick up a task before the stop signal is received, in which case the worker will not stop
// until it finishes the task. This will not block this function.
// Note 3: this function is not thread-safe, it should be called from within a mutex lock.
func (wp *workerPool) stopWorkers(workersToStop int) error {
	// Validate number of workers to remove
	if workersToStop <= 0 {
		return fmt.Errorf("invalid number of workers to remove: %d", workersToStop)
	}
	if workersToStop > int(wp.runningWorkers()) {
		return fmt.Errorf("cannot remove %d out of %d running workers", workersToStop, wp.runningWorkers())
	}
	logger.Debug().Msgf("Removing %d workers from the pool", workersToStop)

	busyWorkers, idleWorkers := wp.busyAndIdleWorkers()

	// Stop a subset of the idle workers if there is an abundance
	var errs error
	if len(idleWorkers) >= workersToStop {
		workersToRemove := idleWorkers[:workersToStop]
		for _, workerID := range workersToRemove {
			err := wp.stopWorker(workerID)
			if err != nil {
				errs = errors.Join(errs, err)
				logger.Debug().Err(err).Msgf("Failed to stop worker %s", workerID)
			}
		}
		return errs
	}

	// If there aren't enough idle workers, first stop all idle workers
	for _, workerID := range idleWorkers {
		err := wp.stopWorker(workerID)
		if err != nil {
			errs = errors.Join(errs, err)
			logger.Debug().Err(err).Msgf("Failed to stop worker %s", workerID)
		}
	}

	// Then stop busy workers as well, up to the number of workers to remove
	remainingToStop := min(workersToStop-len(idleWorkers), len(busyWorkers))
	workersToRemove := busyWorkers[:remainingToStop]
	for _, workerID := range workersToRemove {
		err := wp.stopWorker(workerID)
		if err != nil {
			errs = errors.Join(errs, err)
			logger.Debug().Err(err).Msgf("Failed to stop worker %s", workerID)
		}
	}

	return errs
}

// utilization returns the utilization of the worker pool as a float between 0.0 and 1.0.
func (wp *workerPool) utilization() float64 {
	if wp.runningWorkers() == 0 {
		return 0.0
	}
	return float64(wp.activeWorkers()) / float64(wp.runningWorkers())
}

// workerCountScalingChannel returns a write-only channel for scaling the worker count.
func (wp *workerPool) workerCountScalingChannel() chan<- int32 {
	return wp.workerCountChan
}

// newWorkerPool creates and returns a new worker pool.
func newWorkerPool(
	initialWorkerCount int,
	errorChan chan error,
	execTimeChan chan time.Duration,
	taskChan chan Task,
	workerPoolDone chan struct{},
) *workerPool {
	pool := &workerPool{
		errorChan:       errorChan,
		execTimeChan:    execTimeChan,
		stopPoolChan:    make(chan struct{}),
		taskChan:        taskChan,
		workerCountChan: make(chan int32, 1), // Buffered channel to prevent blocking
		workerPoolDone:  workerPoolDone,
	}
	pool.addWorkers(initialWorkerCount)
	pool.workerCountTarget.Store(int32(initialWorkerCount))

	go pool.processWorkerCountScaling()

	return pool
}
