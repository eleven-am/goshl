package domain

import (
	"math"
	"time"
)

const (
	OutputTimelineOffset = 10 * time.Second
	JobContractVersion   = 5
	BoundaryTolerance    = 0.000001
	StreamTailSlack      = 0.5
)

func Adjacent(previous, next Segment) bool {
	return next.Index == previous.Index+1 && math.Abs(previous.End-next.Start) <= BoundaryTolerance
}
