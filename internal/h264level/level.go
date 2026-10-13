package h264level

import "math"

type limits struct {
	level                    int
	maxFrameMacroblocks      int
	maxMacroblocksPerSecond  int
	maxHighProfileBitrateBps int
}

var supported = []limits{
	{level: 21, maxFrameMacroblocks: 792, maxMacroblocksPerSecond: 19800, maxHighProfileBitrateBps: 5_000_000},
	{level: 22, maxFrameMacroblocks: 1620, maxMacroblocksPerSecond: 20250, maxHighProfileBitrateBps: 5_000_000},
	{level: 30, maxFrameMacroblocks: 1620, maxMacroblocksPerSecond: 40500, maxHighProfileBitrateBps: 12_500_000},
	{level: 31, maxFrameMacroblocks: 3600, maxMacroblocksPerSecond: 108000, maxHighProfileBitrateBps: 17_500_000},
	{level: 32, maxFrameMacroblocks: 5120, maxMacroblocksPerSecond: 216000, maxHighProfileBitrateBps: 25_000_000},
	{level: 40, maxFrameMacroblocks: 8192, maxMacroblocksPerSecond: 245760, maxHighProfileBitrateBps: 25_000_000},
	{level: 41, maxFrameMacroblocks: 8192, maxMacroblocksPerSecond: 245760, maxHighProfileBitrateBps: 62_500_000},
	{level: 42, maxFrameMacroblocks: 8704, maxMacroblocksPerSecond: 522240, maxHighProfileBitrateBps: 62_500_000},
	{level: 50, maxFrameMacroblocks: 22080, maxMacroblocksPerSecond: 589824, maxHighProfileBitrateBps: 168_750_000},
	{level: 51, maxFrameMacroblocks: 36864, maxMacroblocksPerSecond: 983040, maxHighProfileBitrateBps: 300_000_000},
	{level: 52, maxFrameMacroblocks: 36864, maxMacroblocksPerSecond: 2073600, maxHighProfileBitrateBps: 300_000_000},
	{level: 60, maxFrameMacroblocks: 139264, maxMacroblocksPerSecond: 4177920, maxHighProfileBitrateBps: 300_000_000},
	{level: 61, maxFrameMacroblocks: 139264, maxMacroblocksPerSecond: 8355840, maxHighProfileBitrateBps: 600_000_000},
	{level: 62, maxFrameMacroblocks: 139264, maxMacroblocksPerSecond: 16711680, maxHighProfileBitrateBps: 1_000_000_000},
}

func Minimum(width, height int, frameRate float64, peakBitrate int) int {
	macroblockWidth := divideRoundUp(width, 16)
	macroblockHeight := divideRoundUp(height, 16)
	frameMacroblocks := macroblockWidth * macroblockHeight
	if frameRate <= 0 || math.IsNaN(frameRate) || math.IsInf(frameRate, 0) {
		frameRate = 60
	}
	macroblocksPerSecond := float64(frameMacroblocks) * frameRate

	for _, candidate := range supported {
		maxDimension := int(math.Sqrt(float64(candidate.maxFrameMacroblocks * 8)))
		if frameMacroblocks > candidate.maxFrameMacroblocks || macroblockWidth > maxDimension || macroblockHeight > maxDimension {
			continue
		}
		if macroblocksPerSecond > float64(candidate.maxMacroblocksPerSecond) {
			continue
		}
		if peakBitrate > 0 && peakBitrate > candidate.maxHighProfileBitrateBps {
			continue
		}
		return candidate.level
	}
	return supported[len(supported)-1].level
}

func divideRoundUp(value, divisor int) int {
	if value <= 0 {
		return 0
	}
	return (value + divisor - 1) / divisor
}
