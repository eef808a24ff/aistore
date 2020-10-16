#!/bin/bash

set -e

# TODO: this is a workaround to work with VPNs
if command -v /opt/cisco/anyconnect/bin/vpn &> /dev/null; then
  echo "Do you wish to disable VPN (y/n) ?"
  read -r disable_vpn
  if [[ "$disable_vpn" == "y" ]]; then
    /opt/cisco/anyconnect/bin/vpn disconnect || true
  fi
fi

source utils/deploy_minikube.sh
source utils/minikube_registry.sh

source utils/deploy_ais.sh
