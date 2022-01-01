package metric

import (
	"bufio"
	"fmt"
	"runtime"
	"time"
)

var startTime time.Time

func init() {
	startTime = time.Now()
}

func StartTime() time.Time {
	return startTime
}

type Runtime struct{}

var _ = Metrical(&Runtime{})

func (*Runtime) CollectMetrics(w *bufio.Writer) {
	_, _ = w.WriteString(fmt.Sprintf("Uptime: %v (since %v)\n", time.Since(startTime), startTime))
	_, _ = w.WriteString(fmt.Sprintln("Num CPU:", runtime.NumCPU()))
	_, _ = w.WriteString(fmt.Sprintln("Num Goroutines:", runtime.NumGoroutine()))
	var memstats runtime.MemStats
	runtime.ReadMemStats(&memstats)
	_, _ = w.WriteString(fmt.Sprintln("Heap Used:", memstats.Alloc))
	_, _ = w.WriteString(fmt.Sprintln("Heap Allocated:", memstats.Sys))
	_, _ = w.WriteString(fmt.Sprintln("Stack Used:", memstats.StackInuse))
	_, _ = w.WriteString(fmt.Sprintln("Stack Allocated:", memstats.StackSys))
}
