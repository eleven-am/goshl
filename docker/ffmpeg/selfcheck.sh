set -eu

EXPECTED_VERSION="${EXPECTED_FFMPEG_VERSION:-9.0.2}"
EXPECTED_NVENC_API="${EXPECTED_NVENC_API:-13.0}"
EXPECTED_NVENC_DRIVER="${EXPECTED_NVENC_DRIVER:-570.0}"
BUILD_INFO="${FFMPEG_BUILD_INFO:-/usr/local/share/ffmpeg-build/nv-codec-headers}"
ARCH="$(dpkg --print-architecture)"

ENCODERS="libx264 h264_nvenc h264_vaapi aac ac3 eac3 png mjpeg mov_text webvtt srt ass"
FILTERS="scale_cuda zscale tonemap tonemap_vaapi tonemap_opencl"
MUXERS="mp4 hls segment mpegts matroska webvtt image2 null"
DEMUXERS="mov hls mpegts matroska webvtt image2 srt"
INPUT_DEVICES="lavfi"
if [ "$ARCH" = "amd64" ]; then
    ENCODERS="$ENCODERS h264_qsv"
fi

failures=0

pass() {
    printf 'PASS  %s\n' "$1"
}

fail() {
    printf 'FAIL  %s\n' "$1"
    failures=$((failures + 1))
}

check() {
    label="$1"
    shift
    if "$@" >/dev/null 2>&1; then
        pass "$label"
    else
        fail "$label"
    fi
}

has_name() {
    printf '%s\n' "$1" | tr ',' '\n' | grep -qx "$2"
}

in_list() {
    for candidate in $1; do
        if has_name "$candidate" "$2"; then
            return 0
        fi
    done
    return 1
}

stream_field() {
    ffprobe -v error -select_streams "$2" -show_entries "stream=$3" -of default=nw=1:nk=1 "$1" | head -n 1
}

expect_field() {
    actual="$(stream_field "$2" "$3" "$4" || true)"
    if [ "$actual" = "$5" ]; then
        pass "$1 $4=$5"
    else
        fail "$1 $4 expected $5 got ${actual:-nothing}"
    fi
}

run_ffmpeg() {
    label="$1"
    shift
    if ffmpeg -hide_banner -nostdin -v error -y "$@" >"$WORK/log" 2>&1; then
        pass "$label"
    else
        fail "$label"
        sed 's/^/      /' "$WORK/log"
    fi
}

printf 'ffmpeg self-check on %s\n' "$ARCH"

version_line="$(ffmpeg -hide_banner -version | head -n 1)"
case "$version_line" in
    "ffmpeg version $EXPECTED_VERSION "*) pass "ffmpeg version $EXPECTED_VERSION" ;;
    *) fail "ffmpeg version expected $EXPECTED_VERSION got: $version_line" ;;
esac

probe_line="$(ffprobe -hide_banner -version | head -n 1)"
case "$probe_line" in
    "ffprobe version $EXPECTED_VERSION "*) pass "ffprobe version $EXPECTED_VERSION" ;;
    *) fail "ffprobe version expected $EXPECTED_VERSION got: $probe_line" ;;
esac

buildconf="$(ffmpeg -hide_banner -buildconf)"
for flag in --enable-shared --disable-static --disable-debug --enable-cuda-llvm; do
    if printf '%s\n' "$buildconf" | grep -q -- "$flag"; then
        pass "configured with $flag"
    else
        fail "configured with $flag"
    fi
done

encoders="$(ffmpeg -hide_banner -encoders | awk 'seen {print $2} /^ *------/ {seen=1}')"
for name in $ENCODERS; do
    if in_list "$encoders" "$name"; then pass "encoder $name"; else fail "encoder $name"; fi
done

filters="$(ffmpeg -hide_banner -filters | awk '$3 ~ /->/ {print $2}')"
for name in $FILTERS; do
    if in_list "$filters" "$name"; then pass "filter $name"; else fail "filter $name"; fi
done

muxers="$(ffmpeg -hide_banner -muxers | awk 'seen {print $2} /^ *---/ {seen=1}')"
for name in $MUXERS; do
    if in_list "$muxers" "$name"; then pass "muxer $name"; else fail "muxer $name"; fi
done

demuxers="$(ffmpeg -hide_banner -demuxers | awk 'seen {print $2} /^ *---/ {seen=1}')"
for name in $DEMUXERS; do
    if in_list "$demuxers" "$name"; then pass "demuxer $name"; else fail "demuxer $name"; fi
done

devices="$(ffmpeg -hide_banner -devices | awk 'seen && $1 ~ /D/ {print $2} /^ *---/ {seen=1}')"
for name in $INPUT_DEVICES; do
    if in_list "$devices" "$name"; then pass "input device $name"; else fail "input device $name"; fi
done

for binary in ffmpeg ffprobe; do
    path="$(command -v "$binary")"
    linked="$(ldd "$path" 2>&1 || true)"
    if printf '%s\n' "$linked" | grep -q 'not found'; then
        fail "ldd $binary has unresolved libraries"
        printf '%s\n' "$linked" | grep 'not found' | sed 's/^/      /'
    elif printf '%s\n' "$linked" | grep -q '/usr/local/lib/libavcodec\.so'; then
        pass "ldd $binary resolves every library"
    else
        fail "ldd $binary does not link the shared libavcodec in /usr/local/lib"
    fi
done

for library in /usr/local/lib/lib*.so.*; do
    if ldd "$library" 2>&1 | grep -q 'not found'; then
        fail "ldd $library has unresolved libraries"
    fi
done

if ls /usr/local/lib/*.a >/dev/null 2>&1; then
    fail "static archives present in /usr/local/lib"
else
    pass "no static archives in /usr/local/lib"
fi

if grep -qx "nvenc_api=$EXPECTED_NVENC_API" "$BUILD_INFO" 2>/dev/null; then
    pass "nv-codec-headers NVENC API $EXPECTED_NVENC_API recorded at build ($(grep '^ffnvcodec=' "$BUILD_INFO"))"
else
    fail "nv-codec-headers NVENC API $EXPECTED_NVENC_API not recorded in $BUILD_INFO"
fi

avcodec="$(ls /usr/local/lib/libavcodec.so.*.*.* | head -n 1)"
if grep -a -q -F "$EXPECTED_NVENC_DRIVER" "$avcodec" && ! grep -a -q -F '610.00' "$avcodec"; then
    pass "libavcodec nvenc minimum driver string is $EXPECTED_NVENC_DRIVER"
else
    fail "libavcodec nvenc minimum driver string is not $EXPECTED_NVENC_DRIVER"
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

AV_SOURCE="-f lavfi -i testsrc2=size=640x360:rate=25 -f lavfi -i sine=frequency=440:sample_rate=48000"

run_ffmpeg "encode libx264+aac to mpegts" $AV_SOURCE -t 4 \
    -c:v libx264 -preset veryfast -pix_fmt yuv420p -c:a aac -b:a 128k -f mpegts "$WORK/out.ts"
expect_field "mpegts" "$WORK/out.ts" v:0 codec_name h264
expect_field "mpegts" "$WORK/out.ts" a:0 codec_name aac

run_ffmpeg "encode libx264+aac to hls" $AV_SOURCE -t 4 \
    -c:v libx264 -preset veryfast -pix_fmt yuv420p -g 50 -c:a aac -b:a 128k \
    -f hls -hls_time 2 -hls_playlist_type vod -hls_segment_type mpegts \
    -hls_segment_filename "$WORK/seg_%03d.ts" "$WORK/index.m3u8"
check "hls playlist lists a segment" grep -q '^seg_000\.ts$' "$WORK/index.m3u8"
check "hls playlist is terminated" grep -q '^#EXT-X-ENDLIST$' "$WORK/index.m3u8"
expect_field "hls segment" "$WORK/seg_000.ts" v:0 codec_name h264
expect_field "hls segment" "$WORK/seg_000.ts" a:0 codec_name aac
expect_field "hls playlist" "$WORK/index.m3u8" v:0 codec_name h264

run_ffmpeg "tone map 10-bit PQ to SDR with zscale+tonemap" \
    -f lavfi -i "testsrc2=size=640x360:rate=24,format=yuv420p10le,setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc:range=tv" \
    -t 2 \
    -vf "zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv,format=yuv420p" \
    -c:v libx264 -preset veryfast -f mpegts "$WORK/sdr.ts"
expect_field "tone mapped" "$WORK/sdr.ts" v:0 pix_fmt yuv420p
expect_field "tone mapped" "$WORK/sdr.ts" v:0 color_transfer bt709
expect_field "tone mapped" "$WORK/sdr.ts" v:0 color_primaries bt709
expect_field "tone mapped" "$WORK/sdr.ts" v:0 color_space bt709

printf '1\n00:00:01,000 --> 00:00:02,500\nfirst cue\n\n2\n00:00:03,000 --> 00:00:04,000\nsecond cue\n' > "$WORK/in.srt"
run_ffmpeg "convert srt to webvtt" -i "$WORK/in.srt" -c:s webvtt -f webvtt "$WORK/out.vtt"
check "webvtt header" grep -q '^WEBVTT' "$WORK/out.vtt"
check "webvtt cue timing" grep -q '^00:01\.000 --> 00:02\.500$' "$WORK/out.vtt"
check "webvtt cue text" grep -q '^second cue$' "$WORK/out.vtt"

if [ "$failures" -ne 0 ]; then
    printf 'self-check FAILED with %s failure(s)\n' "$failures"
    exit 1
fi
printf 'self-check PASSED\n'
