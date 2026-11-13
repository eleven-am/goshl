package source

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

const ProtocolWhitelist = "file"

const RemoteProtocolWhitelist = "http,https,tcp,tls"

const FormatWhitelist = "mov,matroska,mpegts,mpeg,avi,flv,asf,ogg,mxf,nut,rm,wtv,dv,gxf,m4v,h264,hevc,mpegvideo,ivf,mp3,aac,ac3,eac3,flac,wav,w64,aiff,caf,dts,truehd,wv,ape,tta,amr"

var ErrUnsupportedSource = errors.New("unsupported media source")

type Input struct {
	Arg       string
	LocalPath string
	Remote    bool
}

var pseudoProtocols = map[string]bool{
	"async": true, "cache": true, "concat": true, "crypto": true, "data": true,
	"fd": true, "ftp": true, "gopher": true, "hls": true, "md5": true,
	"pipe": true, "rtmp": true, "rtp": true, "rtsp": true, "sftp": true,
	"srt": true, "srtp": true, "subfile": true, "tcp": true, "tls": true,
	"udp": true, "unix": true,
}

func Resolve(raw string) (Input, error) {
	if raw == "" {
		return Input{}, fmt.Errorf("%w: empty source", ErrUnsupportedSource)
	}
	scheme, rest, hasScheme := splitScheme(raw)
	if !hasScheme {
		return local(raw), nil
	}
	switch scheme {
	case "file":
		return resolveFileURL(raw, rest)
	case "http", "https":
		if strings.HasPrefix(rest, "//") {
			return resolveRemote(raw, scheme)
		}
	}
	if strings.HasPrefix(rest, "//") {
		return Input{}, fmt.Errorf("%w: scheme %q", ErrUnsupportedSource, scheme)
	}
	if pseudoProtocols[scheme] {
		return Input{}, fmt.Errorf("%w: scheme %q", ErrUnsupportedSource, scheme)
	}
	return local(raw), nil
}

func InputArgs(raw string) ([]string, error) {
	input, err := Resolve(raw)
	if err != nil {
		return nil, err
	}
	whitelist := ProtocolWhitelist
	if input.Remote {
		whitelist = RemoteProtocolWhitelist
	}
	return []string{"-format_whitelist", FormatWhitelist, "-protocol_whitelist", whitelist, "-i", input.Arg}, nil
}

func local(path string) Input {
	return Input{Arg: "file:" + path, LocalPath: path}
}

func splitScheme(raw string) (string, string, bool) {
	colon := strings.IndexByte(raw, ':')
	if colon <= 0 {
		return "", "", false
	}
	for index := 0; index < colon; index++ {
		character := raw[index]
		letter := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
		other := character >= '0' && character <= '9' || character == '+' || character == '-' || character == '.'
		if !letter && (index == 0 || !other) {
			return "", "", false
		}
	}
	return strings.ToLower(raw[:colon]), raw[colon+1:], true
}

func resolveFileURL(raw, rest string) (Input, error) {
	path := rest
	if strings.HasPrefix(rest, "//") {
		path = strings.TrimPrefix(rest, "//")
		switch {
		case strings.HasPrefix(path, "/"):
		case strings.HasPrefix(path, "localhost/"):
			path = strings.TrimPrefix(path, "localhost")
		default:
			return Input{}, fmt.Errorf("%w: file URL with remote host", ErrUnsupportedSource)
		}
	}
	if !strings.HasPrefix(path, "/") {
		return Input{}, fmt.Errorf("%w: file URL without absolute path", ErrUnsupportedSource)
	}
	decoded, err := url.PathUnescape(path)
	if err != nil {
		return Input{}, fmt.Errorf("%w: invalid file URL escape: %v", ErrUnsupportedSource, err)
	}
	return local(decoded), nil
}

func resolveRemote(raw, scheme string) (Input, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return Input{}, fmt.Errorf("%w: invalid %s URL", ErrUnsupportedSource, scheme)
	}
	return Input{Arg: scheme + raw[len(scheme):], Remote: true}, nil
}
