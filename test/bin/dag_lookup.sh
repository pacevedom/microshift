#!/bin/bash
#
# Wrapper script for dag_lookup.py
# Displays image dependency relationships in the DAG
#
# Usage:
#   ./dag_lookup.sh rhel-9.6-microshift-source    # Show info for specific image
#   ./dag_lookup.sh --all                         # Show full DAG
#   ./dag_lookup.sh --layer layer1-base           # Show only layer1-base images
#   ./dag_lookup.sh --ancestors <name>            # Show all ancestors
#   ./dag_lookup.sh --descendants <name>          # Show all descendants
#   ./dag_lookup.sh --search source               # Search for images
#   ./dag_lookup.sh --roots                       # Show all root nodes
#   ./dag_lookup.sh --leaves                      # Show all leaf nodes
#   ./dag_lookup.sh --tree <name>                 # Show tree from image

SCRIPTDIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec python3 "${SCRIPTDIR}/pyutils/dag_lookup.py" "$@"
