package taskman

import (
	"math"
	"sync/atomic"
	"time"

	uatomic "go.uber.org/atomic"
)

// managerMetrics stores metrics about the task manager.
type managerMetrics struct {
	averageExecTime     uatomic.Duration // Average execution time of tasks
	totalTaskExecutions atomic.Int64     // Total number of tasks executed
	tasksPerSecond      uatomic.Float32  // Number of tasks executed per second
	tasksInQueue        atomic.Int64     // Total number of tasks in the queue
	maxJobWidth         atomic.Int32     // Widest job in the queue in terms of number of tasks

	done <-chan struct{}
}

// consumeExecTime consumes execution times and calculates the average execution time of tasks.
func (mm *managerMetrics) consumeExecTime(execTimeChan <-chan time.Duration) {
	for {
		select {
		case execTime := <-execTimeChan:
			avgExecTime := mm.averageExecTime.Load()
			taskExecutions := mm.totalTaskExecutions.Load()

			// Calculate the new average execution time
			newAvgExecTime := (avgExecTime*time.Duration(taskExecutions) + execTime) / time.Duration(taskExecutions+1)

			// Store the updated metrics
			mm.averageExecTime.Store(newAvgExecTime)
			mm.totalTaskExecutions.Add(1)
		case <-mm.done:
			// Only stop consuming once done is received
			return
		}
	}
}

// updateTaskMetrics updates the task metrics. The input taskDelta is the number of tasks added or
// removed, and tasksPerSecond is the number of tasks executed per second by those tasks.
func (mm *managerMetrics) updateTaskMetrics(taskDelta int, taskCadence time.Duration) {
	// Calculate the new number of tasks in the queue
	currentTaskCount := mm.tasksInQueue.Load()
	newTaskCount := currentTaskCount + int64(taskDelta)

	// Avoid division by zero
	if newTaskCount <= 0 {
		mm.tasksPerSecond.Store(0)
		mm.tasksInQueue.Store(0)
		return
	}

	if int32(taskDelta) > mm.maxJobWidth.Load() {
		mm.maxJobWidth.Store(int32(taskDelta))
	}

	// Calculate the new tasks per second
	tasksPerSecond := calcTasksPerSecond(taskDelta, taskCadence)

	// Update the tasks per second metric base on a weighted average
	newTasksPerSecond := (tasksPerSecond*float32(taskDelta) + mm.tasksPerSecond.Load()*float32(currentTaskCount)) / float32(newTaskCount)

	// Store updated values
	mm.tasksPerSecond.Store(newTasksPerSecond)
	mm.tasksInQueue.Store(newTaskCount)

	// TODO: consider keeping
	//log.Debug().Msgf("Task metrics updated: %d running tasks, %f tasks/s", newTaskCount, newTasksPerSecond)
}

// calcTasksPerSecond calculates the number of tasks executed per second.
func calcTasksPerSecond(nTasks int, cadence time.Duration) float32 {
	if cadence == 0 {
		return 0
	}
	tasks := math.Abs(float64(nTasks))
	return float32(tasks) / float32(cadence.Seconds())
}
