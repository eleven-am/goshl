package domain

import (
	"math"
	"time"
)

const (
	OutputTimelineOffset = 10 * time.Second
	JobContractVersion   = 4
	BoundaryTolerance    = 0.000001
)

func Adjacent(previous, next Segment) bool {
	return next.Index == previous.Index+1 && math.Abs(previous.End-next.Start) <= BoundaryTolerance
}
