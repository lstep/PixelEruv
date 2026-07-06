#!/bin/sh
# Generate a self-signed TLS cert (with SANs from $TLS_HOSTS) for nginx, then
# exec nginx. Required so browsers expose crypto.subtle (PKCE) when the app is
# accessed remotely over HTTPS instead of localhost.
#
# TLS_HOSTS is a comma-separated list of DNS names and/or IPs the cert should
# cover. Defaults to localhost,127.0.0.1. For LAN access set it to the host's
# LAN IP, e.g. TLS_HOSTS=localhost,127.0.0.1,192.168.1.10
set -e

TLS_HOSTS="${TLS_HOSTS:-localhost,127.0.0.1}"
CERT_DIR="/etc/nginx/certs"
mkdir -p "$CERT_DIR"

# Build subjectAltName entries: IPs -> IP.N, everything else -> DNS.N.
i_dns=1
i_ip=1
san=""
old_ifs="$IFS"
IFS=','
for h in $TLS_HOSTS; do
  h=$(echo "$h" | tr -d ' ')
  [ -z "$h" ] && continue
  case "$h" in
    *.*.*.*)
      san="${san}IP.$i_ip=$h
"
      i_ip=$((i_ip + 1))
      ;;
    *)
      san="${san}DNS.$i_dns=$h
"
      i_dns=$((i_dns + 1))
      ;;
  esac
done
IFS="$old_ifs"

cat > "$CERT_DIR/openssl.cnf" <<EOF
[req]
distinguished_name = req_distinguished_name
x509_extensions = v3_req
prompt = no
[req_distinguished_name]
CN = pixeleruv-dev
[v3_req]
subjectAltName = @alt_names
[alt_names]
${san}
EOF

openssl req -x509 -nodes -newkey rsa:2048 -days 365 \
  -keyout "$CERT_DIR/key.pem" \
  -out "$CERT_DIR/cert.pem" \
  -config "$CERT_DIR/openssl.cnf" >/dev/null 2>&1

exec nginx -g 'daemon off;'
