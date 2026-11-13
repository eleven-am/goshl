FROM ubuntu:24.04 AS build

ARG TARGETARCH
ARG FFMPEG_VERSION=9.0.2
ARG FFMPEG_SHA256=8c3850283eb25fa026482078a04051e0be17347b09ef81a0849bec15a96e002e
ARG NV_CODEC_HEADERS_VERSION=13.0.19.0
ARG NV_CODEC_HEADERS_SHA256=13da39edb3a40ed9713ae390ca89faa2f1202c9dda869ef306a8d4383e242bee
ARG NVENC_API=13.0

RUN set -eux; \
    export DEBIAN_FRONTEND=noninteractive; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        clang \
        libass-dev \
        libdrm-dev \
        libva-dev \
        libx264-dev \
        libzimg-dev \
        nasm \
        ocl-icd-opencl-dev \
        opencl-headers \
        pkgconf \
        wget \
        xz-utils \
        zlib1g-dev; \
    if [ "$TARGETARCH" = "amd64" ]; then \
        apt-get install -y --no-install-recommends libvpl-dev; \
    fi; \
    rm -rf /var/lib/apt/lists/*

WORKDIR /src

RUN set -eux; \
    wget -q -O nv-codec-headers.tar.gz "https://github.com/FFmpeg/nv-codec-headers/releases/download/n${NV_CODEC_HEADERS_VERSION}/nv-codec-headers-${NV_CODEC_HEADERS_VERSION}.tar.gz"; \
    echo "${NV_CODEC_HEADERS_SHA256}  nv-codec-headers.tar.gz" | sha256sum -c -; \
    mkdir nv-codec-headers; \
    tar -xzf nv-codec-headers.tar.gz -C nv-codec-headers --strip-components=1; \
    make -C nv-codec-headers install PREFIX=/usr/local; \
    test "$(pkg-config --modversion ffnvcodec)" = "${NV_CODEC_HEADERS_VERSION}"; \
    api="$(printf '#include <ffnvcodec/nvEncodeAPI.h>\nNVENCAPI_MAJOR_VERSION.NVENCAPI_MINOR_VERSION\n' | cc -E -P -I/usr/local/include - | tail -n 1 | tr -d ' ')"; \
    test "$api" = "${NVENC_API}"; \
    mkdir -p /out/share/ffmpeg-build; \
    printf 'ffnvcodec=%s\nnvenc_api=%s\n' "$(pkg-config --modversion ffnvcodec)" "$api" > /out/share/ffmpeg-build/nv-codec-headers

RUN set -eux; \
    wget -q -O ffmpeg.tar.xz "https://ffmpeg.org/releases/ffmpeg-${FFMPEG_VERSION}.tar.xz"; \
    echo "${FFMPEG_SHA256}  ffmpeg.tar.xz" | sha256sum -c -; \
    mkdir ffmpeg; \
    tar -xJf ffmpeg.tar.xz -C ffmpeg --strip-components=1; \
    cd ffmpeg; \
    arch_flags=""; \
    if [ "$TARGETARCH" = "amd64" ]; then arch_flags="--enable-libvpl"; fi; \
    ./configure \
        --prefix=/usr/local \
        --disable-debug \
        --disable-doc \
        --disable-static \
        --enable-shared \
        --disable-autodetect \
        --disable-ffplay \
        --enable-gpl \
        --enable-libx264 \
        --enable-libzimg \
        --enable-libass \
        --enable-zlib \
        --enable-libdrm \
        --enable-vaapi \
        --enable-opencl \
        --enable-ffnvcodec \
        --enable-nvenc \
        --enable-nvdec \
        --enable-cuvid \
        --enable-cuda-llvm \
        $arch_flags; \
    make -j"$(nproc)"; \
    make install; \
    mkdir -p /out/bin /out/lib; \
    cp -a /usr/local/bin/ffmpeg /usr/local/bin/ffprobe /out/bin/; \
    cp -a /usr/local/lib/lib*.so* /out/lib/; \
    cp ffbuild/config.log /out/share/ffmpeg-build/config.log

FROM ubuntu:24.04 AS runtime

ARG TARGETARCH

ENV NVIDIA_DRIVER_CAPABILITIES=compute,utility,video

RUN set -eux; \
    export DEBIAN_FRONTEND=noninteractive; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates \
        gosu \
        libass9 \
        libdrm2 \
        libva-drm2 \
        libva2 \
        libx264-164 \
        libzimg2 \
        ocl-icd-libopencl1 \
        passwd \
        tzdata \
        wget \
        zlib1g; \
    if [ "$TARGETARCH" = "amd64" ]; then \
        apt-get install -y --no-install-recommends libvpl2; \
    fi; \
    rm -rf /var/lib/apt/lists/*

COPY --from=build /out/ /usr/local/
COPY selfcheck.sh /usr/local/share/ffmpeg-build/selfcheck.sh

RUN ldconfig && sh /usr/local/share/ffmpeg-build/selfcheck.sh
