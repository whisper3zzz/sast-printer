# syntax=docker/dockerfile:1

FROM ubuntu:24.04

ARG DEBIAN_FRONTEND=noninteractive
ARG TZ=Asia/Shanghai
ARG WPS_PKGVER=12.1.2.25838
ARG WPS_BUILD=648473
ARG WPS_SECURITY_KEY=7f8faaaa468174dc1c9cd62e5f218a5b
ARG WPS_DEB_URL=

ENV TZ=${TZ}
ENV DISPLAY=:99
ENV LD_LIBRARY_PATH=/usr/lib/x86_64-linux-gnu:/lib/x86_64-linux-gnu:/opt/kingsoft/wps-office/office6:${LD_LIBRARY_PATH}
ENV PYTHONUNBUFFERED=1

WORKDIR /app

RUN apt update && \
    apt install -y ca-certificates && \
    sed -i \
    -e 's|http://archive.ubuntu.com/ubuntu|https://mirrors.ustc.edu.cn/ubuntu|g' \
    -e 's|http://security.ubuntu.com/ubuntu|https://mirrors.ustc.edu.cn/ubuntu|g' \
    /etc/apt/sources.list.d/ubuntu.sources && \
    apt-get update && \
    apt-get install -y --no-install-recommends \
    wget \
    python3 \
    python3-pip \
    python3-setuptools \
    python3-wheel \
    xvfb \
    xserver-xorg-core \
    xserver-xorg-video-dummy \
    x11-xserver-utils \
    dbus \
    dbus-x11 \
    fontconfig \
    libx11-6 \
    libxext6 \
    libxrender1 \
    libx11-xcb1 \
    libxcb1 \
    libxcomposite1 \
    libxcursor1 \
    libxdamage1 \
    libxfixes3 \
    libxi6 \
    libxrandr2 \
    libxss1 \
    libxtst6 \
    libfreetype6 \
    libfontconfig1 \
    libglib2.0-0 \
    libdbus-1-3 \
    libxslt1.1 \
    libxkbcommon0 \
    libxkbcommon-x11-0 \
    libgl1 \
    libnss3 \
    libnspr4 \
    libgbm1 \
    libasound2t64 \
    libpulse0 \
    libcups2 \
    libglu1-mesa \
    libsdl2-2.0-0 \
    libsm6 \
    libice6 \
    libsqlite3-0 \
    libtool \
    libpango-1.0-0 \
    libqt5core5a \
    libqt5gui5 \
    libqt5xml5 \
    libqt5network5 \
    libqt5printsupport5 \
    libqt5widgets5 \
    xdg-utils \
    hicolor-icon-theme \
    xfonts-utils \
    bsdmainutils \
    dialog \
    binutils \
    desktop-file-utils \
    shared-mime-info \
    libjpeg-turbo8 \
    && strip --remove-section=.note.ABI-tag /usr/lib/x86_64-linux-gnu/libQt5Core.so.5 || true \
    && fc-cache -fv \
    && rm -rf /var/lib/apt/lists/*

RUN set -eux; \
    if [ -n "${WPS_DEB_URL}" ]; then \
    wget -q "${WPS_DEB_URL}" -O /tmp/wps.deb; \
    else \
    BASE_URL="https://wps-linux-personal.wpscdn.cn/wps/download/ep/Linux2023/${WPS_PKGVER##*.}/wps-office_${WPS_PKGVER}.AK.preread.sw_${WPS_BUILD}_amd64.deb"; \
    URI="${BASE_URL#https://wps-linux-personal.wpscdn.cn}"; \
    TS="$(date +%s)"; \
    SIGN="$(printf '%s' "${WPS_SECURITY_KEY}${URI}${TS}" | md5sum | awk '{print $1}')"; \
    wget -q "${BASE_URL}?t=${TS}&k=${SIGN}" -O /tmp/wps.deb; \
    fi; \
    apt-get update; \
    apt-get install -y --no-install-recommends /tmp/wps.deb; \
    rm -f /tmp/wps.deb; \
    rm -rf /var/lib/apt/lists/*

RUN chmod -x /opt/kingsoft/wps-office/office6/wpscloudsvr 2>/dev/null || true && \
    rm -f /opt/kingsoft/wps-office/office6/libstdc++.so* \
    /opt/kingsoft/wps-office/office6/libjpeg.so* \
    /opt/kingsoft/wps-office/office6/libdbus-1.so* \
    && true

# Validate critical shared libs are available at build time.
COPY office_converter/requirements.txt ./requirements.txt
COPY office_converter/server.py ./server.py
COPY office_converter/office_converter_pb2.py ./office_converter_pb2.py
COPY office_converter/office_converter_pb2_grpc.py ./office_converter_pb2_grpc.py
COPY office_converter/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
COPY office_converter/dummy.conf /etc/X11/dummy.conf

RUN mkdir -p /root/.config/Kingsoft /usr/local/share/fonts/custom && \
    python3 -c "import ctypes; ctypes.CDLL('libQt5Core.so.5'); ctypes.CDLL('libQt5Gui.so.5'); print('qt runtime ok')" && \
    pip3 install --no-cache-dir -r requirements.txt --break-system-packages && \
    fc-cache -fv && \
    chmod +x /usr/local/bin/docker-entrypoint.sh

EXPOSE 50061

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["python3", "server.py", "--listen", "0.0.0.0:50061", "--output-dir", "/tmp/office-output", "--max-workers", "1", "--accepted-formats", "doc,docx,ppt,pptx"]
