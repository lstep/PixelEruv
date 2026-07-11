#!/bin/sh
# Refresh Cloudflare IP ranges in an nginx config.
#
# Downloads the current Cloudflare IPv4 and IPv6 lists and replaces the
# set_real_ip_from block between the BEGIN/END CLOUDFLARE_IPS markers.
#
# Usage:
#   ./update-cloudflare-ips.sh /etc/nginx/sites-available/pixeleruv.conf
#
# After running, validate and reload:
#   sudo nginx -t && sudo systemctl reload nginx
#
# Requires curl and awk. POSIX sh — no bashisms.

set -eu

CF_V4_URL="https://www.cloudflare.com/ips-v4/"
CF_V6_URL="https://www.cloudflare.com/ips-v6/"

if [ $# -ne 1 ]; then
    echo "Usage: $0 <nginx-config-path>" >&2
    exit 1
fi

conf="$1"

if [ ! -f "$conf" ]; then
    echo "Error: file not found: $conf" >&2
    exit 1
fi

# Download current ranges.
v4=$(curl -fsS "$CF_V4_URL") || { echo "Error: failed to download IPv4 list" >&2; exit 1; }
v6=$(curl -fsS "$CF_V6_URL") || { echo "Error: failed to download IPv6 list" >&2; exit 1; }

# Build the replacement block in a temp file: one set_real_ip_from per CIDR.
block_file=$(mktemp)
trap 'rm -f "$block_file"' EXIT

printf '%s\n' "$v4" "$v6" \
    | grep -v '^[[:space:]]*$' \
    | sed 's/[[:space:]]*$//' \
    | awk '{ printf "    set_real_ip_from %s;\n", $0 }' > "$block_file"

if [ ! -s "$block_file" ]; then
    echo "Error: no IP ranges downloaded — refusing to write empty block" >&2
    exit 1
fi

# Replace the block between markers using awk.
# Prints everything before BEGIN, the new block (from temp file), everything
# after END. Reading from a file via getline avoids BSD awk's -v newline issue.
# Patterns allow leading whitespace so indented markers work too.
awk -v block="$block_file" '
    /^[[:space:]]*# BEGIN CLOUDFLARE_IPS/ {
        print
        while ((getline line < block) > 0) print line
        close(block)
        in_block = 1
        next
    }
    /^[[:space:]]*# END CLOUDFLARE_IPS/ { in_block = 0; print; next }
    !in_block { print }
' "$conf" > "$conf.tmp" && mv "$conf.tmp" "$conf"

echo "Updated Cloudflare IP ranges in $conf"
echo "Validate and reload: sudo nginx -t && sudo systemctl reload nginx"
