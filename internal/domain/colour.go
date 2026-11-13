package domain

const (
	TransferPQ  = "smpte2084"
	TransferHLG = "arib-std-b67"
)

type ColourPlan struct {
	Tonemap         bool
	AssumeMatrix    string
	AssumePrimaries string
}

func HDRTransfer(transfer string) bool {
	return transfer == TransferPQ || transfer == TransferHLG
}

func PlanColour(video VideoStream) ColourPlan {
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
