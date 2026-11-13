package subtitle

type Kind int

const (
	KindUnknown Kind = iota
	KindText
	KindBitmap
)

var codecKinds = map[string]Kind{
	"subrip":            KindText,
	"srt":               KindText,
	"ass":               KindText,
	"ssa":               KindText,
	"mov_text":          KindText,
	"tx3g":              KindText,
	"webvtt":            KindText,
	"wvtt":              KindText,
	"text":              KindText,
	"microdvd":          KindText,
	"mpl2":              KindText,
	"pjs":               KindText,
	"realtext":          KindText,
	"sami":              KindText,
	"stl":               KindText,
	"subviewer":         KindText,
	"subviewer1":        KindText,
	"vplayer":           KindText,
	"jacosub":           KindText,
	"eia_608":           KindText,
	"hdmv_pgs_subtitle": KindBitmap,
	"pgssub":            KindBitmap,
	"dvd_subtitle":      KindBitmap,
	"dvdsub":            KindBitmap,
	"dvb_subtitle":      KindBitmap,
	"dvbsub":            KindBitmap,
	"dvb_teletext":      KindBitmap,
	"xsub":              KindBitmap,
}

func KindOf(codec string) Kind {
	return codecKinds[codec]
}

func Convertible(codec string) bool {
	return KindOf(codec) == KindText
}
