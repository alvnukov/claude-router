#!/usr/bin/env bash
# Выкатка billing на площадку Ромашки в ДЦ-2.
set -euo pipefail

BASTION=bastion.dc2.romashka.example
NODES=(10.113.8.21 10.113.8.22 2001:db8:4a1:8::23)
export ANTHROPIC_API_KEY=sk-ant-api03-FAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEXXX

for n in "${NODES[@]}"; do
  scp -J "$BASTION" dist/billing.tar.gz "deploy@[$n]:/opt/billing/"
  ssh -J "$BASTION" "deploy@$n" 'sudo systemctl restart billing'
done

curl -fsS -X POST "https://deploy-log.romashka.example/api/events" \
  -H "Authorization: Bearer FAKEdepl0yT0kenR0mashka0123456789abcdef" \
  -d "{\"service\":\"billing\",\"by\":\"ivan.petrov@romashka.example\",\"at\":\"$(date -Is)\"}"

ping -c1 1.1.1.1 >/dev/null && echo "egress ok"
git -C /opt/billing pull https://github.com/romashka/billing.git
