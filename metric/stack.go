package metric

import (
	"bufio"
	"runtime"
)

const stackMaxBytes = 262144

type Stack struct{}

var _ = Metrical(&Stack{})

func (*Stack) CollectMetrics(w *bufio.Writer) {
	var stack [stackMaxBytes]byte
	n := runtime.Stack(stack[:], true)
	_, _ = w.Write(stack[:n])
}
