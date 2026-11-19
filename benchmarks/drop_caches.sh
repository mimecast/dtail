#!/usr/bin/env bash
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
   echo "This script must be run as root (use sudo)." 
   exit 1
fi

echo "Syncing dirty pages to disk..."
sync

echo "Dropping all caches (page cache, dentries, and inodes)..."
echo 3 > /proc/sys/vm/drop_caches

echo "Caches cleared successfully."
