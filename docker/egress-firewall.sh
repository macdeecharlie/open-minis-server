#!/bin/sh
set -eu
CHAIN=DOCKER-USER
rule() {
  iptables -C "$CHAIN" "$@" 2>/dev/null || iptables -I "$CHAIN" "$@"
}
# The agent container lives on 172.19.0.0/16 and is allowed to reach the
# internet and its single-target forwarder on :8080. It must not reach the
# host, other Docker networks, the WireGuard subnet, or the host's public
# listeners (which would expose Caddy and unrelated services).
rule -s 172.19.0.0/16 -d 152.53.197.210 -m conntrack --ctstate NEW -j DROP
rule -s 172.19.0.0/16 -d 10.0.0.0/8 -m conntrack --ctstate NEW -j DROP
rule -s 172.19.0.0/16 -d 172.17.0.0/16 -m conntrack --ctstate NEW -j DROP
rule -s 172.19.0.0/16 -d 172.18.0.0/16 -m conntrack --ctstate NEW -j DROP
rule -s 172.19.0.0/16 -d 172.20.0.0/16 -m conntrack --ctstate NEW -j DROP
# The forwarder is the only same-network destination on :8080. The Docker
# gateway's :8080 is Traefik on the host and must stay unreachable.
rule -s 172.19.0.0/16 -d 172.19.0.1 -p tcp --dport 8080 -m conntrack --ctstate NEW -j DROP
rule -s 172.19.0.0/16 -d 172.19.0.0/16 -p tcp ! --dport 8080 -m conntrack --ctstate NEW -j DROP
