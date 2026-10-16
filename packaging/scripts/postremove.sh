#!/bin/sh
# gifs-server package: runs after the files are removed (deb postrm, rpm %postun). The catalogue
# in /var/lib/gifs-server and the gifs-server user are kept; a purge (deb) also deletes the
# generated API key file.
set -e

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi
if [ "$1" = purge ]; then
    rm -f /etc/gifs-server/api-keys
    rmdir /etc/gifs-server 2>/dev/null || true
fi
exit 0
