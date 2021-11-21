package metric

import (
	"bufio"
)

type Metrical interface {
	CollectMetrics(*bufio.Writer)
}
