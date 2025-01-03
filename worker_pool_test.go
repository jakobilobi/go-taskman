package taskman

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNewWorkerPool(t *testing.T) {
	errorChan := make(chan error, 1)
	taskChan := make(chan Task, 1)
	workerPoolDone := make(chan struct{})
	pool := newWorkerPool(10, errorChan, taskChan, workerPoolDone)
	defer pool.stop()

	// Verify stopChan initialization
	assert.NotNil(t, pool.stopChan, "Expected stop channel to be non-nil")
}

func TestWorkerPoolStartStop(t *testing.T) {
	errorChan := make(chan error, 1)
	taskChan := make(chan Task, 1)
	workerPoolDone := make(chan struct{})
	pool := newWorkerPool(4, errorChan, taskChan, workerPoolDone)
	defer func() {
		pool.stop()

		// Verify worker counts post-stop
		assert.Equal(t, 4, pool.workersTotal, "Expected worker count to be 4")
		assert.Equal(t, int32(0), pool.activeWorkers(), "Expected no active workers")
		assert.Equal(t, int32(0), pool.runningWorkers(), "Expected no running workers")
	}()

	// Verify worker counts pre-start
	assert.Equal(t, 4, pool.workersTotal, "Expected worker count to be 4")
	assert.Equal(t, int32(0), pool.activeWorkers(), "Expected no active workers")
	assert.Equal(t, int32(0), pool.runningWorkers(), "Expected no running workers")

	pool.start()
	time.Sleep(20 * time.Millisecond) // Wait for workers to start

	// Verify worker counts post-start
	assert.Equal(t, 4, pool.workersTotal, "Expected worker count to be 4")
	assert.Equal(t, int32(0), pool.activeWorkers(), "Expected no active workers")
	assert.Equal(t, int32(4), pool.runningWorkers(), "Expected 4 running workers")
}

func TestWorkerPoolTaskExecution(t *testing.T) {
	errorChan := make(chan error, 1)
	taskChan := make(chan Task, 1)
	workerPoolDone := make(chan struct{})
	pool := newWorkerPool(1, errorChan, taskChan, workerPoolDone)
	defer pool.stop()

	// Start the worker
	pool.start()
	time.Sleep(10 * time.Millisecond) // Wait for worker to start

	// Create a task
	task := &MockTask{
		executeFunc: func() error {
			time.Sleep(30 * time.Millisecond)
			return nil
		},
		ID: "test-task",
	}

	// Listen to the error channel, confirm no error is received
	timeout := time.After(100 * time.Millisecond) // Timeout to close goroutine
	go func() {
		select {
		case err := <-errorChan:
			assert.Failf(t, "No error should have been received", err.Error())
		case <-timeout:
			return
		}

	}()

	// Send the task to the worker and verify active workers during task execution
	taskChan <- task
	time.Sleep(5 * time.Millisecond) // Wait for worker to pick up task
	assert.Equal(t, int32(1), pool.activeWorkers(), "Expected 1 active worker")

	// Verify workers after task execution
	time.Sleep(30 * time.Millisecond) // Wait for worker to execute task
	assert.Equal(t, int32(0), pool.activeWorkers(), "Expected 0 active workers")
}

func TestWorkerPoolExecutionError(t *testing.T) {
	errorChan := make(chan error, 1)
	taskChan := make(chan Task, 1)
	workerPoolDone := make(chan struct{})
	pool := newWorkerPool(1, errorChan, taskChan, workerPoolDone)
	defer pool.stop()

	// Start the worker
	pool.start()
	time.Sleep(10 * time.Millisecond) // Wait for worker to start

	// Create a task which produces an error
	errorTask := &MockTask{
		executeFunc: func() error {
			return errors.New("test error")
		},
		ID: "error-task",
	}

	var wg sync.WaitGroup
	wg.Add(1)

	// Listen to the error channel, confirm error is received
	timeout := time.After(100 * time.Millisecond)
	go func() {
		defer wg.Done()
		select {
		case err := <-errorChan:
			assert.Contains(t, err.Error(), "test error")
		case <-timeout:
			assert.Fail(t, "Test timed out waiting on error")
		}

	}()

	// Send the error-returning task to the worker
	taskChan <- errorTask
	wg.Wait() // Don't exit the test until the error has been received
}

func TestWorkerPoolBusyWorkers(t *testing.T) {
	errorChan := make(chan error, 1)
	taskChan := make(chan Task, 1)
	workerPoolDone := make(chan struct{})
	pool := newWorkerPool(2, errorChan, taskChan, workerPoolDone)
	defer pool.stop()

	// Start the workers
	pool.start()
	time.Sleep(10 * time.Millisecond) // Wait for workers to start

	// Create tasks that will keep workers busy
	task1 := &MockTask{
		executeFunc: func() error {
			time.Sleep(50 * time.Millisecond)
			return nil
		},
		ID: "task-1",
	}
	task2 := &MockTask{
		executeFunc: func() error {
			time.Sleep(50 * time.Millisecond)
			return nil
		},
		ID: "task-2",
	}

	// Send tasks to the workers
	taskChan <- task1
	taskChan <- task2
	time.Sleep(5 * time.Millisecond) // Wait for workers to pick up tasks

	// Verify active workers during task execution
	assert.Equal(t, int32(2), pool.activeWorkers(), "Expected 2 active workers")

	// Create another task to be queued
	task3 := &MockTask{
		executeFunc: func() error {
			time.Sleep(50 * time.Millisecond)
			return nil
		},
		ID: "task-3",
	}

	// Send the third task while workers are busy
	taskChan <- task3
	time.Sleep(5 * time.Millisecond) // Allow some time for task to be queued

	// Verify that the third task is queued and not yet executed
	assert.Equal(t, int32(2), pool.activeWorkers(), "Expected 2 active workers")
	assert.Equal(t, 1, len(taskChan), "Expected 1 task in the queue")

	// Wait for the first two tasks to complete
	time.Sleep(50 * time.Millisecond)

	// Verify that the third task is now being executed
	assert.Equal(t, int32(1), pool.activeWorkers(), "Expected 1 active worker")
	assert.Equal(t, 0, len(taskChan), "Expected no tasks in the queue")
}
