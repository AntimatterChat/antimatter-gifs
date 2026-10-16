#!/bin/sh
# gifs-server package: runs after the files are installed or upgraded (deb postinst, rpm %post).
# Creates the gifs-server system user and, at the first install, an API key; enables the service
# without starting it.
set -e

# deb: "configure" [<previous version>]; rpm: 1 (install) or 2 (upgrade).
first=
case "$1" in
    configure) [ -n "$2" ] || first=1 ;;
    1) first=1 ;;
    2) ;;
    *) exit 0 ;;
esac

user=gifs-server
nologin=/usr/sbin/nologin
[ -x "$nologin" ] || nologin=/sbin/nologin
getent group "$user" >/dev/null 2>&1 || groupadd --system "$user"
getent passwd "$user" >/dev/null 2>&1 ||
    useradd --system --gid "$user" --home-dir /var/lib/gifs-server --no-create-home \
        --shell "$nologin" --comment "gifs-server" "$user"

install -d -o "$user" -g "$user" -m 0750 /var/lib/gifs-server

# The service reads its settings and keys as gifs-server: root owns them, the group reads them.
chgrp "$user" /etc/gifs-server/gifs-server.env && chmod 0640 /etc/gifs-server/gifs-server.env
keys=/etc/gifs-server/api-keys
if [ ! -e "$keys" ]; then
    umask 027
    {
        echo "# gifs-server API keys, one per line (AM_GIFS_API_KEYS_FILE). Generated at install."
        od -An -N32 -tx1 /dev/urandom | tr -d ' \n'
        echo
    } > "$keys"
    chgrp "$user" "$keys"
    newkey=1
fi

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload >/dev/null 2>&1 || true
    if [ -n "$first" ]; then
        systemctl enable gifs-server.service >/dev/null 2>&1 || true
    fi
fi

if [ -n "$first" ]; then
    cat <<MSG

gifs-server is installed and enabled, but not started. Next steps:
  1. Review /etc/gifs-server/gifs-server.env (it listens on 127.0.0.1:8080).
  2. Import the bundled sticker pack:  sudo systemctl start gifs-server-stickers
  3. Start the service:                sudo systemctl start gifs-server
  4. In Antimatter, System Console > Plugins > GIFs: set the GIF service URL
     (http://127.0.0.1:8080/v2) and the API key from $keys.
Logs: journalctl -u gifs-server
MSG
    [ -z "$newkey" ] || echo "A random API key was written to $keys."
else
    echo "gifs-server was upgraded; restart it to use the new version: sudo systemctl restart gifs-server"
fi
exit 0
