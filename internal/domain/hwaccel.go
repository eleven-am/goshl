package domain

type Accelerator string

const (
	AccelNone         Accelerator = "none"
	AccelCUDA         Accelerator = "cuda"
	AccelVideoToolbox Accelerator = "videotoolbox"
	AccelVAAPI        Accelerator = "vaapi"
	AccelQSV          Accelerator = "qsv"
)

type HWAccelConfig struct {
	Accelerator  Accelerator
	Requested    Accelerator
	Available    []Accelerator
	Error        string
	DecodeFlags  []string
	EncodeFlags  []string
	Encoder      string
	KeyframeFlag string
	ScaleFilter  string
}
