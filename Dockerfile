# Build: docker buildx build --builder fleet --platform linux/amd64,linux/arm64 --push -t ghcr.io/davefinster/chromemcp .
# Run:   identities (the logged-in profiles) live on a MOUNTED volume at
#        /data/identities; sessions are ephemeral under /tmp.
#          docker run --rm -p 127.0.0.1:8787:8787 -v chromemcp:/data --shm-size=1g \
#            ghcr.io/davefinster/chromemcp serve -http :8787

# ---- the Go server ---------------------------------------------------------
FROM golang:1.27-alpine AS build
ENV CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The suite's Chrome-driven tests skip themselves where there is no Chrome
# (here); the rest — the OAuth stack, the identity store, the snapshot
# rendering, the flag and key parsing — is the build gate.
RUN go vet ./... && go test ./...
ARG VERSION=dev
RUN go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/chromemcp .

# ---- runtime ---------------------------------------------------------------
# Debian: Google Chrome from Google's own apt repository (which serves amd64
# and arm64 alike), TigerVNC's Xvnc for the headful display, noVNC for the
# live view, Node for chrome-devtools-mcp, and the fonts the device profiles
# present under Windows names (fonts.go): Liberation for Arial / Times New
# Roman / Courier New, Carlito and Caladea for Calibri and Cambria, DejaVu
# for Verdana and Tahoma, Noto CJK for the East Asian families, and Selawik
# — Microsoft's own OFL-licensed metric-compatible stand-in for Segoe UI,
# which no distribution packages — from its release on GitHub. fontconfig's
# fc-list is how the server learns which of them are there.
FROM debian:trixie-slim
ARG DEVTOOLS_MCP_VERSION=1.8.0
ARG SELAWIK_URL=https://github.com/microsoft/Selawik/releases/download/1.01/Selawik_Release.zip
ARG SELAWIK_SHA256=3f62c51e05e3b5a1e6241cf92a371f0be2ea1183aa87b30718bbd40832a8d423
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl gnupg unzip \
 && curl -fsSL https://dl.google.com/linux/linux_signing_key.pub | gpg --dearmor -o /usr/share/keyrings/google-chrome.gpg \
 && printf 'Types: deb\nURIs: https://dl.google.com/linux/chrome/deb/\nSuites: stable\nComponents: main\nArchitectures: amd64 arm64\nSigned-By: /usr/share/keyrings/google-chrome.gpg\n' > /etc/apt/sources.list.d/google-chrome.sources \
 && apt-get update \
 && apt-get install -y --no-install-recommends \
      google-chrome-stable \
      tigervnc-standalone-server novnc \
      nodejs npm \
      fontconfig fonts-liberation fonts-liberation-sans-narrow fonts-dejavu-core fonts-crosextra-carlito fonts-crosextra-caladea \
      fonts-noto-core fonts-noto-color-emoji fonts-noto-cjk \
      tzdata procps \
 && curl -fsSL -o /tmp/selawik.zip "${SELAWIK_URL}" \
 && echo "${SELAWIK_SHA256}  /tmp/selawik.zip" | sha256sum -c - \
 && install -d /usr/local/share/fonts/selawik \
 && unzip -j -q /tmp/selawik.zip '*.ttf' -d /usr/local/share/fonts/selawik \
 && rm /tmp/selawik.zip \
 && fc-cache -f \
 && npm install -g --omit=dev "chrome-devtools-mcp@${DEVTOOLS_MCP_VERSION}" \
 && npm cache clean --force \
 && apt-get purge -y npm gnupg unzip && apt-get autoremove -y \
 && rm -rf /var/lib/apt/lists/* /root/.npm \
 && useradd --uid 1000 --user-group --home-dir /data --no-create-home --shell /usr/sbin/nologin chrome \
 && install -d -o chrome -g chrome -m 0755 /data /data/identities
COPY --from=build /out/chromemcp /usr/local/bin/chromemcp

# Chrome's sandbox needs user namespaces the container runtime usually does
# not grant; --no-sandbox is the norm for containerised Chrome. Drop the
# variable (CHROMEMCP_NO_SANDBOX=0) on a runtime that allows them.
ENV CHROMEMCP_NO_SANDBOX=1 \
    CHROMEMCP_IDENTITIES_DIR=/data/identities \
    CHROMEMCP_SESSIONS_DIR=/tmp/chromemcp-sessions \
    CHROMEMCP_NOVNC=/usr/share/novnc \
    CHROMEMCP_DEVTOOLS_MCP=/usr/local/bin/chrome-devtools-mcp \
    HOME=/data
VOLUME /data
EXPOSE 8787
USER chrome
ENTRYPOINT ["/usr/local/bin/chromemcp"]
CMD ["serve", "-http", ":8787"]
