#!/usr/bin/env bash
# Выкатка billing на площадку ⟦org:Ромашки⟧ в ДЦ-2.
set -euo pipefail

BASTION=⟦host:bastion.dc2.romashka.example⟧
NODES=(⟦ipv4:10.113.8.21⟧ ⟦ipv4:10.113.8.22⟧ ⟦ipv6:2001:db8:4a1:8::23⟧)
export ANTHROPIC_API_KEY=⟦secret:sk-ant-api03-FAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEXXX⟧

for n in "${NODES[@]}"; do
  scp -J "$BASTION" dist/billing.tar.gz "deploy@[$n]:/opt/billing/"
  ssh -J "$BASTION" "deploy@$n" 'sudo systemctl restart billing'
done

curl -fsS -X POST "https://⟦host:deploy-log.romashka.example⟧/api/events" \
  -H "Authorization: Bearer ⟦secret:FAKEdepl0yT0kenR0mashka0123456789abcdef⟧" \
  -d "{\"service\":\"billing\",\"by\":\"⟦email:ivan.petrov@romashka.example⟧\",\"at\":\"$(date -Is)\"}"

ping -c1 ⟦public:1.1.1.1⟧ >/dev/null && echo "egress ok"
git -C /opt/billing pull https://⟦public:github.com⟧/⟦org:romashka⟧/billing.git
