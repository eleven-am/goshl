package h264level

import "math"

const (
	Floor   = 30
	Ceiling = 52

	deviceFrameRate = 30
	unknownRate     = 60
)

type limits struct {
	level                   int
	maxFrameMacroblocks     int
	maxMacroblocksPerSecond int
	maxHighProfileBitrate   int
	maxHighProfileCPB       int
}

var supported = []limits{
	{level: 30, maxFrameMacroblocks: 1620, maxMacroblocksPerSecond: 40500, maxHighProfileBitrate: 12_500_000, maxHighProfileCPB: 12_500_000},
	{level: 31, maxFrameMacroblocks: 3600, maxMacroblocksPerSecond: 108000, maxHighProfileBitrate: 17_500_000, maxHighProfileCPB: 17_500_000},
	{level: 32, maxFrameMacroblocks: 5120, maxMacroblocksPerSecond: 216000, maxHighProfileBitrate: 25_000_000, maxHighProfileCPB: 25_000_000},
	{level: 40, maxFrameMacroblocks: 8192, maxMacroblocksPerSecond: 245760, maxHighProfileBitrate: 25_000_000, maxHighProfileCPB: 31_250_000},
	{level: 41, maxFrameMacroblocks: 8192, maxMacroblocksPerSecond: 245760, maxHighProfileBitrate: 62_500_000, maxHighProfileCPB: 78_125_000},
	{level: 42, maxFrameMacroblocks: 8704, maxMacroblocksPerSecond: 522240, maxHighProfileBitrate: 62_500_000, maxHighProfileCPB: 78_125_000},
	{level: 50, maxFrameMacroblocks: 22080, maxMacroblocksPerSecond: 589824, maxHighProfileBitrate: 168_750_000, maxHighProfileCPB: 168_750_000},
	{level: 51, maxFrameMacroblocks: 36864, maxMacroblocksPerSecond: 983040, maxHighProfileBitrate: 300_000_000, maxHighProfileCPB: 300_000_000},
	{level: 52, maxFrameMacroblocks: 36864, maxMacroblocksPerSecond: 2073600, maxHighProfileBitrate: 300_000_000, maxHighProfileCPB: 300_000_000},
}

func Minimum(width, height int, frameRate float64, peakBitrate int) (int, bool) {
	macroblockWidth := divideRoundUp(width, 16)
	macroblockHeight := divideRoundUp(height, 16)
	frameMacroblocks := macroblockWidth * macroblockHeight
	if frameRate <= 0 || math.IsNaN(frameRate) || math.IsInf(frameRate, 0) {
		frameRate = unknownRate
	}
	macroblocksPerSecond := float64(frameMacroblocks) * math.Max(frameRate, deviceFrameRate)

	for _, candidate := range supported {
		maxDimension := int(math.Sqrt(float64(candidate.maxFrameMacroblocks * 8)))
		if frameMacroblocks > candidate.maxFrameMacroblocks || macroblockWidth > maxDimension || macroblockHeight > maxDimension {
			continue
		}
		if macroblocksPerSecond > float64(candidate.maxMacroblocksPerSecond) {
			continue
		}
		if peakBitrate > candidate.maxHighProfileBitrate {
			continue
		}
		return candidate.level, true
	}
	return 0, false
}

func MaxCPB(level int) int {
	for _, candidate := range supported {
		if candidate.level == level {
			return candidate.maxHighProfileCPB
		}
	}
	return 0
}

func divideRoundUp(value, divisor int) int {
	if value <= 0 {
		return 0
	}
	return (value + divisor - 1) / divisor
}
