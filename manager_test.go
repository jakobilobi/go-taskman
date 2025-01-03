package taskman

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
)

type MockTask struct {
	ID      string
	cadence time.Duration

	executeFunc func() error
}

func (mt MockTask) Execute() error {
	log.Debug().Msgf("Executing MockTask with ID: %s", mt.ID)
	if mt.executeFunc != nil {
		err := mt.executeFunc()
		if err != nil {
			return err
		}
	}
	return nil
}

// Helper function to determine the buffer size of a channel
func getChannelBufferSize(ch interface{}) int {
	switch v := ch.(type) {
	case chan Task:
		return cap(v)
	case chan error:
		return cap(v)
	default:
		return 0
	}
}

// Helper function to get a Job with mocked tasks.
func getMockedJob(nTasks int, jobID string, cadence, timeToNextExec time.Duration) Job {
	var mockTasks []MockTask
	for i := 0; i < nTasks; i++ {
		mockTasks = append(mockTasks, MockTask{ID: fmt.Sprintf("task-%d", i), cadence: cadence})
	}
	// Explicitly convert []MockTask to []Task to satisfy the Job struct
	tasks := make([]Task, len(mockTasks))
	for i, task := range mockTasks {
		tasks[i] = task
	}
	job := Job{
		Cadence:  cadence,
		ID:       jobID,
		NextExec: time.Now().Add(timeToNextExec),
		Tasks:    tasks,
	}
	return job
}

func TestMain(m *testing.M) {
	zerolog.TimeFieldFormat = time.RFC3339Nano
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05.999"}).Level(zerolog.DebugLevel)
	os.Exit(m.Run())
}

func TestNewManagerCustom(t *testing.T) {
	manager := NewManagerCustom(10, 1, 1)
	defer manager.Stop()

	// Verify jobQueue is initialized
	assert.NotNil(t, manager.jobQueue, "Expected job queue to be non-nil")

	// Verify taskChan is initialized and has the correct buffer size
	assert.NotNil(t, manager.taskChan, "Expected task channel to be non-nil")
	taskChanBuffer := getChannelBufferSize(manager.taskChan)
	assert.Equal(t, 1, taskChanBuffer, "Expected task channel to have buffer size 1")

	// Verify errorChan is initialized and has the correct buffer size
	assert.NotNil(t, manager.errorChan, "Expected error channel to be non-nil")
	errorChanBuffer := getChannelBufferSize(manager.errorChan)
	assert.Equal(t, 1, errorChanBuffer, "Expected error channel to have buffer size 1")
}

func TestManagerStop(t *testing.T) {
	// NewManagerCustom starts the manager
	manager := NewManagerCustom(10, 2, 1)

	// Immediately stop the manager
	manager.Stop()

	// Attempt to add a task after stopping
	testChan := make(chan bool)
	testTask := MockTask{ID: "a-task", cadence: 50 * time.Millisecond, executeFunc: func() error {
		testChan <- true
		return nil
	}}
	manager.ScheduleTask(testTask, testTask.cadence)

	// Since the manager is stopped, the task should not have been added to the job queue
	if manager.jobsInQueue() != 0 {
		t.Fatalf("Expected job queue length to be 0, got %d", manager.jobsInQueue())
	}

	// Wait some time to see if the task executes
	select {
	case <-testChan:
		t.Fatal("Did not expect any tasks to be executed after manager is stopped")
	case <-time.After(100 * time.Millisecond):
		// No tasks executed, as expected
	}
}

func TestScheduleFunc(t *testing.T) {
	manager := NewManagerCustom(10, 2, 1)
	defer manager.Stop()

	jobID, err := manager.ScheduleFunc(
		func() error {
			return nil
		},
		100*time.Millisecond,
	)
	if err != nil {
		t.Fatalf("Error adding function: %v", err)
	}

	assert.Equal(t, 1, manager.jobsInQueue(), "Expected job queue length to be 1, got %d", manager.jobsInQueue())

	job := manager.jobQueue[0]
	assert.Equal(t, 1, len(job.Tasks), "Expected job to have 1 task, got %d", len(job.Tasks))
	assert.Equal(t, jobID, job.ID, "Expected job ID to be %s, got %s", jobID, job.ID)
}

func TestScheduleTask(t *testing.T) {
	manager := NewManagerCustom(10, 2, 1)
	defer manager.Stop()

	testTask := MockTask{ID: "test-task", cadence: 100 * time.Millisecond}
	jobID, err := manager.ScheduleTask(testTask, testTask.cadence)
	if err != nil {
		t.Fatalf("Error adding task: %v", err)
	}

	assert.Equal(t, 1, manager.jobsInQueue(), "Expected job queue length to be 1, got %d", manager.jobsInQueue())

	job := manager.jobQueue[0]
	assert.Equal(t, 1, len(job.Tasks), "Expected job to have 1 task, got %d", len(job.Tasks))
	assert.Equal(t, testTask, job.Tasks[0], "Expected the task in the job to be the test task")
	assert.Equal(t, jobID, job.ID, "Expected job ID to be %s, got %s", jobID, job.ID)
}

func TestScheduleTasks(t *testing.T) {
	manager := NewManagerCustom(10, 2, 1)
	defer manager.Stop()

	mockTasks := []MockTask{
		{ID: "task1", cadence: 100 * time.Millisecond},
		{ID: "task2", cadence: 100 * time.Millisecond},
	}
	// Explicitly convert []MockTask to []Task to satisfy the ScheduleTasks method signature, since slices are not covariant in Go
	tasks := make([]Task, len(mockTasks))
	for i, task := range mockTasks {
		tasks[i] = task
	}
	jobID, err := manager.ScheduleTasks(tasks, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Error adding tasks: %v", err)
	}

	// Assert that the job was added
	assert.Equal(t, 1, manager.jobsInQueue(), "Expected job queue length to be 1, got %d", manager.jobsInQueue())
	job := manager.jobQueue[0]
	assert.Equal(t, 2, len(job.Tasks), "Expected job to have 2 tasks, got %d", len(job.Tasks))
	assert.Equal(t, jobID, job.ID, "Expected job ID to be %s, got %s", jobID, job.ID)
}

func TestScheduleJob(t *testing.T) {
	manager := NewManagerCustom(10, 2, 1)
	defer manager.Stop()

	job := getMockedJob(2, "test-job", 100*time.Millisecond, 100*time.Millisecond)
	err := manager.ScheduleJob(job)
	if err != nil {
		t.Fatalf("Error adding job: %v", err)
	}

	// Assert that the job was added
	assert.Equal(t, 1, manager.jobsInQueue(), "Expected job queue length to be 1, got %d", manager.jobsInQueue())
	scheduledJob := manager.jobQueue[0]
	assert.Equal(t, len(job.Tasks), len(scheduledJob.Tasks), "Expected job to have 2 tasks, got %d", len(job.Tasks))
	assert.Equal(t, job.ID, scheduledJob.ID, "Expected job ID to be %s, got %s", scheduledJob.ID, job.ID)
}

func TestRemoveJob(t *testing.T) {
	manager := NewManagerCustom(10, 2, 1)
	defer manager.Stop()

	job := getMockedJob(2, "someJob", 100*time.Millisecond, 100*time.Millisecond)
	err := manager.ScheduleJob(job)
	assert.Nil(t, err, "Error adding job")

	// Assert that the job was added
	assert.Equal(t, 1, manager.jobsInQueue(), "Expected job queue length to be 1, got %d", manager.jobsInQueue())
	qJob := manager.jobQueue[0]
	assert.Equal(t, job.ID, qJob.ID, "Expected job ID to be %s, got %s", job.ID, qJob.ID)
	assert.Equal(t, 2, len(qJob.Tasks), "Expected job to have 2 tasks, got %d", len(qJob.Tasks))

	// Remove the job
	err = manager.RemoveJob(job.ID)
	assert.Nil(t, err, "Error removing job")

	// Assert that the job was removed
	assert.Equal(t, 0, manager.jobsInQueue(), "Expected job queue length to be 0, got %d", manager.jobsInQueue())

	// Try removing the job once more
	err = manager.RemoveJob(job.ID)
	assert.Equal(t, ErrJobNotFound, err, "Expected removal of non-existent job to produce ErrJobNotFound")
}

func TestReplaceJob(t *testing.T) {
	manager := NewManagerCustom(4, 4, 4)
	defer manager.Stop()

	// Add a job
	firstJob := getMockedJob(2, "aJobID", 100*time.Millisecond, 100*time.Millisecond)
	err := manager.ScheduleJob(firstJob)
	assert.Nil(t, err, "Error adding job")
	// Assert job added
	assert.Equal(t, 1, manager.jobsInQueue(), "Expected job queue length to be 1, got %d", manager.jobsInQueue())
	qJob := manager.jobQueue[0]
	assert.Equal(t, firstJob.ID, qJob.ID, "Expected ID to be '%s', got '%s'", firstJob.ID, qJob.ID)

	// Replace the first job
	secondJob := getMockedJob(4, "aJobID", 50*time.Millisecond, 100*time.Millisecond)
	err = manager.ReplaceJob(secondJob)
	assert.Nil(t, err, "Error replacing job")
	// Assert that the job was replaced in the queue
	assert.Equal(t, 1, manager.jobsInQueue(), "Expected job queue length to be 1, got %d", manager.jobsInQueue())
	qJob = manager.jobQueue[0]
	// The queue job should retain the index and NextExec time of the first job
	assert.Equal(t, firstJob.index, qJob.index, "Expected index to be '%s', got '%s'", secondJob.index, qJob.index)
	assert.Equal(t, firstJob.NextExec, qJob.NextExec, "Expected ID to be '%s', got '%s'", secondJob.NextExec, qJob.NextExec)
	// The queue job should have the ID, cadence and tasks of the new (second) job
	assert.Equal(t, secondJob.ID, qJob.ID, "Expected ID to be '%s', got '%s'", secondJob.ID, qJob.ID)
	assert.Equal(t, secondJob.Cadence, qJob.Cadence, "Expected cadence to be '%s', got '%s'", secondJob.Cadence, qJob.Cadence)
	assert.Equal(t, len(secondJob.Tasks), len(qJob.Tasks), "Expected job to have %d tasks, got %d", len(secondJob.Tasks), len(qJob.Tasks))

	// Try to replace a non-existing job
	thirdJob := getMockedJob(2, "anotherJobID", 10*time.Millisecond, 100*time.Millisecond)
	err = manager.ReplaceJob(thirdJob)
	assert.Equal(t, ErrJobNotFound, err, "Expected replace attempt of non-existent job to produce ErrJobNotFound")
}

func TestTaskExecution(t *testing.T) {
	manager := NewManagerCustom(10, 1, 1)
	defer manager.Stop()

	var wg sync.WaitGroup
	wg.Add(1)

	// Use a channel to receive execution times
	executionTimes := make(chan time.Time, 1)

	// Create a test task
	testTask := &MockTask{
		ID:      "test-execution-task",
		cadence: 100 * time.Millisecond,
		executeFunc: func() error {
			log.Debug().Msg("Executing TestTaskExecution task")
			executionTimes <- time.Now()
			wg.Done()
			return nil
		},
	}

	manager.ScheduleTask(testTask, testTask.cadence)

	select {
	case execTime := <-executionTimes:
		elapsed := time.Since(execTime)
		assert.Greater(t, 150*time.Millisecond, elapsed, "Task executed after %v, expected around 100ms", elapsed)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Task did not execute in expected time")
	}
}

func TestTaskRescheduling(t *testing.T) {
	// Make room in buffered channel for multiple errors (4), since we're not consuming them in this test
	// and the error channel otherwise blocks the workers from executing tasks
	manager := NewManagerCustom(10, 1, 4)
	defer manager.Stop()

	var executionTimes []time.Time
	var mu sync.Mutex

	// Create a test task that records execution times
	mockTask := &MockTask{
		ID:      "test-rescheduling-task",
		cadence: 100 * time.Millisecond,
		executeFunc: func() error {
			mu.Lock()
			executionTimes = append(executionTimes, time.Now())
			mu.Unlock()
			return nil
		},
	}

	manager.ScheduleTask(mockTask, mockTask.cadence)

	// Wait for the task to execute multiple times
	// Sleeing for 350ms should allow for about 3 executions
	time.Sleep(350 * time.Millisecond)

	mu.Lock()
	execCount := len(executionTimes)
	mu.Unlock()

	assert.LessOrEqual(t, 3, execCount, "Expected at least 3 executions, got %d", execCount)

	// Check that the executions occurred at roughly the correct intervals
	mu.Lock()
	for i := 1; i < len(executionTimes); i++ {
		diff := executionTimes[i].Sub(executionTimes[i-1])
		if diff < 90*time.Millisecond || diff > 110*time.Millisecond {
			t.Fatalf("Execution interval out of expected range: %v", diff)
		}
	}
	mu.Unlock()
}

func TestScheduleTaskDuringExecution(t *testing.T) {
	manager := NewManagerCustom(10, 1, 1)
	defer manager.Stop()

	// Dedicated channels for task execution signals
	task1Executed := make(chan struct{})
	defer close(task1Executed)

	// Use a context to cancel the test
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackTasks := map[string]bool{
		"task1": false,
		"task2": false,
	}
	var mu sync.Mutex

	// Create test tasks
	testTask1 := &MockTask{
		ID:      "test-add-mid-execution-task-1",
		cadence: 15 * time.Millisecond,
		executeFunc: func() error {
			select {
			case <-ctx.Done():
				return nil
			default:
				task1Executed <- struct{}{}
				mu.Lock()
				trackTasks["task1"] = true
				mu.Unlock()
			}
			return nil
		},
	}
	testTask2 := &MockTask{
		ID:      "test-add-mid-execution-task-2",
		cadence: 10 * time.Millisecond,
		executeFunc: func() error {
			select {
			case <-ctx.Done():
				return nil
			default:
				mu.Lock()
				trackTasks["task2"] = true
				mu.Unlock()
			}
			return nil
		},
	}

	// Schedule first task
	manager.ScheduleTask(testTask1, testTask1.cadence)
	start := time.Now()

	select {
	case <-task1Executed:
		// Task executed as expected
	case <-time.After(20 * time.Millisecond):
		t.Fatalf("Task 1 did not execute within the expected time frame (40ms), %v", time.Since(start))
	}

	// Consume task1Executed to prevent it from blocking
	go func() {
		for range task1Executed {
			// Do nothing
		}
	}()

	// Schedule second task after we've had at least one execution of the first task
	manager.ScheduleTask(testTask2, testTask2.cadence)

	// Sleep enough time to make sure both tasks have executed at least once
	time.Sleep(30 * time.Millisecond)

	cancel()
	manager.Stop()

	// Check that both tasks have executed
	mu.Lock()
	for msg, executed := range trackTasks {
		if !executed {
			t.Fatalf("Task '%s' did not execute, %v", msg, time.Since(start))
		}
	}
	mu.Unlock()
}

func TestConcurrentScheduleTask(t *testing.T) {
	// Deactivate debug logs for this test
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05.999"}).Level(zerolog.InfoLevel)

	manager := NewManagerCustom(10, 1, 1)
	defer manager.Stop()

	var wg sync.WaitGroup
	numGoroutines := 20
	numTasksPerGoroutine := 250

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < numTasksPerGoroutine; j++ {
				taskID := fmt.Sprintf("task-%d-%d", id, j)
				// Use a long cadence to avoid task execution before test ends, as this changes the queue length
				task := MockTask{ID: taskID, cadence: 2 * time.Second}
				_, err := manager.ScheduleTask(task, task.cadence)
				assert.NoError(t, err, "Error adding task concurrently")
			}
		}(i)
	}

	wg.Wait()

	// Verify that all tasks are scheduled
	expectedTasks := numGoroutines * numTasksPerGoroutine
	assert.Equal(t, expectedTasks, manager.jobsInQueue(), "Expected job queue length to be %d, got %d", expectedTasks, manager.jobsInQueue())
}

func TestConcurrentScheduleJob(t *testing.T) {
	// Deactivate debug logs for this test
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05.999"}).Level(zerolog.InfoLevel)

	manager := NewManagerCustom(10, 1, 1)
	defer manager.Stop()

	var wg sync.WaitGroup
	numGoroutines := 20
	numTasksPerGoroutine := 250

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < numTasksPerGoroutine; j++ {
				jobID := fmt.Sprintf("task-%d-%d", id, j)
				// Use a long cadence to avoid job execution before test ends, as this changes the queue length
				job := getMockedJob(1, jobID, 100*time.Millisecond, 2*time.Second)
				err := manager.ScheduleJob(job)
				assert.NoError(t, err, "Error adding job concurrently")
			}
		}(i)
	}

	wg.Wait()

	// Verify that all tasks are scheduled
	expectedTasks := numGoroutines * numTasksPerGoroutine
	assert.Equal(t, expectedTasks, manager.jobsInQueue(), "Expected job queue length to be %d, got %d", expectedTasks, manager.jobsInQueue())
}

func TestZeroCadenceTask(t *testing.T) {
	manager := NewManagerCustom(10, 1, 1)
	defer manager.Stop()

	testChan := make(chan bool)
	testTask := MockTask{ID: "zero-cadence-task", cadence: 0, executeFunc: func() error {
		testChan <- true
		return nil
	}}
	manager.ScheduleTask(testTask, testTask.cadence)

	// Expect the task to not execute
	select {
	case <-testChan:
		// Task executed, which is unexpected
		t.Fatal("Task with zero cadence should not execute")
	case <-time.After(50 * time.Millisecond):
		// After 50ms, the task would have executed if it was scheduled
		log.Debug().Msg("Task with zero cadence never executed")
	}
}

func TestValidateJob(t *testing.T) {
	manager := NewManagerCustom(10, 1, 1)
	defer manager.Stop()

	// Test case: valid job
	validJob := Job{
		ID:       "valid-job",
		Cadence:  100 * time.Millisecond,
		NextExec: time.Now().Add(100 * time.Millisecond),
		Tasks:    []Task{MockTask{ID: "task1"}},
	}
	err := manager.validateJob(validJob)
	assert.NoError(t, err, "Expected no error for valid job")

	// Test case: invalid job with zero cadence
	invalidJobZeroCadence := Job{
		ID:       "invalid-job-zero-cadence",
		Cadence:  0,
		NextExec: time.Now().Add(100 * time.Millisecond),
		Tasks:    []Task{MockTask{ID: "task1"}},
	}
	err = manager.validateJob(invalidJobZeroCadence)
	assert.ErrorIs(t, err, ErrInvalidCadence, "Expected ErrInvalidCadence for job with zero cadence")

	// Test case: invalid job with no tasks
	invalidJobNoTasks := Job{
		ID:       "invalid-job-no-tasks",
		Cadence:  100 * time.Millisecond,
		NextExec: time.Now().Add(100 * time.Millisecond),
		Tasks:    []Task{},
	}
	err = manager.validateJob(invalidJobNoTasks)
	assert.ErrorIs(t, err, ErrNoTasks, "Expected ErrNoTasks for job with no tasks")

	// Test case: invalid job with zero NextExec time
	invalidJobZeroNextExec := Job{
		ID:       "invalid-job-zero-next-exec",
		Cadence:  100 * time.Millisecond,
		NextExec: time.Time{},
		Tasks:    []Task{MockTask{ID: "task1"}},
	}
	err = manager.validateJob(invalidJobZeroNextExec)
	assert.ErrorIs(t, err, ErrZeroNextExec, "Expected ErrZeroNextExec for job with zero NextExec time")

	// Test case: existing job ID
	alreadyPresentJob := Job{
		ID:       "job-in-queue",
		Cadence:  100 * time.Millisecond,
		NextExec: time.Now().Add(100 * time.Millisecond),
		Tasks:    []Task{MockTask{ID: "task1"}},
	}
	manager.ScheduleJob(alreadyPresentJob)
	duplicateJob := alreadyPresentJob
	err = manager.validateJob(duplicateJob)
	assert.ErrorIs(t, err, ErrDuplicateJobID, "Expected to find duplicate job ID")
}

func TestErrorChannelConsumption(t *testing.T) {
	manager := NewManagerCustom(10, 1, 1)
	defer manager.Stop()

	// Simulate errors being sent to the error channel
	go func() {
		manager.errorChan <- errors.New("internal error 1")
		manager.errorChan <- errors.New("internal error 2")
		time.Sleep(5 * time.Millisecond) // Allow some time for errors to propagate
	}()

	// Verify internal consumption by ensuring no errors remain after consumption
	time.Sleep(15 * time.Millisecond) // Allow internal consumer to log errors

	// Attempt to call ErrorChannel and transition ownership to the caller
	errCh, err := manager.ErrorChannel()
	assert.NoError(t, err, "Expected no error when calling ErrorChannel")
	assert.NotNil(t, errCh, "ErrorChannel should return a valid channel")

	// Send additional errors after ownership transition
	go func() {
		manager.errorChan <- errors.New("external error 1")
		manager.errorChan <- errors.New("external error 2")
	}()

	// Consume errors from the external ErrorChannel
	receivedErrors := []string{}
	timeout := time.After(50 * time.Millisecond)

Loop:
	for {
		select {
		case err, ok := <-errCh:
			if !ok {
				break Loop // Channel closed
			}
			receivedErrors = append(receivedErrors, err.Error())
		case <-timeout:
			break Loop // Avoid infinite loop in case of test failure
		}
	}

	// Validate that the correct errors were received after transition
	assert.Contains(t, receivedErrors, "external error 1")
	assert.Contains(t, receivedErrors, "external error 2")
	assert.NotContains(t, receivedErrors, "internal error 1", "Errors logged internally should not appear")
	assert.NotContains(t, receivedErrors, "internal error 2", "Errors logged internally should not appear")
}
