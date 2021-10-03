#!/bin/bash
set -o errexit
set -o nounset

# Will not run without:
# - DEPLOY_TO

make

mkdir -p "$DEPLOY_TO"
sha1sum cadvisor > "${DEPLOY_TO}/cadvisor.sha1sum"
mv cadvisor "$DEPLOY_TO"
