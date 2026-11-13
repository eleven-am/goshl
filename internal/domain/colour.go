package domain

const (
	TransferPQ  = "smpte2084"
	TransferHLG = "arib-std-b67"
)

type ColourPlan struct {
	Tonemap         bool
	Retag           bool
	AssumeMatrix    string
	AssumePrimaries string
}

func HDRTransfer(transfer string) bool {
	return transfer == TransferPQ || transfer == TransferHLG
}

func MistaggedHDR(video VideoStream) bool {
	return HDRTransfer(video.ColorTransfer) && sdrPrimaries(video.ColorPrimaries)
}

func sdrPrimaries(primaries string) bool {
	switch primaries {
	case "bt709", "smpte170m", "bt470bg", "bt470m":
		return true
	}
	return false
}

func PlanColour(video VideoStream) ColourPlan {
	if MistaggedHDR(video) {
		return ColourPlan{Retag: true}
	}
	plan := ColourPlan{Tonemap: HDRTransfer(video.ColorTransfer)}
	if plan.Tonemap {
		if untagged(video.ColorSpace) {
			plan.AssumeMatrix = "bt2020nc"
		}
		if untagged(video.ColorPrimaries) {
			plan.AssumePrimaries = "bt2020"
		}
		return plan
	}
	if untagged(video.ColorSpace) {
		plan.AssumeMatrix = "smpte170m"
		if video.Width >= 1280 || video.Height > 576 {
			plan.AssumeMatrix = "bt709"
		}
	}
	return plan
}

func untagged(value string) bool {
	return value == "" || value == "unknown" || value == "reserved"
}
